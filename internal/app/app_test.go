package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
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
	var logs io.Writer = h.logs
	if g, ok := opts.Logs.(*gate); ok {
		g.dest = h.logs
		logs = g
	}
	opts.Logs, opts.Signals, opts.Build, opts.ConfigPath, opts.Listen = logs, h.signals, testBuild, path, h.listen
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
			// With the default log.decisions, deny, a denial has its event.
			var decisions []string
			events := slices.DeleteFunc(h.events(), func(e string) bool {
				if strings.Contains(e, " msg=decision ") {
					decisions = append(decisions, e[:strings.Index(e, " duration_ms=")])
					return true
				}
				return false
			})
			wantDecisions := map[string][]string{"allow": nil, "deny": {`level=warn msg=decision request_id="" host="" method="" path="" client_ip="" action=deny enforced=true rule=default reason=default`}}[tc.defaultAction]
			if !slices.Equal(decisions, wantDecisions) {
				t.Fatalf("decision events %q, want %q", decisions, wantDecisions)
			}
			wantEvents(t, events, []string{
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

// clientAddresses records the client address of each request it decides.
type clientAddresses struct {
	mu   sync.Mutex
	seen []netip.Addr
}

func (c *clientAddresses) Decide(r *request.Request) engine.Decision {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, r.ClientIP)
	return engine.Decision{Action: engine.Allow}
}

// serve reads the client address from the source that client_ip declares.
func TestClientIPSource(t *testing.T) {
	req := &authv3.CheckRequest{Attributes: &authv3.AttributeContext{
		Source: &authv3.AttributeContext_Peer{Address: &corev3.Address{Address: &corev3.Address_SocketAddress{
			SocketAddress: &corev3.SocketAddress{Address: "2001:db8::5"},
		}}},
		Request: &authv3.AttributeContext_Request{Http: &authv3.AttributeContext_HttpRequest{Headers: map[string]string{
			"x-forwarded-for":          "192.0.2.66, 203.0.113.10, 2001:db8::5",
			"x-envoy-external-address": "198.51.100.7",
		}}},
	}}
	for _, tc := range []struct{ clientIP, want string }{
		{"", "2001:db8::5"},
		{"client_ip:\n  from: header\n  header: x-envoy-external-address\n", "198.51.100.7"},
		{"client_ip:\n  from: x_forwarded_for\n  trusted_hops: 1\n", "203.0.113.10"},
	} {
		decider := &clientAddresses{}
		h := launch(t, baseConfig("deny", "  drain_delay: 0s\n")+tc.clientIP, Options{decider: decider})
		grpcAddress, _ := h.addresses()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := authv3.NewAuthorizationClient(dialGRPC(t, grpcAddress)).Check(ctx, req)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if err := h.stop(); err != nil {
			t.Fatal(err)
		}
		decider.mu.Lock()
		if len(decider.seen) != 1 || decider.seen[0] != netip.MustParseAddr(tc.want) {
			t.Errorf("%q: client addresses %v, want %s", tc.clientIP, decider.seen, tc.want)
		}
		decider.mu.Unlock()
	}
}

// serve decides with the rules of the configuration, and sends the response
// of a denial; a policy in audit mode allows every request.
func TestServeDecidesWithTheRules(t *testing.T) {
	const policy = `
client_ip:
  from: x_forwarded_for
  trusted_hops: 1
policy:
  mode: %s
  default_action: deny
  deny_response:
    body: Forbidden.
  rules:
    - name: blocked
      action: deny
      when: client.ip.inCidr("198.51.100.0/24")
      response:
        status: 451
        headers:
          Cache-Control: no-store
    - name: office
      action: allow
      when: client.ip.inCidr("192.0.2.0/24") && request.path.startsWith("/admin")
`
	checkFrom := func(conn *grpc.ClientConn, xff, path string) *authv3.CheckResponse {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		resp, err := authv3.NewAuthorizationClient(conn).Check(ctx, &authv3.CheckRequest{Attributes: &authv3.AttributeContext{
			Request: &authv3.AttributeContext_Request{Http: &authv3.AttributeContext_HttpRequest{
				Host: "www.example.com", Path: path, Headers: map[string]string{"x-forwarded-for": xff},
			}},
		}})
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	for _, tc := range []struct {
		mode, xff, path string
		code            codes.Code
		status          int32
		body            string
		headers         int
	}{
		{"enforce", "192.0.2.10, 2001:db8::5", "/admin/users", codes.OK, 0, "", 0},
		{"enforce", "198.51.100.7, 2001:db8::5", "/admin/users", codes.PermissionDenied, 451, "Forbidden.", 1},
		{"enforce", "203.0.113.9, 2001:db8::5", "/", codes.PermissionDenied, 403, "Forbidden.", 0},
		// Without a client address, the deny rule denies.
		{"enforce", "2001:db8::5", "/admin/users", codes.PermissionDenied, 451, "Forbidden.", 1},
		{"audit", "198.51.100.7, 2001:db8::5", "/admin/users", codes.OK, 0, "", 0},
		{"audit", "203.0.113.9, 2001:db8::5", "/", codes.OK, 0, "", 0},
	} {
		h := launch(t, "server:\n  listen: "+grpcPlaceholder+"\n  drain_delay: 0s\nadmin:\n  listen: "+adminPlaceholder+"\n"+fmt.Sprintf(policy, tc.mode), Options{})
		grpcAddress, _ := h.addresses()
		resp := checkFrom(dialGRPC(t, grpcAddress), tc.xff, tc.path)
		denied := resp.GetDeniedResponse()
		if codes.Code(resp.GetStatus().GetCode()) != tc.code || int32(denied.GetStatus().GetCode()) != tc.status || denied.GetBody() != tc.body || len(denied.GetHeaders()) != tc.headers {
			t.Errorf("%s, %s %s: %v", tc.mode, tc.xff, tc.path, resp)
		}
		if tc.headers == 1 && (denied.GetHeaders()[0].GetHeader().GetKey() != "cache-control" || denied.GetHeaders()[0].GetHeader().GetValue() != "no-store") {
			t.Errorf("%s, %s %s: headers %v", tc.mode, tc.xff, tc.path, denied.GetHeaders())
		}
		if err := h.stop(); err != nil {
			t.Fatal(err)
		}
	}
}

// gate passes the events to dest, the logs of the harness, until it is shut:
// then every write waits until it opens again.
type gate struct {
	dest   io.Writer
	mu     sync.Mutex
	shut   bool
	opened chan struct{}
}

func (g *gate) close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.shut, g.opened = true, make(chan struct{})
}

func (g *gate) open() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.shut {
		g.shut = false
		close(g.opened)
	}
}

