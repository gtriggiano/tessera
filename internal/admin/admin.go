// Package admin serves the admin listener: liveness at /livez, readiness at
// /readyz and the metrics at /metrics, in cleartext HTTP/1.1. The paths, the
// methods and the responses are a public interface.
//
// Probes and scrapes read memory only. The server has its own fixed limits,
// so that it stays bounded and keeps answering whatever the load on the gRPC
// listener.
package admin

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// The reasons that /readyz reports.
const (
	ReasonStarting     = "starting"
	ReasonShuttingDown = "shutting down"
)

// limits are the fixed ceilings of the admin server.
type limits struct {
	readHeader, read, write, idle time.Duration
	headerBytes, connections      int
}

var defaultLimits = limits{
	readHeader: 5 * time.Second, read: 10 * time.Second, write: 10 * time.Second, idle: 10 * time.Second,
	headerBytes: 8 << 10, connections: 16,
}

// Server is the admin server. It starts not ready, with the reason
// ReasonStarting.
type Server struct {
	http        *http.Server
	connections int

	mu     sync.Mutex
	reason string // empty when ready
	// fresh holds the connections that have not sent a request yet, which
	// net/http's Shutdown would wait for.
	fresh map[net.Conn]struct{}
}

// NewServer returns an admin server that serves metrics at /metrics, without
// opening a socket.
func NewServer(metrics http.Handler) *Server {
	return newServer(metrics, defaultLimits)
}

func newServer(metrics http.Handler, l limits) *Server {
	s := &Server{connections: l.connections, reason: ReasonStarting, fresh: map[net.Conn]struct{}{}}
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	s.http = &http.Server{
		Handler:           s.handler(metrics),
		Protocols:         protocols,
		ReadHeaderTimeout: l.readHeader,
		ReadTimeout:       l.read,
		WriteTimeout:      l.write,
		IdleTimeout:       l.idle,
		MaxHeaderBytes:    l.headerBytes,
		ConnState:         s.track,
		// net/http writes its own errors to standard error otherwise, and they
		// can carry client addresses and values that clients sent.
		ErrorLog: log.New(io.Discard, "", 0),
	}
	s.http.RegisterOnShutdown(s.closeFresh)
	return s
}

// SetReady makes /readyz answer 200.
func (s *Server) SetReady() { s.setReason("") }

// SetNotReady makes /readyz answer 503 with reason.
func (s *Server) SetNotReady(reason string) { s.setReason(reason) }

func (s *Server) setReason(reason string) {
	s.mu.Lock()
	s.reason = reason
	s.mu.Unlock()
}

func (s *Server) notReady() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reason
}

// Serve answers the connections of l, at most a fixed number at a time,
// until Shutdown or Close, and closes l. It returns nil after Shutdown or
// Close, and an error when l fails.
func (s *Server) Serve(l net.Listener) error {
	if err := s.http.Serve(newLimitListener(l, s.connections)); !errors.Is(err, http.ErrServerClosed) {
		_ = s.http.Close()
		return err
	}
	return nil
}

// closeFresh closes the connections that have not sent a request yet. net/http
// runs it once Shutdown has closed the listener.
func (s *Server) closeFresh() {
	s.mu.Lock()
	fresh := make([]net.Conn, 0, len(s.fresh))
	for c := range s.fresh {
		fresh = append(fresh, c)
	}
	s.mu.Unlock()
	for _, c := range fresh {
		_ = c.Close()
	}
}

// track records whether c has sent a request yet.
func (s *Server) track(c net.Conn, state http.ConnState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if state == http.StateNew {
		s.fresh[c] = struct{}{}
	} else {
		delete(s.fresh, c)
	}
}

// Shutdown closes the listener, the idle connections and those that have not
// sent a request yet, and waits for the requests in progress until ctx ends;
// it then closes the remaining connections and returns the context error.
func (s *Server) Shutdown(ctx context.Context) error {
	err := s.http.Shutdown(ctx)
	if err != nil {
		_ = s.http.Close()
	}
	return err
}

// Close closes the listener and every connection at once.
func (s *Server) Close() error { return s.http.Close() }

// handler routes the exact paths as sent, so that encoded spellings are not
// aliases. Every response has Cache-Control: no-store.
func (s *Server) handler(metrics http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		path := req.URL.EscapedPath()
		if path != "/livez" && path != "/readyz" && path != "/metrics" {
			text(w, http.StatusNotFound, "not found\n")
			return
		}
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			text(w, http.StatusMethodNotAllowed, "method not allowed\n")
			return
		}
		switch path {
		case "/livez":
			text(w, http.StatusOK, "ok\n")
		case "/readyz":
			if reason := s.notReady(); reason != "" {
				text(w, http.StatusServiceUnavailable, "not ready: "+reason+"\n")
			} else {
				text(w, http.StatusOK, "ready\n")
			}
		default:
			metrics.ServeHTTP(w, req)
		}
	})
}

// text writes a short plain-text response; net/http drops the body of a
// response to HEAD.
func text(w http.ResponseWriter, status int, body string) {
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// limitListener takes one of cap(slots) slots before each Accept, so that
// excess clients wait in the operating system's backlog rather than in a
// queue of tessera's. A connection gives its slot back once, when it closes.
type limitListener struct {
	net.Listener
	slots  chan struct{}
	closed chan struct{}
	once   sync.Once
}

func newLimitListener(inner net.Listener, n int) *limitListener {
	return &limitListener{Listener: inner, slots: make(chan struct{}, n), closed: make(chan struct{})}
}

func (l *limitListener) Accept() (net.Conn, error) {
	select {
	case l.slots <- struct{}{}:
	case <-l.closed:
		return nil, net.ErrClosed
	}
	c, err := l.Listener.Accept()
	if err != nil {
		<-l.slots
		return nil, err
	}
	return &limitConn{Conn: c, release: func() { <-l.slots }}, nil
}

func (l *limitListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return l.Listener.Close()
}

type limitConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *limitConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}
