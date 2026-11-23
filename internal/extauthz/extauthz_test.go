package extauthz

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"reflect"
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
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/gtriggiano/tessera/internal/engine"
	"github.com/gtriggiano/tessera/internal/expr"
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

// fixed decides every request with the same decision.
type fixed engine.Decision

func (f fixed) Decide(*request.Request) engine.Decision { return engine.Decision(f) }

var (
	allowAll = fixed{Action: engine.Allow, Enforced: true}
	denyAll  = fixed{Action: engine.Deny, Enforced: true, Response: &engine.Response{Status: 403}}
)

func TestCheckAnswersTheDecision(t *testing.T) {
	allowed := &authv3.CheckResponse{
		HttpResponse: &authv3.CheckResponse_OkResponse{OkResponse: &authv3.OkHttpResponse{}},
	}
	denied := func(code typev3.StatusCode, body string, headers ...string) *authv3.CheckResponse {
		r := &authv3.DeniedHttpResponse{Status: &typev3.HttpStatus{Code: code}, Body: body}
		for i := 0; i < len(headers); i += 2 {
			r.Headers = append(r.Headers, &corev3.HeaderValueOption{Header: &corev3.HeaderValue{Key: headers[i], Value: headers[i+1]}, Append: wrapperspb.Bool(false), AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD})
		}
		return &authv3.CheckResponse{HttpResponse: &authv3.CheckResponse_DeniedResponse{DeniedResponse: r}}
	}
	for _, tc := range []struct {
		name     string
		decision fixed
		code     codes.Code
		want     *authv3.CheckResponse
	}{
		{"allow", allowAll, codes.OK, allowed},
		{"deny", denyAll, codes.PermissionDenied, denied(typev3.StatusCode_Forbidden, "")},
		{"deny with a response", fixed{Action: engine.Deny, Enforced: true, Response: &engine.Response{
			Status:  451,
			Body:    "Not available in your region.",
			Headers: []engine.Header{{Name: "cache-control", Value: "no-store"}, {Name: "content-type", Value: "text/plain; charset=utf-8"}},
		}}, codes.PermissionDenied, denied(451, "Not available in your region.", "cache-control", "no-store", "content-type", "text/plain; charset=utf-8")},
		{"deny without a response", fixed{Action: engine.Deny, Enforced: true}, codes.PermissionDenied, denied(typev3.StatusCode_Forbidden, "")},
		// A policy in audit mode: the denial is not enforced.
		{"deny in audit mode", fixed{Action: engine.Deny, Enforced: false, Response: &engine.Response{Status: 451}}, codes.OK, allowed},
		// Anything but allow, or a deny that is not enforced, denies.
		{"no decision", fixed{}, codes.PermissionDenied, denied(typev3.StatusCode_Forbidden, "")},
		{"unknown action", fixed{Action: "maybe", Response: &engine.Response{Status: 451}}, codes.PermissionDenied, denied(451, "")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := dial(t, start(t, NewServer(tc.decision, defaultOptions)))
			for _, req := range []*authv3.CheckRequest{httpRequest("GET", "www.example.com", "/"), {}} {
				resp, err := check(t, conn, req)
				if err != nil {
					t.Fatal(err)
				}
				if got := codes.Code(resp.GetStatus().GetCode()); got != tc.code || resp.GetStatus().GetMessage() != "" || len(resp.GetStatus().GetDetails()) != 0 {
					t.Fatalf("status %v, want %v alone", resp.GetStatus(), tc.code)
				}
				// Nothing but the status and the HTTP response: no metadata,
				// and only the headers of the decision.
				resp.Status = nil
				if !proto.Equal(resp, tc.want) {
					t.Fatalf("response %v, want %v", resp, tc.want)
				}
			}
		})
	}
}