func (g *gate) Write(p []byte) (int, error) {
	g.mu.Lock()
	shut, opened := g.shut, g.opened
	g.mu.Unlock()
	if shut {
		<-opened
	}
	return g.dest.Write(p)
}

// checkMany sends n Check calls from 16 goroutines, the i-th with the host,
// path and X-Forwarded-For of request(i), and fails when one fails.
func checkMany(t *testing.T, conn *grpc.ClientConn, n int, request func(i int) (host, path, xff string)) {
	t.Helper()
	client := authv3.NewAuthorizationClient(conn)
	var wg sync.WaitGroup
	var failures atomic.Int32
	for w := range 16 {
		wg.Go(func() {
			for i := w; i < n; i += 16 {
				host, path, xff := request(i)
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				_, err := client.Check(ctx, &authv3.CheckRequest{Attributes: &authv3.AttributeContext{
					Request: &authv3.AttributeContext_Request{Http: &authv3.AttributeContext_HttpRequest{
						Method: "GET", Host: host, Path: path, Headers: map[string]string{"x-forwarded-for": xff},
					}},
				}})
				cancel()
				if err != nil {
					failures.Add(1)
				}
			}
		})
	}
	wg.Wait()
	if f := failures.Load(); f != 0 {
		t.Fatalf("%d calls of %d failed", f, n)
	}
}

const rulesConfig = `
client_ip:
  from: x_forwarded_for
  trusted_hops: 0
log:
  decisions: all
policy:
  default_action: deny
  rules:
    - name: blocked
      action: deny
      when: client.ip.inCidr("198.51.100.0/24")
    - name: watch
      action: deny
      mode: audit
      when: request.path.contains("/admin")
    - name: office
      action: allow
      when: client.ip.inCidr("192.0.2.0/24")
`

func scrapeSeries(t *testing.T, adminAddress string) []string {
	t.Helper()
	code, body := get(t, "http://"+adminAddress+"/metrics")
	if code != 200 {
		t.Fatalf("/metrics: %d", code)
	}
	var out []string
	for line := range strings.Lines(body) {
		if strings.HasPrefix(line, "tessera_") {
			out = append(out, line[:strings.LastIndexByte(line, ' ')])
		}
	}
	return out
}

