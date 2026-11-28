package admin

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// observingListener reports each accepted connection.
type observingListener struct {
	net.Listener
	accepted chan struct{}
}

func (l *observingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		select {
		case l.accepted <- struct{}{}:
		default:
		}
	}
	return c, err
}

// gatedListener hands over its first connection at once, and holds each later
// one until open; held receives once it holds one.
type gatedListener struct {
	net.Listener
	gate, held chan struct{}
	once       sync.Once
	accepted   int
}

// open lets the held connections through. It can be called more than once.
func (l *gatedListener) open() { l.once.Do(func() { close(l.gate) }) }

func (l *gatedListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	if l.accepted++; l.accepted > 1 {
		select {
		case l.held <- struct{}{}:
		default:
		}
		<-l.gate
	}
	return c, nil
}

type fixture struct {
	server   *Server
	listener *observingListener
	url      string
	client   *http.Client
}

var metricsText = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
	_, _ = io.WriteString(w, "tessera_build_info 1\n")
})

// start serves s on a loopback port until the test ends.
func start(t *testing.T, s *Server) *fixture {
	t.Helper()
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{
		server:   s,
		listener: &observingListener{Listener: inner, accepted: make(chan struct{}, 16)},
		url:      "http://" + inner.Addr().String(),
		client:   &http.Client{Timeout: 3 * time.Second},
	}
	done := make(chan error, 1)
	go func() { done <- s.Serve(f.listener) }()
	t.Cleanup(func() {
		f.client.CloseIdleConnections()
		_ = s.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("Serve did not stop")
		}
	})
	return f
}

func (f *fixture) do(t *testing.T, method, path string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, f.url+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("%s %s: Cache-Control %q", method, path, resp.Header.Get("Cache-Control"))
	}
	return resp, string(body)
}

func (f *fixture) want(t *testing.T, method, path string, status int, body string) {
	t.Helper()
	if resp, got := f.do(t, method, path); resp.StatusCode != status || got != body {
		t.Fatalf("%s %s = %d %q; want %d %q", method, path, resp.StatusCode, got, status, body)
	}
}

func dial(t *testing.T, address string) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", address, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func get(t *testing.T, c net.Conn, path string) *http.Response {
	t.Helper()
	if _, err := io.WriteString(c, "GET "+path+" HTTP/1.1\r\nHost: localhost\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp
}

func TestRoutes(t *testing.T) {
	s := NewServer(metricsText)
	s.SetReady()
	f := start(t, s)
	for _, c := range []struct {
		method, path string
		status       int
		body         string
	}{
		{"GET", "/livez", 200, "ok\n"},
		{"HEAD", "/livez", 200, ""},
		{"GET", "/readyz", 200, "ready\n"},
		{"HEAD", "/readyz", 200, ""},
		{"GET", "/readyz?verbose", 200, "ready\n"},
		{"GET", "/metrics", 200, "tessera_build_info 1\n"},
		{"HEAD", "/metrics", 200, ""},
		{"GET", "/", 404, "not found\n"},
		{"GET", "/livez/", 404, "not found\n"},
		{"GET", "/readyz/extra", 404, "not found\n"},
		{"GET", "/LIVEZ", 404, "not found\n"},
		{"GET", "/live%7A", 404, "not found\n"},
		{"GET", "//livez", 404, "not found\n"},
		{"GET", "/statusz", 404, "not found\n"},
		{"GET", "/debug/pprof/", 404, "not found\n"},
		{"POST", "/nowhere", 404, "not found\n"},
		{"HEAD", "/nowhere", 404, ""},
	} {
		resp, body := f.do(t, c.method, c.path)
		if resp.StatusCode != c.status || body != c.body {
			t.Errorf("%s %s = %d %q; want %d %q", c.method, c.path, resp.StatusCode, body, c.status, c.body)
		}
	}
	for _, path := range []string{"/livez", "/readyz", "/metrics"} {
		for _, method := range []string{"POST", "PUT", "DELETE", "OPTIONS"} {
			resp, body := f.do(t, method, path)
			if resp.StatusCode != 405 || resp.Header.Get("Allow") != "GET, HEAD" || body != "method not allowed\n" {
				t.Errorf("%s %s = %d %q, Allow %q", method, path, resp.StatusCode, body, resp.Header.Get("Allow"))
			}
		}
	}
	resp, _ := f.do(t, "HEAD", "/readyz")
	if resp.ContentLength != int64(len("ready\n")) || resp.Header.Get("Content-Type") != "text/plain; charset=utf-8" || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("HEAD /readyz: length %d, headers %v", resp.ContentLength, resp.Header)
	}
}

