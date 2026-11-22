// Package extauthz is the adapter between Envoy and tessera: the gRPC server
// of Envoy's external authorization protocol, version 3, and of the gRPC
// health service. It translates a CheckRequest into a request.Request, and a
// decision into a CheckResponse. It is the only package that imports Envoy's
// types.
package extauthz

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	rpcstatus "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/grpclog"
	"google.golang.org/grpc/health"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"

	"github.com/gtriggiano/tessera/internal/engine"
	"github.com/gtriggiano/tessera/internal/request"
)

// ServiceName is the full name of the authorization service, which the
// health service also answers for.
const ServiceName = "envoy.service.auth.v3.Authorization"

// MaxMessageBytes is the size of the largest request message the server
// reads. A larger one fails with RESOURCE_EXHAUSTED, without reaching the
// decision.
const MaxMessageBytes = 256 << 10

// HandshakeTimeout is the longest time a new connection has to complete its
// HTTP/2 handshake; the server then closes it.
const HandshakeTimeout = 5 * time.Second

func init() {
	// grpc-go writes its errors to standard error in a format of its own,
	// which would break the stream of log events there, and its messages can
	// carry client addresses.
	grpclog.SetLoggerV2(grpclog.NewLoggerV2(io.Discard, io.Discard, io.Discard))
}

// Decider decides requests, as *engine.Engine does.
type Decider interface {
	Decide(*request.Request) engine.Decision
}

// Options are the ceilings of the server, and how it reports a refused
// connection.
type Options struct {
	// MaxConnections is the largest number of open connections, those in
	// their handshake included. A connection beyond it is closed as soon as
	// it is accepted.
	MaxConnections int
	// Refused, when set, is called for each connection closed because
	// MaxConnections were open.
	Refused func()
	// MaxConcurrentStreams is the largest number of concurrent calls on one
	// connection.
	MaxConcurrentStreams uint32
	// MaxConnectionAge is the age at which a connection is closed gracefully,
	// with a random variation of up to 10%. Zero sets no limit.
	MaxConnectionAge time.Duration
}

// Server serves the authorization and health services on one listener.
type Server struct {
	grpc     *grpc.Server
	health   *health.Server
	stopping chan struct{}
	stopOnce sync.Once

	// conns holds the connections that Serve accepted and that are still
	// open, at most maxConns. A stop of grpc-go waits for the connections in
	// their handshake, so the server closes them itself; once closing, it
	// closes every new one at once.
	mu       sync.Mutex
	conns    map[*conn]struct{}
	closing  bool
	maxConns int
	refused  func()
}

// NewServer returns a server that decides with d, within the ceilings of o.
// Its health service answers NOT_SERVING until SetServing(true).
func NewServer(d Decider, o Options) *Server {
	s := &Server{
		health:   health.NewServer(),
		stopping: make(chan struct{}),
		conns:    map[*conn]struct{}{},
		maxConns: o.MaxConnections,
		refused:  o.Refused,
	}
	s.grpc = grpc.NewServer(
		grpc.ConnectionTimeout(HandshakeTimeout),
		grpc.MaxConcurrentStreams(o.MaxConcurrentStreams),
		grpc.MaxRecvMsgSize(MaxMessageBytes),
		grpc.KeepaliveParams(keepalive.ServerParameters{MaxConnectionAge: o.MaxConnectionAge}),
	)
	authv3.RegisterAuthorizationServer(s.grpc, authorization{decider: d})
	healthgrpc.RegisterHealthServer(s.grpc, healthService{Server: s.health, stopping: s.stopping})
	s.SetServing(false)
	return s
}

// SetServing sets the status that the health service reports, for the empty
// service name and for ServiceName.
func (s *Server) SetServing(serving bool) {
	status := healthgrpc.HealthCheckResponse_NOT_SERVING
	if serving {
		status = healthgrpc.HealthCheckResponse_SERVING
	}
	s.health.SetServingStatus("", status)
	s.health.SetServingStatus(ServiceName, status)
}

// Serve answers the connections of l until GracefulStop or Stop, and closes
// l. It returns nil after a stop, and an error when l fails.
func (s *Server) Serve(l net.Listener) error {
	if err := s.grpc.Serve(&listener{Listener: l, server: s}); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return err
	}
	return nil
}