// A thousand different hosts, paths and client addresses create no series.
func TestRequestsCreateNoSeries(t *testing.T) {
	h := launch(t, "server:\n  listen: "+grpcPlaceholder+"\n  drain_delay: 0s\nadmin:\n  listen: "+adminPlaceholder+"\n"+rulesConfig, Options{})
	grpcAddress, adminAddress := h.addresses()
	before := scrapeSeries(t, adminAddress)
	checkMany(t, dialGRPC(t, grpcAddress), 1000, func(i int) (string, string, string) {
		return fmt.Sprintf("host-%d.example.com", i), fmt.Sprintf("/admin/%d", i), []string{"198.51.100.", "192.0.2.", "203.0.113."}[i%3] + strconv.Itoa(i%250+1)
	})
	after := scrapeSeries(t, adminAddress)
	if !slices.Equal(before, after) {
		t.Fatalf("series before:\n%s\nafter:\n%s", strings.Join(before, "\n"), strings.Join(after, "\n"))
	}
	_, body := get(t, "http://"+adminAddress+"/metrics")
	for _, line := range []string{
		`tessera_decisions_total{action="deny",enforced="true",reason="rule",rule="blocked"} 334`,
		`tessera_decisions_total{action="allow",enforced="true",reason="rule",rule="office"} 333`,
		`tessera_decisions_total{action="deny",enforced="true",reason="default",rule="default"} 333`,
		`tessera_audit_matches_total{rule="watch"} 666`,
		`tessera_decision_duration_seconds_count{action="deny"} 667`,
		`tessera_policy_audit_mode 0`,
	} {
		if !strings.Contains(body, "\n"+line+"\n") {
			t.Errorf("/metrics has no line %s", line)
		}
	}
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	// Every decision has its event, written before the shutdown completes.
	events := h.events()
	if n := strings.Count(strings.Join(events, "\n"), " msg=decision "); n != 1000 {
		t.Fatalf("%d decision events, want 1000", n)
	}
	if last := events[len(events)-1]; last != "level=info msg=shutdown_complete outcome=drained" {
		t.Fatalf("last event %q", last)
	}
}

// A standard output that blocks never blocks a decision: the decision events
// that do not fit the queue are dropped and counted.
func TestBlockedOutputDoesNotBlockChecks(t *testing.T) {
	g := &gate{}
	h := launch(t, "server:\n  listen: "+grpcPlaceholder+"\n  drain_delay: 0s\nadmin:\n  listen: "+adminPlaceholder+"\n"+rulesConfig, Options{Logs: g})
	grpcAddress, adminAddress := h.addresses()
	g.close()
	start := time.Now()
	checkMany(t, dialGRPC(t, grpcAddress), 5000, func(i int) (string, string, string) {
		return "www.example.com", "/", "192.0.2.10"
	})
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("5000 calls took %v with the output blocked", elapsed)
	}
	_, body := get(t, "http://"+adminAddress+"/metrics")
	m := regexp.MustCompile(`\ntessera_decision_logs_dropped_total (\d+)\n`).FindStringSubmatch(body)
	if m == nil || m[1] == "0" {
		t.Fatalf("no drops counted: %v", m)
	}
	g.open()
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.logs.String(), " msg=decision_logs_dropped dropped=") {
		t.Fatal("no decision_logs_dropped event")
	}
}

// An indeterminate condition is reported by evaluation_error.
func TestEvaluationErrorEvent(t *testing.T) {
	h := launch(t, "server:\n  listen: "+grpcPlaceholder+"\n  drain_delay: 0s\nadmin:\n  listen: "+adminPlaceholder+"\n"+strings.Replace(rulesConfig, "trusted_hops: 0", "trusted_hops: 1", 1), Options{})
	grpcAddress, adminAddress := h.addresses()
	checkMany(t, dialGRPC(t, grpcAddress), 3, func(int) (string, string, string) { return "www.example.com", "/", "192.0.2.10" })
	h.waitFor("evaluation_error")
	if _, body := get(t, "http://"+adminAddress+"/metrics"); !strings.Contains(body, "\n"+`tessera_rule_evaluation_errors_total{cause="client_ip_missing",rule="blocked"} 3`+"\n") {
		t.Error("tessera_rule_evaluation_errors_total does not count the 3 conditions")
	}
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	logs := h.logs.String()
	if want := " level=warn msg=decision request_id=\"\" host=www.example.com method=GET path=/ client_ip=\"\" action=deny enforced=true rule=blocked reason=error error=client_ip_missing duration_ms="; strings.Count(logs, want) != 3 {
		t.Errorf("not 3 events %q in:\n%s", want, logs)
	}
	// The first condition is reported at once, the others with the next
	// report, at the latest when the output closes.
	total := 0
	for _, m := range regexp.MustCompile(` level=warn msg=evaluation_error rule=blocked cause=client_ip_missing count=(\d+)\n`).FindAllStringSubmatch(logs, -1) {
		n, _ := strconv.Atoi(m[1])
		total += n
	}
	if total != 3 {
		t.Errorf("evaluation_error events count %d conditions, want 3:\n%s", total, logs)
	}
}