func TestReadiness(t *testing.T) {
	s := NewServer(metricsText)
	f := start(t, s)
	expect := func(status int, body string) {
		t.Helper()
		f.want(t, "GET", "/readyz", status, body)
		f.want(t, "HEAD", "/readyz", status, "")
		f.want(t, "GET", "/livez", 200, "ok\n")
		f.want(t, "GET", "/metrics", 200, "tessera_build_info 1\n")
	}
	expect(503, "not ready: starting\n")
	s.SetReady()
	expect(200, "ready\n")
	s.SetNotReady(ReasonShuttingDown)
	expect(503, "not ready: shutting down\n")
}

func TestConnectionLimit(t *testing.T) {
	l := defaultLimits
	l.connections = 1
	f := start(t, newServer(metricsText, l))
	address := f.listener.Addr().String()
	first := dial(t, address)
	if resp := get(t, first, "/livez"); resp.StatusCode != 200 {
		t.Fatalf("first connection: status %d", resp.StatusCode)
	}
	<-f.listener.accepted
	second := dial(t, address)
	if _, err := io.WriteString(second, "GET /livez HTTP/1.1\r\nHost: localhost\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	// The first connection holds the only slot while idle, so the second
	// waits in the backlog until it closes.
	select {
	case <-f.listener.accepted:
		t.Fatal("accepted a connection beyond the limit")
	case <-time.After(150 * time.Millisecond):
	}
	_ = first.Close()
	resp, err := http.ReadResponse(bufio.NewReader(second), &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("second connection: status %d", resp.StatusCode)
	}
}

func TestTimeoutsAndHeaderSize(t *testing.T) {
	short := defaultLimits
	short.readHeader, short.idle, short.write = 100*time.Millisecond, 100*time.Millisecond, 100*time.Millisecond
	slow := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = io.WriteString(w, "late\n")
	})
	f := start(t, newServer(slow, short))
	address := f.listener.Addr().String()
	t.Run("header", func(t *testing.T) {
		c := dial(t, address)
		_, _ = io.WriteString(c, "GET /livez HTTP/1.1\r\nHost:")
		var b [1]byte
		if _, err := c.Read(b[:]); err == nil {
			t.Fatal("a connection with incomplete headers was not closed")
		} else if e, ok := err.(net.Error); ok && e.Timeout() {
			t.Fatal("the client deadline expired before the header deadline")
		}
	})
	t.Run("header size", func(t *testing.T) {
		c := dial(t, address)
		_, _ = io.WriteString(c, "GET /livez HTTP/1.1\r\nHost: localhost\r\nX-Large: "+strings.Repeat("a", 16<<10)+"\r\n\r\n")
		resp, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: http.MethodGet})
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
			t.Fatalf("oversized headers: status %d", resp.StatusCode)
		}
	})
	t.Run("idle", func(t *testing.T) {
		c := dial(t, address)
		get(t, c, "/livez")
		var b [1]byte
		if _, err := c.Read(b[:]); !errors.Is(err, io.EOF) {
			t.Fatalf("an idle connection was not closed: %v", err)
		}
	})
	t.Run("write", func(t *testing.T) {
		c := dial(t, address)
		_, _ = io.WriteString(c, "GET /metrics HTTP/1.1\r\nHost: localhost\r\n\r\n")
		if _, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: http.MethodGet}); err == nil {
			t.Fatal("a response outlived the write deadline")
		} else if e, ok := err.(net.Error); ok && e.Timeout() {
			t.Fatal("the client deadline expired before the write deadline")
		}
	})
}