// GracefulStop closes the listener and the connections whose client has
// sent nothing yet, asks every other client to close its connection, ends
// the health watches and waits for the calls in progress to finish. A
// connection still in its handshake holds it for at most HandshakeTimeout.
// Stop, called meanwhile, closes the remaining connections, and GracefulStop
// returns once the handlers of their calls return.
func (s *Server) GracefulStop() {
	s.endWatches()
	s.closeConns(false)
	s.grpc.GracefulStop()
}

// Stop closes the listener and every connection at once, cancelling the
// calls in progress. It does not wait for their handlers to return.
func (s *Server) Stop() {
	s.endWatches()
	s.closeConns(true)
	s.grpc.Stop()
}

// closeConns closes every open connection, or only those whose client has
// sent nothing, and every connection accepted from now on.
func (s *Server) closeConns(all bool) {
	s.mu.Lock()
	s.closing = true
	var closing []*conn
	for c := range s.conns {
		if all || !c.read.Load() {
			closing = append(closing, c)
		}
	}
	s.mu.Unlock()
	for _, c := range closing {
		_ = c.Close()
	}
}

// listener tracks the connections it accepts in server.conns, and closes
// those beyond server.maxConns without handing them to grpc-go.
type listener struct {
	net.Listener
	server *Server
}

func (l *listener) Accept() (net.Conn, error) {
	s := l.server
	for {
		inner, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		c := &conn{Conn: inner, server: s}
		s.mu.Lock()
		closing, full := s.closing, len(s.conns) >= s.maxConns
		if !closing && !full {
			s.conns[c] = struct{}{}
		}
		s.mu.Unlock()
		switch {
		case closing:
			_ = inner.Close()
			return c, nil
		case full:
			// Counted before the close, so a client that sees the close
			// also sees the count.
			if s.refused != nil {
				s.refused()
			}
			_ = inner.Close()
			continue
		}
		return c, nil
	}
}

// conn is an accepted connection that knows whether its client has sent
// anything.
type conn struct {
	net.Conn
	server *Server
	read   atomic.Bool
	once   sync.Once
}

func (c *conn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.read.Store(true)
	}
	return n, err
}

func (c *conn) Close() error {
	c.once.Do(func() {
		c.server.mu.Lock()
		delete(c.server.conns, c)
		c.server.mu.Unlock()
	})
	return c.Conn.Close()
}

func (s *Server) endWatches() {
	s.stopOnce.Do(func() { close(s.stopping) })
}

type authorization struct {
	authv3.UnimplementedAuthorizationServer
	decider Decider
}

func (a authorization) Check(_ context.Context, req *authv3.CheckRequest) (*authv3.CheckResponse, error) {
	return checkResponse(a.decider.Decide(newRequest(req))), nil
}

// newRequest translates the HTTP request that Envoy describes.
func newRequest(req *authv3.CheckRequest) *request.Request {
	h := req.GetAttributes().GetRequest().GetHttp()
	return &request.Request{Method: h.GetMethod(), Host: h.GetHost(), Path: h.GetPath()}
}

// checkResponse translates a decision. Any action other than allow denies.
func checkResponse(d engine.Decision) *authv3.CheckResponse {
	if d.Action == engine.Allow {
		return &authv3.CheckResponse{
			Status:       &rpcstatus.Status{Code: int32(codes.OK)},
			HttpResponse: &authv3.CheckResponse_OkResponse{OkResponse: &authv3.OkHttpResponse{}},
		}
	}
	return &authv3.CheckResponse{
		Status: &rpcstatus.Status{Code: int32(codes.PermissionDenied)},
		HttpResponse: &authv3.CheckResponse_DeniedResponse{DeniedResponse: &authv3.DeniedHttpResponse{
			Status: &typev3.HttpStatus{Code: typev3.StatusCode_Forbidden},
		}},
	}
}

// healthService ends its Watch streams when the server stops: a graceful
// stop waits for every call in progress, and a watch never ends on its own.
type healthService struct {
	*health.Server
	stopping <-chan struct{}
}

func (h healthService) Watch(req *healthgrpc.HealthCheckRequest, stream healthgrpc.Health_WatchServer) error {
	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()
	go func() {
		select {
		case <-h.stopping:
			cancel()
		case <-ctx.Done():
		}
	}()
	return h.Server.Watch(req, &watchStream{Health_WatchServer: stream, ctx: ctx})
}

// watchStream is a Watch stream whose context also ends when the server
// stops.
type watchStream struct {
	healthgrpc.Health_WatchServer
	ctx context.Context
}

func (w *watchStream) Context() context.Context { return w.ctx }
