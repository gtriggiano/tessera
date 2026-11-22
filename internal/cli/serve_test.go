package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gtriggiano/tessera/internal/app"
	"github.com/gtriggiano/tessera/internal/config"
)

// serveConfig listens on placeholder addresses, which ephemeral binds to free
// loopback ports.
const serveConfig = "server:\n  listen: 127.0.0.1:1\n  drain_delay: 0s\nadmin:\n  listen: 127.0.0.1:2\npolicy:\n  default_action: deny\n"

func noServe(t *testing.T) func(*config.Config, app.Options) error {
	return func(*config.Config, app.Options) error {
		t.Error("serve started")
		return nil
	}
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

// ephemeral binds the placeholder addresses of serveConfig to free ports, and
// records every address it binds.
type ephemeral struct {
	mu    sync.Mutex
	bound map[string]string
}

func (e *ephemeral) listen(ctx context.Context, network, address string) (net.Listener, error) {
	target := address
	if address == "127.0.0.1:1" || address == "127.0.0.1:2" {
		target = "127.0.0.1:0"
	}
	var lc net.ListenConfig
	l, err := lc.Listen(ctx, network, target)
	if err == nil {
		e.mu.Lock()
		e.bound[address] = l.Addr().String()
		e.mu.Unlock()
	}
	return l, err
}

func (e *ephemeral) get(address string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.bound[address]
}

func realServe(e *ephemeral) func(*config.Config, app.Options) error {
	return func(cfg *config.Config, opts app.Options) error {
		opts.Listen = e.listen
		return app.Run(cfg, opts)
	}
}

func TestServeHelp(t *testing.T) {
	code, out, errOut := execute([]string{"--help"})
	if code != 0 || errOut != "" || !strings.Contains(out, "\n  serve ") {
		t.Fatalf("the help does not list serve: code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	for _, args := range [][]string{{"serve", "--help"}, {"help", "serve"}} {
		var stdout, stderr bytes.Buffer
		code := Run(context.Background(), args, Options{Out: &stdout, Err: &stderr, Serve: noServe(t),
			LookupEnv: func(string) (string, bool) { t.Error("the help read the environment"); return "", false }})
		help := stdout.String()
		if code != 0 || stderr.Len() != 0 || !strings.Contains(help, "--config") || !strings.Contains(help, "a second signal closes every connection at once") {
			t.Fatalf("args=%q code=%d stdout=%q stderr=%q", args, code, help, stderr.String())
		}
	}
}

func TestServeConfigurationErrorsExitTwo(t *testing.T) {
	const secret = "secret-accidentally-pasted-value"
	invalid := writeConfig(t, "policy:\n  default_action: ${ACTION}\n")
	for _, tc := range []struct {
		name string
		args []string
		vars map[string]string
		err  string
	}{
		{"no file", []string{"serve"}, nil, "tessera: no configuration file; use --config or TESSERA_CONFIG\n"},
		{"empty flag", []string{"serve", "--config="}, map[string]string{"TESSERA_CONFIG": invalid}, "tessera: configuration file path must not be empty\n"},
		{"empty environment", []string{"serve"}, map[string]string{"TESSERA_CONFIG": ""}, "tessera: configuration file path must not be empty\n"},
		{"missing file", []string{"serve", "--config", invalid + ".missing"}, nil, "tessera: configuration file: not found\n"},
		{"invalid", []string{"serve"}, map[string]string{"TESSERA_CONFIG": invalid, "ACTION": secret}, "tessera: policy.default_action (line 2, column 19): expected allow or deny\n"},
		{"argument", []string{"serve", secret}, nil, "tessera: unexpected arguments; see --help\n"},
		{"flag", []string{"serve", "--" + secret}, nil, "tessera: invalid command flags; see --help\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := Run(context.Background(), tc.args, Options{Out: &stdout, Err: &stderr, Serve: noServe(t),
				LookupEnv: func(name string) (string, bool) { v, ok := tc.vars[name]; return v, ok }})
			if code != 2 || stdout.Len() != 0 || stderr.String() != tc.err {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestServeOptions(t *testing.T) {
	path := writeConfig(t, serveConfig)
	signals := make(chan os.Signal, 1)
	var stdout, stderr bytes.Buffer
	var got app.Options
	code := Run(context.Background(), []string{"serve"}, Options{
		Out: &stdout, Err: &stderr, Signals: signals, Build: testBuild,
		LookupEnv: func(name string) (string, bool) { return path, name == "TESSERA_CONFIG" },
		Serve: func(_ *config.Config, opts app.Options) error {
			got = opts
			return nil
		},
	})
	if code != 0 || stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	_, printed, _ := execute([]string{"config", "print", "--config", path})
	hash := sha256.Sum256([]byte(printed))
	if got.ConfigPath != path || got.ConfigHash != hex.EncodeToString(hash[:]) || got.Signals != (<-chan os.Signal)(signals) ||
		got.Build.Version != "v0.1.0" || got.Build.Revision != "abc123" || got.Build.GoVersion != "go1.test" {
		t.Fatalf("serve options: %+v", got)
	}
	if _, err := got.Logs.Write([]byte("event\n")); err != nil || stdout.String() != "event\n" || stderr.Len() != 0 {
		t.Fatalf("logs do not go to standard output: err=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
}

// A standard output that fails, such as a closed pipe, loses the log events
// but does not make serve fail.
func TestServeWithFailingStandardOutput(t *testing.T) {
	path := writeConfig(t, serveConfig)
	for _, output := range []interface{ Write([]byte) (int, error) }{failingWriter{}, shortWriter{}} {
		var stderr bytes.Buffer
		code := Run(context.Background(), []string{"serve", "--config", path}, Options{Out: output, Err: &stderr,
			Serve: func(_ *config.Config, opts app.Options) error {
				_, _ = opts.Logs.Write([]byte("event\n"))
				return nil
			}})
		if code != 0 || stderr.Len() != 0 {
			t.Fatalf("%T: code=%d stderr=%q", output, code, stderr.String())
		}
	}
}

// TestServeReceivesEverySignal checks that serve receives the signals itself:
// the first one does not cancel it, as it cancels the other commands.
func TestServeReceivesEverySignal(t *testing.T) {
	path := writeConfig(t, serveConfig)
	signals := make(chan os.Signal, 2)
	code := Run(context.Background(), []string{"serve", "--config", path}, Options{Signals: signals,
		Serve: func(_ *config.Config, opts app.Options) error {
			for range 2 {
				signals <- syscall.SIGTERM
				select {
				case <-opts.Signals:
				case <-time.After(5 * time.Second):
					return errors.New("serve did not receive the signal")
				}
			}
			return nil
		}})
	if code != 0 {
		t.Fatalf("exit status %d", code)
	}
}

func TestServeRunsUntilASignal(t *testing.T) {
	path := writeConfig(t, serveConfig)
	signals := make(chan os.Signal, 2)
	var stdout, stderr syncBuffer
	e := &ephemeral{bound: map[string]string{}}
	exit := make(chan int, 1)
	go func() {
		exit <- Run(context.Background(), []string{"serve", "--config", path}, Options{Out: &stdout, Err: &stderr, Signals: signals, Build: testBuild, Serve: realServe(e)})
	}()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(stdout.String(), " msg=ready\n") {
		if time.Now().After(deadline) {
			t.Fatalf("no ready event: %q", stdout.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	resp, err := http.Get("http://" + e.get("127.0.0.1:2") + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	_, printed, _ := execute([]string{"config", "print", "--config", path})
	hash := sha256.Sum256([]byte(printed))
	if line := `tessera_config_info{hash="` + hex.EncodeToString(hash[:]) + `"} 1`; !strings.Contains(string(body), "\n"+line+"\n") {
		t.Fatalf("no line %s in /metrics", line)
	}
	signals <- syscall.SIGTERM
	select {
	case code := <-exit:
		logs := stdout.String()
		if code != 0 || stderr.String() != "" || !strings.Contains(logs, " level=info msg=starting version=v0.1.0 revision=abc123 config_path="+path+"\n") ||
			!strings.HasSuffix(logs, " level=info msg=shutdown_complete outcome=drained\n") {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, logs, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop after a signal")
	}
}

func TestServeBusyPortExitsOne(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	path := writeConfig(t, strings.Replace(serveConfig, "127.0.0.1:2", busy.Addr().String(), 1))
	var stdout, stderr bytes.Buffer
	e := &ephemeral{bound: map[string]string{}}
	code := Run(context.Background(), []string{"serve", "--config", path}, Options{Out: &stdout, Err: &stderr, Serve: realServe(e)})
	lines := strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n")
	// The failure is a log event, and nothing else is written.
	if code != 1 || stderr.Len() != 0 || len(lines) != 2 || !strings.Contains(lines[0], " msg=starting ") ||
		!strings.HasSuffix(lines[1], " level=error msg=startup_failed listener=admin address="+busy.Addr().String()+` error="address already in use"`) {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	grpcAddress := e.get("127.0.0.1:1")
	again, err := net.Listen("tcp", grpcAddress)
	if err != nil {
		t.Fatalf("the gRPC listener %s is still open: %v", grpcAddress, err)
	}
	_ = again.Close()
}