// flushConfig logs every decision, and drains for a second, during which the
// tests queue decision events.
func flushConfig(shutdownTimeout string) string {
	return "server:\n  listen: " + grpcPlaceholder + "\n  drain_delay: 1s\n  shutdown_timeout: " + shutdownTimeout + "\nadmin:\n  listen: " + adminPlaceholder + "\n" + rulesConfig
}

// queueBehindABlockedOutput starts a shutdown, shuts the gate once it is
// draining, and queues decision events behind the blocked output.
func queueBehindABlockedOutput(t *testing.T, h *harness, g *gate) int {
	t.Helper()
	grpcAddress, _ := h.addresses()
	conn := dialGRPC(t, grpcAddress)
	h.signals <- syscall.SIGTERM
	h.waitFor("not_ready")
	g.close()
	const n = 200
	checkMany(t, conn, n, func(int) (string, string, string) { return "www.example.com", "/", "192.0.2.10" })
	return n
}

// lastEvents checks that shutdown_complete is the last event, with want, and
// that it follows fewer decision events than were decided.
func lastEvents(t *testing.T, h *harness, decided int, want string) {
	t.Helper()
	events := h.events()
	if last := events[len(events)-1]; last != want {
		t.Fatalf("last event %q, want %q", last, want)
	}
	if n := strings.Count(strings.Join(events, "\n"), " msg=decision "); n >= decided {
		t.Fatalf("%d decision events written of %d: none was dropped", n, decided)
	}
}

// Another signal while the queued decision events are written forces the
// shutdown: the events left are dropped, and shutdown_complete stays the last
// event.
func TestSignalDuringTheFlush(t *testing.T) {
	g := &gate{}
	signals := make(chan chan os.Signal, 1)
	h := launch(t, flushConfig("20s"), Options{Logs: g, flushing: func() {
		s := <-signals
		s <- syscall.SIGTERM
		time.AfterFunc(300*time.Millisecond, g.open)
	}})
	signals <- h.signals
	decided := queueBehindABlockedOutput(t, h, g)
	if err := h.result(); err == nil || !strings.Contains(err.Error(), "forced by signal") {
		t.Fatalf("Run: %v", err)
	}
	lastEvents(t, h, decided, "level=warn msg=shutdown_complete outcome=forced forced_by=signal")
}

// A flush that outlasts server.shutdown_timeout forces the shutdown too.
func TestFlushTimeout(t *testing.T) {
	g := &gate{}
	h := launch(t, flushConfig("300ms"), Options{Logs: g, flushing: func() {
		time.AfterFunc(time.Second, g.open)
	}})
	decided := queueBehindABlockedOutput(t, h, g)
	start := time.Now()
	if err := h.result(); err == nil || !strings.Contains(err.Error(), "forced by timeout") {
		t.Fatalf("Run: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the shutdown took %v", elapsed)
	}
	lastEvents(t, h, decided, "level=warn msg=shutdown_complete outcome=forced forced_by=timeout")
}

// failing panics when it decides.
type failing struct{}

func (failing) Decide(*request.Request) engine.Decision {
	var headers map[string]string
	headers["x"] = "y"
	return engine.Decision{}
}

// A failure while serve handles a call fails the call, which Envoy treats as
// a failed call, and serve reports it and goes on.
func TestPanicOutsideAConditionFailsTheCall(t *testing.T) {
	h := launch(t, baseConfig("allow", "  drain_delay: 0s\n"), Options{decider: failing{}})
	grpcAddress, adminAddress := h.addresses()
	if _, err := check(dialGRPC(t, grpcAddress)); status.Code(err) != codes.Internal {
		t.Fatalf("Check: %v, want INTERNAL", err)
	}
	h.waitFor("panic_recovered")
	if _, body := get(t, "http://"+adminAddress+"/metrics"); !strings.Contains(body, "\n"+`tessera_decisions_total{action="allow",enforced="true",reason="default",rule="default"} 0`+"\n") {
		t.Error("a failed call is counted as a decision")
	}
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(` level=error msg=panic_recovered rule="" count=1 error="assignment to entry in nil map" stack="goroutine `).MatchString(h.logs.String()) {
		t.Fatalf("logs:\n%s", h.logs.String())
	}
}
