package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	"github.com/gtriggiano/tessera/internal/config"
	"github.com/gtriggiano/tessera/internal/engine"
	"github.com/gtriggiano/tessera/internal/metrics"
	"github.com/gtriggiano/tessera/internal/request"
)

var testBuild = metrics.BuildInfo{Version: "1.2.3", Revision: "abc123", GoVersion: "go1.test"}

// The test configurations listen on these addresses, which the harness binds
// to free loopback ports instead.
const (
	grpcPlaceholder  = "127.0.0.1:1"
	adminPlaceholder = "127.0.0.1:2"
	// slowAddress never opens: its listen waits until its context ends.
	slowAddress = "127.0.0.1:3"
)

// baseConfig returns a configuration on the placeholder addresses, with
// extra appended to its server block.
func baseConfig(defaultAction, server string) string {
	return "server:\n  listen: " + grpcPlaceholder + "\n" + server + "admin:\n  listen: " + adminPlaceholder + "\npolicy:\n  default_action: " + defaultAction + "\n"
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// harness runs Run in the background.
type harness struct {
	t       *testing.T
	path    string // of the configuration file
	logs    *syncBuffer
	signals chan os.Signal
	done    chan error

	mu       sync.Mutex
	bound    map[string]net.Listener // by configured address
	listened []string                // configured addresses, in order
}

// launch loads yaml from a file in a new directory and runs it.
func launch(t *testing.T, yaml string, opts Options) *harness {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, path: path, logs: &syncBuffer{}, signals: make(chan os.Signal, 4), done: make(chan error, 1), bound: map[string]net.Listener{}}
	opts.Logs, opts.Signals, opts.Build, opts.ConfigPath, opts.Listen = h.logs, h.signals, testBuild, path, h.listen
	go func() { h.done <- Run(cfg, opts) }()
	t.Cleanup(func() {
		select {
		case <-h.done:
			return
		default:
		}
		h.signals <- syscall.SIGTERM
		h.signals <- syscall.SIGTERM
		select {
		case <-h.done:
		case <-time.After(10 * time.Second):
			t.Error("Run did not stop")
		}
	})
	return h
}

func (h *harness) listen(ctx context.Context, network, address string) (net.Listener, error) {
	target := address
	if address == grpcPlaceholder || address == adminPlaceholder {
		target = "127.0.0.1:0"
	}
	if address == slowAddress {
		<-ctx.Done()
		return nil, &net.OpError{Op: "listen", Net: network, Err: ctx.Err()}
	}
	var lc net.ListenConfig
	l, err := lc.Listen(ctx, network, target)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.listened = append(h.listened, address)
	if err == nil {
		h.bound[address] = l
	}
	return l, err
}

func (h *harness) listener(address string) net.Listener {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.bound[address]
}

