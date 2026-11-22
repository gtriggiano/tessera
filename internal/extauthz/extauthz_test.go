package extauthz

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/gtriggiano/tessera/internal/engine"
	"github.com/gtriggiano/tessera/internal/request"
)

var defaultOptions = Options{MaxConnections: 1024, MaxConcurrentStreams: 1024}

// start serves s on a loopback port until the test ends, and returns its
// address.
func start(t *testing.T, s *Server) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- s.Serve(l) }()
	t.Cleanup(func() {
		s.Stop()
		if err := <-served; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return l.Addr().String()
}

// dial opens a client connection to address, closed when the test ends.
func dial(t *testing.T, address string, opts ...grpc.DialOption) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(address, append([]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func check(t *testing.T, conn *grpc.ClientConn, req *authv3.CheckRequest) (*authv3.CheckResponse, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return authv3.NewAuthorizationClient(conn).Check(ctx, req)
}

func httpRequest(method, host, path string) *authv3.CheckRequest {
	return &authv3.CheckRequest{Attributes: &authv3.AttributeContext{Request: &authv3.AttributeContext_Request{
		Http: &authv3.AttributeContext_HttpRequest{Method: method, Host: host, Path: path},
	}}}
}

func TestCheckAnswersTheDecision(t *testing.T) {
	allowed := &authv3.CheckResponse{
		HttpResponse: &authv3.CheckResponse_OkResponse{OkResponse: &authv3.OkHttpResponse{}},
	}
	denied := &authv3.CheckResponse{
		HttpResponse: &authv3.CheckResponse_DeniedResponse{DeniedResponse: &authv3.DeniedHttpResponse{
			Status: &typev3.HttpStatus{Code: typev3.StatusCode_Forbidden},
		}},
	}
	for _, tc := range []struct {
		defaultAction string
		code          codes.Code
		want          *authv3.CheckResponse
	}{
		{"allow", codes.OK, allowed},
		{"deny", codes.PermissionDenied, denied},
	} {
		t.Run(tc.defaultAction, func(t *testing.T) {
			conn := dial(t, start(t, NewServer(engine.New(tc.defaultAction), defaultOptions)))
			for _, req := range []*authv3.CheckRequest{httpRequest("GET", "www.example.com", "/"), {}} {
				resp, err := check(t, conn, req)
				if err != nil {
					t.Fatal(err)
				}
				if got := codes.Code(resp.GetStatus().GetCode()); got != tc.code || resp.GetStatus().GetMessage() != "" || len(resp.GetStatus().GetDetails()) != 0 {
					t.Fatalf("status %v, want %v alone", resp.GetStatus(), tc.code)
				}
				// Nothing but the status and the HTTP response: no header
				// added or removed, no body, no metadata.
				resp.Status = nil
				if !proto.Equal(resp, tc.want) {
					t.Fatalf("response %v, want %v", resp, tc.want)
				}
			}
		})
	}
}

type recorder struct {
	mu   sync.Mutex
	seen []request.Request
}

func (r *recorder) Decide(req *request.Request) engine.Decision {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, *req)
	return engine.Decision{Action: engine.Allow}
}

func TestCheckTranslatesTheRequest(t *testing.T) {
	rec := &recorder{}
	conn := dial(t, start(t, NewServer(rec, defaultOptions)))
	req := httpRequest("POST", "api.example.com:8443", "/v1/webhooks?token=x")
	req.Attributes.Source = &authv3.AttributeContext_Peer{Address: &corev3.Address{Address: &corev3.Address_SocketAddress{
		SocketAddress: &corev3.SocketAddress{Address: "198.51.100.7"},
	}}}
	for _, r := range []*authv3.CheckRequest{req, {}} {
		if _, err := check(t, conn, r); err != nil {
			t.Fatal(err)
		}
	}
	want := []request.Request{{Method: "POST", Host: "api.example.com:8443", Path: "/v1/webhooks?token=x"}, {}}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.seen) != len(want) || rec.seen[0] != want[0] || rec.seen[1] != want[1] {
		t.Fatalf("decided %+v, want %+v", rec.seen, want)
	}
}

func TestMessageSizeLimit(t *testing.T) {
	rec := &recorder{}
	conn := dial(t, start(t, NewServer(rec, defaultOptions)))
	// The path takes all of the message but its framing.
	fits := httpRequest("GET", "www.example.com", "/")
	for size := proto.Size(fits); size != MaxMessageBytes; size = proto.Size(fits) {
		path := fits.Attributes.Request.Http.Path
		if size < MaxMessageBytes {
			path += strings.Repeat("a", MaxMessageBytes-size)
		} else {
			path = path[:len(path)-(size-MaxMessageBytes)]
		}
		fits.Attributes.Request.Http.Path = path
	}
	if _, err := check(t, conn, fits); err != nil {
		t.Fatalf("message of %d bytes: %v", MaxMessageBytes, err)
	}
	fits.Attributes.Request.Http.Path += "a"
	_, err := check(t, conn, fits)
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("message of %d bytes: err=%v, want RESOURCE_EXHAUSTED", proto.Size(fits), err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.seen) != 1 {
		t.Fatalf("%d requests decided, want 1", len(rec.seen))
	}
}

func TestHealthFollowsServing(t *testing.T) {
	s := NewServer(engine.New("deny"), defaultOptions)
	health := healthgrpc.NewHealthClient(dial(t, start(t, s)))
	expect := func(want healthgrpc.HealthCheckResponse_ServingStatus) {
		t.Helper()
		for _, service := range []string{"", ServiceName} {
			resp, err := health.Check(context.Background(), &healthgrpc.HealthCheckRequest{Service: service})
			if err != nil || resp.GetStatus() != want {
				t.Fatalf("service %q: %v, %v; want %v", service, resp.GetStatus(), err, want)
			}
		}
	}
	expect(healthgrpc.HealthCheckResponse_NOT_SERVING)
	s.SetServing(true)
	expect(healthgrpc.HealthCheckResponse_SERVING)
	s.SetServing(false)
	expect(healthgrpc.HealthCheckResponse_NOT_SERVING)
	if _, err := health.Check(context.Background(), &healthgrpc.HealthCheckRequest{Service: "grpc.health.v1.Health"}); status.Code(err) != codes.NotFound {
		t.Fatalf("other service: err=%v, want NOT_FOUND", err)
	}
}

// blocker holds every decision until release is closed.
type blocker struct {
	entered chan struct{}
	release chan struct{}
	active  atomic.Int32
	peak    atomic.Int32
}

func newBlocker() *blocker {
	return &blocker{entered: make(chan struct{}, 64), release: make(chan struct{})}
}

func (b *blocker) Decide(*request.Request) engine.Decision {
	n := b.active.Add(1)
	defer b.active.Add(-1)
	for {
		peak := b.peak.Load()
		if n <= peak || b.peak.CompareAndSwap(peak, n) {
			break
		}
	}
	b.entered <- struct{}{}
	<-b.release
	return engine.Decision{Action: engine.Allow}
}

func TestMaxConcurrentStreams(t *testing.T) {
	b := newBlocker()
	conn := dial(t, start(t, NewServer(b, Options{MaxConnections: 16, MaxConcurrentStreams: 2})))
	const calls = 5
	errs := make(chan error, calls)
	for range calls {
		go func() {
			_, err := check(t, conn, &authv3.CheckRequest{})
			errs <- err
		}()
	}
	for range 2 {
		<-b.entered
	}
	select {
	case <-b.entered:
		t.Fatal("a third call reached the decision on the same connection")
	case <-time.After(200 * time.Millisecond):
	}
	close(b.release)
	for range calls {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if peak := b.peak.Load(); peak != 2 {
		t.Fatalf("%d concurrent calls, want 2", peak)
	}
}

// countingDialer counts the connections a client opens.
type countingDialer struct{ n atomic.Int32 }

func (d *countingDialer) dial(ctx context.Context, address string) (net.Conn, error) {
	d.n.Add(1)
	return (&net.Dialer{}).DialContext(ctx, "tcp", address)
}

func TestMaxConnectionAge(t *testing.T) {
	for _, tc := range []struct {
		age      time.Duration
		minDials int32
		maxDials int32
	}{
		{0, 1, 1},
		// The client may open a new connection at each age.
		{100 * time.Millisecond, 2, 100},
	} {
		t.Run(tc.age.String(), func(t *testing.T) {
			address := start(t, NewServer(engine.New("allow"), Options{MaxConnections: 16, MaxConcurrentStreams: 16, MaxConnectionAge: tc.age}))
			d := &countingDialer{}
			conn := dial(t, address, grpc.WithContextDialer(d.dial))
			if _, err := check(t, conn, &authv3.CheckRequest{}); err != nil {
				t.Fatal(err)
			}
			time.Sleep(500 * time.Millisecond)
			if _, err := check(t, conn, &authv3.CheckRequest{}); err != nil {
				t.Fatal(err)
			}
			if got := d.n.Load(); got < tc.minDials || got > tc.maxDials {
				t.Fatalf("%d connections, want %d to %d", got, tc.minDials, tc.maxDials)
			}
		})
	}
}

func TestGracefulStopWaitsForCallsInProgress(t *testing.T) {
	b := newBlocker()
	s := NewServer(b, defaultOptions)
	conn := dial(t, start(t, s))
	result := make(chan error, 1)
	go func() {
		resp, err := check(t, conn, &authv3.CheckRequest{})
		if err == nil && codes.Code(resp.GetStatus().GetCode()) != codes.OK {
			err = errors.New("not allowed")
		}
		result <- err
	}()
	<-b.entered
	stopped := make(chan struct{})
	go func() {
		s.GracefulStop()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("GracefulStop returned with a call in progress")
	case <-time.After(200 * time.Millisecond):
	}
	close(b.release)
	if err := <-result; err != nil {
		t.Fatalf("call in progress: %v", err)
	}
	<-stopped
}

func TestStopCancelsCallsInProgress(t *testing.T) {
	b := newBlocker()
	s := NewServer(b, defaultOptions)
	conn := dial(t, start(t, s))
	result := make(chan error, 1)
	go func() {
		_, err := check(t, conn, &authv3.CheckRequest{})
		result <- err
	}()
	<-b.entered
	s.Stop()
	if err := <-result; status.Code(err) != codes.Unavailable {
		t.Fatalf("call in progress: err=%v, want UNAVAILABLE", err)
	}
	close(b.release)
}

func TestGracefulStopEndsHealthWatches(t *testing.T) {
	s := NewServer(engine.New("allow"), defaultOptions)
	s.SetServing(true)
	health := healthgrpc.NewHealthClient(dial(t, start(t, s)))
	watch, err := health.Watch(context.Background(), &healthgrpc.HealthCheckRequest{Service: ServiceName})
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := watch.Recv(); err != nil || resp.GetStatus() != healthgrpc.HealthCheckResponse_SERVING {
		t.Fatalf("first update %v, %v", resp.GetStatus(), err)
	}
	s.SetServing(false)
	if resp, err := watch.Recv(); err != nil || resp.GetStatus() != healthgrpc.HealthCheckResponse_NOT_SERVING {
		t.Fatalf("second update %v, %v", resp.GetStatus(), err)
	}
	stopped := make(chan struct{})
	go func() {
		s.GracefulStop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("a health watch held the graceful stop")
	}
	if _, err := watch.Recv(); err == nil {
		t.Fatal("the watch did not end")
	}
}

// A stop does not wait for the connections whose client is still in its
// HTTP/2 handshake: GracefulStop closes those that sent nothing, and Stop
// closes them all.
func TestStopsDoNotWaitForHandshakes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prefix string
		stop   func(*Server)
	}{
		{"graceful, silent client", "", (*Server).GracefulStop},
		{"forced, silent client", "", (*Server).Stop},
		{"forced, partial preface", "PRI * HTTP", (*Server).Stop},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewServer(engine.New("allow"), defaultOptions)
			address := start(t, s)
			c, err := net.Dial("tcp", address)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if _, err := c.Write([]byte(tc.prefix)); err != nil {
				t.Fatal(err)
			}
			// The server has accepted the connection once it answers a call.
			if _, err := check(t, dial(t, address), &authv3.CheckRequest{}); err != nil {
				t.Fatal(err)
			}
			begin := time.Now()
			tc.stop(s)
			if elapsed := time.Since(begin); elapsed > time.Second {
				t.Fatalf("the stop took %v, want well below the handshake timeout of %v", elapsed, HandshakeTimeout)
			}
		})
	}
}

// The connections beyond MaxConnections are closed as soon as accepted, and
// a connection that closes frees its place.
func TestMaxConnections(t *testing.T) {
	var refused atomic.Int32
	address := start(t, NewServer(engine.New("allow"), Options{MaxConnections: 2, MaxConcurrentStreams: 16, Refused: func() { refused.Add(1) }}))
	first, second := dial(t, address), dial(t, address)
	for _, conn := range []*grpc.ClientConn{first, second} {
		if _, err := check(t, conn, &authv3.CheckRequest{}); err != nil {
			t.Fatal(err)
		}
	}
	extra, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer extra.Close()
	_ = extra.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := extra.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("a connection beyond the limit was not closed: %v", err)
	}
	if got := refused.Load(); got != 1 {
		t.Fatalf("%d refusals, want 1", got)
	}
	// Once a connection closes, a new one is served: the client retries until
	// the server has noticed the close.
	_ = first.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := authv3.NewAuthorizationClient(dial(t, address)).Check(ctx, &authv3.CheckRequest{}, grpc.WaitForReady(true)); err != nil {
		t.Fatalf("no connection served after one closed: %v", err)
	}
}