func TestShutdown(t *testing.T) {
	t.Run("graceful", func(t *testing.T) {
		f := start(t, NewServer(metricsText))
		f.want(t, "GET", "/livez", 200, "ok\n") // leaves an idle connection
		begin := time.Now()
		if err := f.server.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
		if elapsed := time.Since(begin); elapsed > time.Second {
			t.Fatalf("Shutdown took %v", elapsed)
		}
		if c, err := net.DialTimeout("tcp", f.listener.Addr().String(), time.Second); err == nil {
			_ = c.Close()
			t.Fatal("the listener accepts connections after Shutdown")
		}
	})
	t.Run("connections without a request", func(t *testing.T) {
		f := start(t, NewServer(metricsText))
		dial(t, f.listener.Addr().String()) // sends nothing
		<-f.listener.accepted
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		begin := time.Now()
		if err := f.server.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
		if elapsed := time.Since(begin); elapsed > time.Second {
			t.Fatalf("Shutdown waited %v for a connection without a request", elapsed)
		}
	})
	t.Run("connection accepted as the shutdown begins", func(t *testing.T) {
		s := NewServer(metricsText)
		inner, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		l := &gatedListener{Listener: inner, gate: make(chan struct{}), held: make(chan struct{}, 1)}
		served := make(chan error, 1)
		go func() { served <- s.Serve(l) }()
		t.Cleanup(func() {
			l.open() // Close waits for Serve, which a held connection would block
			_ = s.Close()
			select {
			case err := <-served:
				if err != nil {
					t.Errorf("Serve: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Error("Serve did not stop")
			}
		})
		first := dial(t, inner.Addr().String()) // sends nothing
		dial(t, inner.Addr().String())          // sends nothing either
		// Serve accepts the second connection only once it has seen the first.
		select {
		case <-l.held:
		case <-time.After(3 * time.Second):
			t.Fatal("the second connection was not accepted")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		begin := time.Now()
		shutdown := make(chan error, 1)
		go func() { shutdown <- s.Shutdown(ctx) }()
		// The server closes the first connection once the shutdown has begun;
		// only then does Serve get the second.
		var b [1]byte
		if _, err := first.Read(b[:]); !errors.Is(err, io.EOF) {
			t.Fatalf("the first connection was not closed: %v", err)
		}
		l.open()
		if err := <-shutdown; err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
		if elapsed := time.Since(begin); elapsed > 2*time.Second {
			t.Fatalf("Shutdown waited %v for a connection accepted as it began", elapsed)
		}
	})
	t.Run("deadline", func(t *testing.T) {
		started, cancelled := make(chan struct{}), make(chan struct{})
		stuck := http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
			close(started)
			<-req.Context().Done()
			close(cancelled)
		})
		f := start(t, NewServer(stuck))
		c := dial(t, f.listener.Addr().String())
		_, _ = io.WriteString(c, "GET /metrics HTTP/1.1\r\nHost: localhost\r\n\r\n")
		<-started
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		if err := f.server.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Shutdown = %v, want the deadline", err)
		}
		select {
		case <-cancelled:
		case <-time.After(time.Second):
			t.Fatal("the request in progress was not cancelled")
		}
	})
}

func TestServeClosesItsListener(t *testing.T) {
	s := NewServer(metricsText)
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Serve(l); err != nil {
		t.Fatalf("Serve after Shutdown = %v", err)
	}
	if _, err := l.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Serve did not close its listener: %v", err)
	}
}