// The upstream headers of a request that Envoy sends on: those with a value
// replace the headers of the client, the others are removed; a denial has
// none.
func TestCheckSendsTheUpstreamHeaders(t *testing.T) {
	upstream := []engine.UpstreamHeader{
		{Name: "x-client-asn", Value: ""},
		{Name: "x-client-city", Cause: expr.CauseDatabaseError},
		{Name: "x-client-country", Value: "IT"},
		{Name: "x-client-ip", Value: "192.0.2.7"},
	}
	ok := &authv3.CheckResponse{HttpResponse: &authv3.CheckResponse_OkResponse{OkResponse: &authv3.OkHttpResponse{
		Headers: []*corev3.HeaderValueOption{
			{Header: &corev3.HeaderValue{Key: "x-client-country", Value: "IT"}, Append: wrapperspb.Bool(false), AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD},
			{Header: &corev3.HeaderValue{Key: "x-client-ip", Value: "192.0.2.7"}, Append: wrapperspb.Bool(false), AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD},
		},
		HeadersToRemove: []string{"x-client-asn", "x-client-city"},
	}}}
	removed := &authv3.CheckResponse{HttpResponse: &authv3.CheckResponse_OkResponse{OkResponse: &authv3.OkHttpResponse{
		HeadersToRemove: []string{"x-client-asn"},
	}}}
	for _, tc := range []struct {
		name     string
		decision fixed
		code     codes.Code
		want     *authv3.CheckResponse
	}{
		{"allow", fixed{Action: engine.Allow, Enforced: true, Upstream: upstream}, codes.OK, ok},
		{"deny in audit mode", fixed{Action: engine.Deny, Enforced: false, Response: &engine.Response{Status: 403}, Upstream: upstream}, codes.OK, ok},
		{"every header removed", fixed{Action: engine.Allow, Enforced: true, Upstream: []engine.UpstreamHeader{{Name: "x-client-asn"}}}, codes.OK, removed},
		{"deny", fixed{Action: engine.Deny, Enforced: true, Response: &engine.Response{Status: 403}, Upstream: upstream}, codes.PermissionDenied, &authv3.CheckResponse{
			HttpResponse: &authv3.CheckResponse_DeniedResponse{DeniedResponse: &authv3.DeniedHttpResponse{Status: &typev3.HttpStatus{Code: typev3.StatusCode_Forbidden}}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := dial(t, start(t, NewServer(tc.decision, defaultOptions)))
			resp, err := check(t, conn, httpRequest("GET", "www.example.com", "/"))
			if err != nil {
				t.Fatal(err)
			}
			if got := codes.Code(resp.GetStatus().GetCode()); got != tc.code {
				t.Fatalf("status %v, want %v", resp.GetStatus(), tc.code)
			}
			resp.Status = nil
			if !proto.Equal(resp, tc.want) {
				t.Fatalf("response %v, want %v", resp, tc.want)
			}
		})
	}
}

type recorder struct {
	mu   sync.Mutex
	seen []request.Request
}

// Decide records req, whose time of reception it checks and leaves out.
func (r *recorder) Decide(req *request.Request) engine.Decision {
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := *req
	if seen.Received.IsZero() || time.Since(seen.Received) > 10*time.Second {
		seen.Method = "the time of reception is not set"
	}
	seen.Received = time.Time{}
	r.seen = append(r.seen, seen)
	return engine.Decision{Action: engine.Allow}
}

// decided sends each request to a server that reads the client address from
// source, and returns the requests that it decided.
func decided(t *testing.T, source request.ClientIPSource, reqs ...*authv3.CheckRequest) []request.Request {
	t.Helper()
	rec := &recorder{}
	o := defaultOptions
	o.ClientIP = source
	conn := dial(t, start(t, NewServer(rec, o)))
	for _, r := range reqs {
		if _, err := check(t, conn, r); err != nil {
			t.Fatal(err)
		}
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.seen
}

func withPeer(req *authv3.CheckRequest, address string) *authv3.CheckRequest {
	req.Attributes.Source = &authv3.AttributeContext_Peer{Address: &corev3.Address{Address: &corev3.Address_SocketAddress{
		SocketAddress: &corev3.SocketAddress{Address: address, PortSpecifier: &corev3.SocketAddress_PortValue{PortValue: 50123}},
	}}}
	return req
}

func TestCheckTranslatesTheRequest(t *testing.T) {
	req := withPeer(httpRequest("post", "API.Example.com:8443", "/v1/webhooks?token=x"), "198.51.100.250")
	h := req.Attributes.Request.Http
	// The ID of Envoy's stream is not the request ID: x-request-id is.
	h.Scheme, h.Id = "https", "4217389012876543210"
	h.Headers = map[string]string{":authority": "API.Example.com:8443", "user-agent": "curl/8.0", "x-forwarded-for": "192.0.2.66,203.0.113.10, 198.51.100.250", "x-request-id": "d2c1e2a0"}
	want := request.Request{
		Method:   "POST",
		Host:     "api.example.com",
		Path:     "/v1/webhooks",
		Query:    "token=x",
		Scheme:   "https",
		Headers:  map[string]string{":authority": "API.Example.com:8443", "user-agent": "curl/8.0", "x-forwarded-for": "192.0.2.66,203.0.113.10, 198.51.100.250", "x-request-id": "d2c1e2a0"},
		ID:       "d2c1e2a0",
		Peer:     netip.MustParseAddr("198.51.100.250"),
		ClientIP: netip.MustParseAddr("203.0.113.10"),
	}
	seen := decided(t, request.ClientIPSource{From: request.FromXForwardedFor, TrustedHops: 1}, req, &authv3.CheckRequest{})
	if len(seen) != 2 || !reflect.DeepEqual(seen[0], want) || !reflect.DeepEqual(seen[1], request.Request{Headers: map[string]string{}}) {
		t.Fatalf("decided %+v\nwant %+v", seen, want)
	}
}

// With encode_raw_headers, Envoy sends one entry per header line, whose value
// is in raw_value and not checked for UTF-8: the request is the one that the
// default form gives.
func TestCheckReadsRawHeaders(t *testing.T) {
	joined := withPeer(httpRequest("GET", "www.example.com", "/"), "198.51.100.250")
	joined.Attributes.Request.Http.Headers = map[string]string{"x-forwarded-for": "192.0.2.66,203.0.113.10", "x-bytes": "a!b"}
	raw := withPeer(httpRequest("GET", "www.example.com", "/"), "198.51.100.250")
	raw.Attributes.Request.Http.HeaderMap = &corev3.HeaderMap{Headers: []*corev3.HeaderValue{
		{Key: "x-forwarded-for", RawValue: []byte("192.0.2.66")},
		{Key: "x-bytes", RawValue: []byte("a\xffb")},
		{Key: "x-forwarded-for", Value: "203.0.113.10"},
	}}
	seen := decided(t, request.ClientIPSource{From: request.FromXForwardedFor}, joined, raw)
	if len(seen) != 2 || !reflect.DeepEqual(seen[0], seen[1]) || seen[1].ClientIP != netip.MustParseAddr("203.0.113.10") {
		t.Fatalf("from headers %+v\nfrom header_map %+v", seen[0], seen[1])
	}
}

// The client address comes from the declared source alone: headers that a
// client can write never replace the address of the connection.
func TestCheckClientAddressSource(t *testing.T) {
	req := withPeer(httpRequest("GET", "www.example.com", "/"), "203.0.113.10")
	req.Attributes.Request.Http.Headers = map[string]string{
		"x-forwarded-for":          "192.0.2.66",
		"x-real-ip":                "192.0.2.66",
		"x-envoy-external-address": "198.51.100.7",
	}
	for _, tc := range []struct {
		source request.ClientIPSource
		want   string
	}{
		{request.ClientIPSource{From: request.FromPeer}, "203.0.113.10"},
		{request.ClientIPSource{From: request.FromHeader, Header: "x-envoy-external-address"}, "198.51.100.7"},
		{request.ClientIPSource{From: request.FromHeader, Header: "x-client-address"}, ""},
		{request.ClientIPSource{From: request.FromXForwardedFor, TrustedHops: 0}, "192.0.2.66"},
		{request.ClientIPSource{From: request.FromXForwardedFor, TrustedHops: 1}, ""},
	} {
		seen := decided(t, tc.source, req)
		if got := seen[0].ClientIP; tc.want == "" && got.IsValid() || tc.want != "" && got != netip.MustParseAddr(tc.want) {
			t.Errorf("%+v: client address %v, want %q", tc.source, got, tc.want)
		}
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
	s := NewServer(denyAll, defaultOptions)
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
			address := start(t, NewServer(allowAll, Options{MaxConnections: 16, MaxConcurrentStreams: 16, MaxConnectionAge: tc.age}))
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
	s := NewServer(allowAll, defaultOptions)
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
			s := NewServer(allowAll, defaultOptions)
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
	address := start(t, NewServer(allowAll, Options{MaxConnections: 2, MaxConcurrentStreams: 16, Refused: func() { refused.Add(1) }}))
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

// panicking fails its first decision.
type panicking struct{ calls atomic.Int32 }

func (p *panicking) Decide(*request.Request) engine.Decision {
	if p.calls.Add(1) == 1 {
		var headers map[string]string
		headers["x"] = "y"
	}
	return engine.Decision{Action: engine.Allow, Enforced: true}
}

// A panic while a call is handled fails that call with INTERNAL, which Envoy
// treats as a failed call, is reported, and leaves the server serving.
func TestCheckRecoversPanics(t *testing.T) {
	var mu sync.Mutex
	var reported []*expr.Panic
	o := defaultOptions
	o.OnPanic = func(p *expr.Panic) {
		mu.Lock()
		defer mu.Unlock()
		reported = append(reported, p)
	}
	conn := dial(t, start(t, NewServer(&panicking{}, o)))
	_, err := check(t, conn, httpRequest("GET", "www.example.com", "/"))
	if status.Code(err) != codes.Internal || status.Convert(err).Message() != "internal error" {
		t.Fatalf("first call: %v, want INTERNAL", err)
	}
	if resp, err := check(t, conn, httpRequest("GET", "www.example.com", "/")); err != nil || codes.Code(resp.GetStatus().GetCode()) != codes.OK {
		t.Fatalf("second call: %v, %v", resp, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reported) != 1 || reported[0].Rule != "" || !strings.Contains(string(reported[0].Stack), "extauthz_test.go") {
		t.Fatalf("reported %+v", reported)
	}
}