// waitFor waits until the logs contain an event with msg.
func (h *harness) waitFor(msg string) {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(h.logs.String(), " msg="+msg) {
		if time.Now().After(deadline) {
			h.t.Fatalf("no %s event in:\n%s", msg, h.logs.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

var listeningRE = regexp.MustCompile(`msg=listening listener=(\S+) address=(\S+)`)

// addresses waits until tessera is ready and returns the addresses of its
// listeners, from the listening events.
func (h *harness) addresses() (grpcAddress, adminAddress string) {
	h.t.Helper()
	h.waitFor("ready")
	for _, m := range listeningRE.FindAllStringSubmatch(h.logs.String(), -1) {
		switch m[1] {
		case "grpc":
			grpcAddress = m[2]
		case "admin":
			adminAddress = m[2]
		}
	}
	if grpcAddress == "" || adminAddress == "" {
		h.t.Fatalf("listening events: %q", h.logs.String())
	}
	return grpcAddress, adminAddress
}

// stop sends a signal and returns the result of Run.
func (h *harness) stop() error {
	h.t.Helper()
	h.signals <- syscall.SIGTERM
	return h.result()
}

func (h *harness) result() error {
	h.t.Helper()
	select {
	case err := <-h.done:
		h.done <- err
		return err
	case <-time.After(10 * time.Second):
		h.t.Fatalf("Run did not return; logs:\n%s", h.logs.String())
		return nil
	}
}

// events returns the msg and the attributes of every event, without ts.
func (h *harness) events() []string {
	var out []string
	for line := range strings.Lines(h.logs.String()) {
		_, rest, _ := strings.Cut(strings.TrimSuffix(line, "\n"), " ")
		out = append(out, rest)
	}
	return out
}

func wantEvents(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func dialGRPC(t *testing.T, address string) *grpc.ClientConn {
	t.Helper()
	target := address
	if socket, ok := strings.CutPrefix(address, config.UnixPrefix); ok {
		target = "unix://" + socket
	}
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func check(conn *grpc.ClientConn) (*authv3.CheckResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return authv3.NewAuthorizationClient(conn).Check(ctx, &authv3.CheckRequest{})
}

func health(t *testing.T, conn *grpc.ClientConn, service string) healthgrpc.HealthCheckResponse_ServingStatus {
	t.Helper()
	resp, err := healthgrpc.NewHealthClient(conn).Check(context.Background(), &healthgrpc.HealthCheckRequest{Service: service})
	if err != nil {
		t.Fatalf("health of %q: %v", service, err)
	}
	return resp.GetStatus()
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

func TestServe(t *testing.T) {
	for _, tc := range []struct {
		defaultAction string
		code          codes.Code
	}{
		{"allow", codes.OK},
		{"deny", codes.PermissionDenied},
	} {
		t.Run(tc.defaultAction, func(t *testing.T) {
			h := launch(t, baseConfig(tc.defaultAction, "  drain_delay: 0s\n"), Options{ConfigHash: "0123abcd"})
			grpcAddress, adminAddress := h.addresses()
			conn := dialGRPC(t, grpcAddress)
			resp, err := check(conn)
			if err != nil || codes.Code(resp.GetStatus().GetCode()) != tc.code {
				t.Fatalf("Check: %v, %v; want %v", resp, err, tc.code)
			}
			for _, service := range []string{"", "envoy.service.auth.v3.Authorization"} {
				if got := health(t, conn, service); got != healthgrpc.HealthCheckResponse_SERVING {
					t.Fatalf("health of %q: %v", service, got)
				}
			}
			admin := "http://" + adminAddress
			for path, want := range map[string]string{"/livez": "ok\n", "/readyz": "ready\n"} {
				if code, body := get(t, admin+path); code != 200 || body != want {
					t.Fatalf("%s: %d %q", path, code, body)
				}
			}
			code, body := get(t, admin+"/metrics")
			for _, line := range []string{
				`tessera_build_info{go_version="go1.test",revision="abc123",version="1.2.3"} 1`,
				`tessera_config_info{hash="0123abcd"} 1`,
			} {
				if code != 200 || !strings.Contains(body, "\n"+line+"\n") {
					t.Fatalf("/metrics: %d, no line %s", code, line)
				}
			}
			if err := h.stop(); err != nil {
				t.Fatalf("Run: %v", err)
			}
			wantEvents(t, h.events(), []string{
				`level=info msg=starting version=1.2.3 revision=abc123 config_path=` + h.path,
				`level=info msg=listening listener=grpc address=` + grpcAddress,
				`level=info msg=listening listener=admin address=` + adminAddress,
				`level=info msg=ready`,
				`level=info msg=shutdown_started reason=signal`,
				`level=info msg=not_ready`,
				`level=info msg=shutdown_complete outcome=drained`,
			})
		})
	}
}

func TestEventsInJSON(t *testing.T) {
	h := launch(t, baseConfig("deny", "  drain_delay: 0s\n")+"log:\n  format: json\n", Options{})
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(h.logs.String(), `"msg":"ready"`) {
		if time.Now().After(deadline) {
			t.Fatalf("no ready event: %s", h.logs.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	var msgs []string
	for line := range strings.Lines(h.logs.String()) {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		if event["msg"] == "starting" && (event["version"] != "1.2.3" || event["revision"] != "abc123") {
			t.Fatalf("starting: %v", event)
		}
		msgs = append(msgs, event["msg"].(string))
	}
	if got := strings.Join(msgs, ","); got != "starting,listening,listening,ready,shutdown_started,not_ready,shutdown_complete" {
		t.Fatalf("events %s", got)
	}
}

// blocker holds every decision until release is closed.
type blocker struct {
	entered chan struct{}
	release chan struct{}
}

func newBlocker() *blocker {
	return &blocker{entered: make(chan struct{}, 16), release: make(chan struct{})}
}

func (b *blocker) Decide(*request.Request) engine.Decision {
	b.entered <- struct{}{}
	<-b.release
	return engine.Decision{Action: engine.Allow}
}

// waitClosed waits until address refuses TCP connections.
func waitClosed(t *testing.T, address string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", address, time.Second)
		if err != nil {
			return
		}
		_ = c.Close()
		if time.Now().After(deadline) {
			t.Fatalf("%s still accepts connections", address)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestGracefulShutdown follows a shutdown: /readyz answers 503 while the
// connections still work, the gRPC listener closes after server.drain_delay,
// and a call in progress completes. The drain waits for the checks of the
// test, so that a slow runner cannot miss it.
func TestGracefulShutdown(t *testing.T) {
	const drainDelay = 500 * time.Millisecond
	b := newBlocker()
	checked := make(chan struct{})
	h := launch(t, baseConfig("deny", "  drain_delay: 500ms\n  shutdown_timeout: 10s\n"), Options{decider: b, afterDrain: func() { <-checked }})
	grpcAddress, adminAddress := h.addresses()
	conn := dialGRPC(t, grpcAddress)
	result := make(chan error, 1)
	go func() {
		resp, err := check(conn)
		if err == nil && codes.Code(resp.GetStatus().GetCode()) != codes.OK {
			err = errors.New("not allowed")
		}
		result <- err
	}()
	<-b.entered

	signalled := time.Now()
	h.signals <- syscall.SIGTERM
	readyz := "http://" + adminAddress + "/readyz"
	deadline := time.Now().Add(5 * time.Second)
	for {
		code, body := get(t, readyz)
		if code == 503 {
			if body != "not ready: shutting down\n" {
				t.Fatalf("/readyz body %q", body)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("/readyz did not turn 503")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Not ready, and still answering: on the open connection, and on a new
	// one.
	if got := health(t, conn, ""); got != healthgrpc.HealthCheckResponse_NOT_SERVING {
		t.Fatalf("health during the drain: %v", got)
	}
	if got := health(t, dialGRPC(t, grpcAddress), extauthzService); got != healthgrpc.HealthCheckResponse_NOT_SERVING {
		t.Fatalf("health on a new connection during the drain: %v", got)
	}
	if code, body := get(t, "http://"+adminAddress+"/livez"); code != 200 || body != "ok\n" {
		t.Fatalf("/livez during the drain: %d %q", code, body)
	}
	close(checked)

	waitClosed(t, grpcAddress)
	if elapsed := time.Since(signalled); elapsed < drainDelay {
		t.Fatalf("the gRPC listener closed %v after the signal, before server.drain_delay", elapsed)
	}
	select {
	case err := <-result:
		t.Fatalf("the call in progress ended before its decision: %v", err)
	default:
	}
	close(b.release)
	if err := <-result; err != nil {
		t.Fatalf("the call in progress: %v", err)
	}
	if err := h.result(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	events := h.events()
	wantEvents(t, events[len(events)-3:], []string{
		`level=info msg=shutdown_started reason=signal`,
		`level=info msg=not_ready`,
		`level=info msg=shutdown_complete outcome=drained`,
	})
}

const extauthzService = "envoy.service.auth.v3.Authorization"

// clientPreface is what an HTTP/2 client sends first.
const clientPreface = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"

// rawConnection opens a TCP connection to address that sends prefix and then
// stays silent, as a client stuck in its HTTP/2 handshake.
func rawConnection(t *testing.T, address, prefix string) {
	t.Helper()
	c, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if _, err := io.WriteString(c, prefix); err != nil {
		t.Fatal(err)
	}
}

// stopWithin sends a signal and fails unless Run returns within bound, well
// below the handshake timeout of the gRPC server.
func (h *harness) stopWithin(bound time.Duration) error {
	h.t.Helper()
	begin := time.Now()
	h.signals <- syscall.SIGTERM
	err := h.result()
	if elapsed := time.Since(begin); elapsed > bound {
		h.t.Fatalf("Run returned %v after the signal, want at most %v", elapsed, bound)
	}
	return err
}

// A connection that sent nothing does not delay a shutdown.
func TestSilentConnectionDoesNotHoldTheShutdown(t *testing.T) {
	h := launch(t, baseConfig("deny", "  drain_delay: 0s\n  shutdown_timeout: 10s\n"), Options{})
	grpcAddress, adminAddress := h.addresses()
	rawConnection(t, grpcAddress, "")
	rawConnection(t, adminAddress, "")
	if err := h.stopWithin(2 * time.Second); err != nil {
		t.Fatalf("Run: %v", err)
	}
	events := h.events()
	wantEvents(t, events[len(events)-1:], []string{`level=info msg=shutdown_complete outcome=drained`})
}

// The shutdown ends at server.drain_delay plus server.shutdown_timeout, with a
// call in progress and connections stuck in their handshake.
func TestShutdownTimeout(t *testing.T) {
	b := newBlocker()
	defer close(b.release)
	h := launch(t, baseConfig("deny", "  drain_delay: 0s\n  shutdown_timeout: 200ms\n"), Options{decider: b})
	grpcAddress, _ := h.addresses()
	result := make(chan error, 1)
	go func() {
		_, err := check(dialGRPC(t, grpcAddress))
		result <- err
	}()
	<-b.entered
	rawConnection(t, grpcAddress, "")
	rawConnection(t, grpcAddress, clientPreface[:10])
	err := h.stopWithin(2 * time.Second)
	if err == nil || err.Error() != "shutdown forced by timeout" {
		t.Fatalf("Run: %v", err)
	}
	if err := <-result; status.Code(err) != codes.Unavailable {
		t.Fatalf("the call in progress: err=%v, want UNAVAILABLE", err)
	}
	events := h.events()
	wantEvents(t, events[len(events)-1:], []string{`level=warn msg=shutdown_complete outcome=forced forced_by=timeout`})
}

func TestSecondSignal(t *testing.T) {
	t.Run("during the drain delay", func(t *testing.T) {
		h := launch(t, baseConfig("deny", "  drain_delay: 1h\n"), Options{})
		grpcAddress, _ := h.addresses()
		rawConnection(t, grpcAddress, clientPreface[:10])
		h.signals <- syscall.SIGTERM
		h.waitFor("not_ready")
		if err := h.stopWithin(2 * time.Second); err == nil || err.Error() != "shutdown forced by signal" {
			t.Fatalf("Run: %v", err)
		}
		events := h.events()
		wantEvents(t, events[len(events)-1:], []string{`level=warn msg=shutdown_complete outcome=forced forced_by=signal`})
	})
	t.Run("during the graceful stop", func(t *testing.T) {
		b := newBlocker()
		defer close(b.release)
		h := launch(t, baseConfig("deny", "  drain_delay: 0s\n  shutdown_timeout: 1h\n"), Options{decider: b})
		grpcAddress, _ := h.addresses()
		result := make(chan error, 1)
		go func() {
			_, err := check(dialGRPC(t, grpcAddress))
			result <- err
		}()
		<-b.entered
		rawConnection(t, grpcAddress, clientPreface[:10])
		h.signals <- syscall.SIGTERM
		waitClosed(t, grpcAddress)
		if err := h.stopWithin(2 * time.Second); err == nil || err.Error() != "shutdown forced by signal" {
			t.Fatalf("Run: %v", err)
		}
		if err := <-result; status.Code(err) != codes.Unavailable {
			t.Fatalf("the call in progress: err=%v, want UNAVAILABLE", err)
		}
	})
}

func TestBusyPort(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	t.Run("gRPC", func(t *testing.T) {
		yaml := strings.Replace(baseConfig("deny", ""), grpcPlaceholder, busy.Addr().String(), 1)
		h := launch(t, yaml, Options{})
		if err := h.result(); err == nil || err.Error() != "cannot open the gRPC listener" {
			t.Fatalf("Run: %v", err)
		}
		events := h.events()
		wantEvents(t, events[1:], []string{`level=error msg=startup_failed listener=grpc address=` + busy.Addr().String() + ` error="address already in use"`})
		if len(h.listened) != 1 {
			t.Fatalf("listeners opened after the failure: %v", h.listened)
		}
	})
	t.Run("admin", func(t *testing.T) {
		yaml := strings.Replace(baseConfig("deny", ""), adminPlaceholder, busy.Addr().String(), 1)
		h := launch(t, yaml, Options{})
		if err := h.result(); err == nil || err.Error() != "cannot open the admin listener" {
			t.Fatalf("Run: %v", err)
		}
		events := h.events()
		wantEvents(t, events[1:], []string{`level=error msg=startup_failed listener=admin address=` + busy.Addr().String() + ` error="address already in use"`})
		// The gRPC listener, open first, is closed again.
		address := h.listener(grpcPlaceholder).Addr().String()
		again, err := net.Listen("tcp", address)
		if err != nil {
			t.Fatalf("the gRPC listener %s is still open: %v", address, err)
		}
		_ = again.Close()
	})
}

func TestUnixSocket(t *testing.T) {
	h := launch(t, strings.Replace(baseConfig("allow", "  drain_delay: 0s\n"), grpcPlaceholder, "unix:grpc.sock", 1), Options{})
	grpcAddress, _ := h.addresses()
	socket, ok := strings.CutPrefix(grpcAddress, "unix:")
	if !ok || !filepath.IsAbs(socket) || filepath.Base(socket) != "grpc.sock" {
		t.Fatalf("listening address %q", grpcAddress)
	}
	conn := dialGRPC(t, grpcAddress)
	if resp, err := check(conn); err != nil || codes.Code(resp.GetStatus().GetCode()) != codes.OK {
		t.Fatalf("Check: %v, %v", resp, err)
	}
	if got := health(t, conn, ""); got != healthgrpc.HealthCheckResponse_SERVING {
		t.Fatalf("health: %v", got)
	}
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the socket is still there after the shutdown: %v", err)
	}

	// An existing file at the path stops the startup.
	if err := os.WriteFile(socket, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	yaml := strings.Replace(baseConfig("allow", ""), grpcPlaceholder, "unix:"+socket, 1)
	h = launch(t, yaml, Options{})
	if err := h.result(); err == nil || err.Error() != "cannot open the gRPC listener" {
		t.Fatalf("Run: %v", err)
	}
	events := h.events()
	wantEvents(t, events[1:], []string{`level=error msg=startup_failed listener=grpc address=unix:` + socket + ` error="address already in use"`})
}

// A listener that fails during the drain delay is logged, and Run fails.
func TestListenerFailureDuringTheShutdown(t *testing.T) {
	// The drain ends only once the gRPC server has returned with the error,
	// before the graceful stop could make it return nil.
	grpcFailed := make(chan struct{})
	var once sync.Once
	var h *harness
	h = launch(t, baseConfig("deny", "  drain_delay: 0s\n"), Options{
		served: func(listener string, err error) {
			if listener == "grpc" && err != nil {
				once.Do(func() { close(grpcFailed) })
			}
		},
		afterDrain: func() {
			_ = h.listener(grpcPlaceholder).Close()
			<-grpcFailed
		},
	})
	h.addresses()
	h.signals <- syscall.SIGTERM
	if err := h.result(); err == nil || err.Error() != "the grpc listener failed" {
		t.Fatalf("Run: %v", err)
	}
	events := h.events()
	wantEvents(t, events[4:], []string{
		`level=info msg=shutdown_started reason=signal`,
		`level=info msg=not_ready`,
		`level=error msg=listener_failed listener=grpc`,
		`level=info msg=shutdown_complete outcome=drained`,
	})
}

// A signal stops a listener that cannot open, such as one whose host name
// takes long to resolve, and so does listenTimeout.
func TestSlowListen(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout time.Duration
		signal  bool
		error   string
	}{
		{"signal", time.Hour, true, "interrupted by a signal"},
		{"timeout", 100 * time.Millisecond, false, "timed out"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			yaml := strings.Replace(baseConfig("deny", ""), grpcPlaceholder, slowAddress, 1)
			h := launch(t, yaml, Options{listenTimeout: tc.timeout})
			if tc.signal {
				h.signals <- syscall.SIGTERM
			}
			if err := h.result(); err == nil || err.Error() != "cannot open the gRPC listener" {
				t.Fatalf("Run: %v", err)
			}
			events := h.events()
			wantEvents(t, events[1:], []string{`level=error msg=startup_failed listener=grpc address=` + slowAddress + ` error="` + tc.error + `"`})
		})
	}
}

func TestListenerFailure(t *testing.T) {
	h := launch(t, baseConfig("deny", "  drain_delay: 0s\n"), Options{})
	h.addresses()
	_ = h.listener(grpcPlaceholder).Close()
	if err := h.result(); err == nil || err.Error() != "the grpc listener failed" {
		t.Fatalf("Run: %v", err)
	}
	events := h.events()
	wantEvents(t, events[4:], []string{
		`level=error msg=listener_failed listener=grpc`,
		`level=info msg=shutdown_started reason=listener_failed`,
		`level=info msg=not_ready`,
		`level=info msg=shutdown_complete outcome=drained`,
	})
}

// server.max_connections bounds the connections of the gRPC listener: the
// next one is closed at once, and counted.
func TestMaxConnections(t *testing.T) {
	h := launch(t, baseConfig("deny", "  max_connections: 2\n  drain_delay: 0s\n"), Options{})
	grpcAddress, adminAddress := h.addresses()
	for range 2 {
		if _, err := check(dialGRPC(t, grpcAddress)); err != nil {
			t.Fatal(err)
		}
	}
	extra, err := net.Dial("tcp", grpcAddress)
	if err != nil {
		t.Fatal(err)
	}
	defer extra.Close()
	_ = extra.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := extra.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("the third connection was not closed: %v", err)
	}
	if code, body := get(t, "http://"+adminAddress+"/metrics"); code != 200 || !strings.Contains(body, "\ntessera_grpc_connections_refused_total 1\n") {
		t.Fatalf("/metrics: %d, no tessera_grpc_connections_refused_total 1", code)
	}
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
}
