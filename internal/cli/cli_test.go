package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

var testBuild = BuildInfo{Version: "v0.1.0", Revision: "abc123", GoVersion: "go1.test", BuildTime: "2026-11-06T12:00:00Z"}

func execute(args []string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), args, Options{Out: &stdout, Err: &stderr, Build: testBuild})
	return code, stdout.String(), stderr.String()
}

func TestHelpAndVersionSucceed(t *testing.T) {
	for _, args := range [][]string{
		nil, {"--help"}, {"help"}, {"help", "version"},
		{"version"}, {"version", "--help"}, {"version", "--output", "json"}, {"version", "--output=text"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, out, errOut := execute(args)
			if code != 0 || out == "" || errOut != "" {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
			}
		})
	}
}

func TestVersionText(t *testing.T) {
	code, out, errOut := execute([]string{"version"})
	want := "tessera v0.1.0 (revision abc123; go1.test; built 2026-11-06T12:00:00Z)\n"
	if code != 0 || out != want || errOut != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	var stdout bytes.Buffer
	if code := Run(context.Background(), []string{"version"}, Options{Out: &stdout}); code != 0 {
		t.Fatal(code)
	}
	if !strings.HasPrefix(stdout.String(), "tessera dev (revision unknown; go") || strings.Contains(stdout.String(), "built") {
		t.Fatal(stdout.String())
	}
}

func TestVersionJSON(t *testing.T) {
	code, out, errOut := execute([]string{"version", "--output", "json"})
	want := `{"version":"v0.1.0","revision":"abc123","go_version":"go1.test","build_time":"2026-11-06T12:00:00Z"}` + "\n"
	if code != 0 || out != want || errOut != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	var stdout bytes.Buffer
	if code := Run(context.Background(), []string{"version", "--output", "json"}, Options{Out: &stdout}); code != 0 {
		t.Fatal(code)
	}
	var fields map[string]string
	if err := json.Unmarshal(stdout.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	if fields["version"] != "dev" || fields["revision"] != "unknown" || !strings.HasPrefix(fields["go_version"], "go") {
		t.Fatal(stdout.String())
	}
	if _, ok := fields["build_time"]; ok || len(fields) != 3 {
		t.Fatalf("unexpected fields: %s", stdout.String())
	}
}

func TestUsageErrorsDoNotEchoArguments(t *testing.T) {
	const secret = "secret-accidentally-pasted-value"
	for _, args := range [][]string{
		{secret}, {"help", secret}, {"help", "version", secret}, {"version", secret},
		{"--" + secret}, {"version", "--" + secret}, {"-version"},
		{"version", "--output", secret}, {"version", "--output"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, out, errOut := execute(args)
			if code != 2 || out != "" || errOut == "" || strings.Contains(errOut, secret) {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
			}
			if !strings.HasPrefix(errOut, "tessera: ") || strings.Count(errOut, "tessera:") != 1 {
				t.Fatalf("stderr=%q, want one tessera: line", errOut)
			}
		})
	}
}

// Cobra echoes an unknown subcommand in its error when a command with
// subcommands has no Args validator. Run rewrites that message, but the
// validator keeps the argument out of the error in the first place.
func TestCommandGroupsValidateArguments(t *testing.T) {
	var walk func(*cobra.Command)
	walk = func(cmd *cobra.Command) {
		if cmd.HasSubCommands() && cmd.Args == nil {
			t.Errorf("%q has subcommands and no Args validator", cmd.CommandPath())
		}
		for _, child := range cmd.Commands() {
			walk(child)
		}
	}
	walk(newCommand(defaults(Options{})))
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("broken output") }

type shortWriter struct{}

func (shortWriter) Write([]byte) (int, error) { return 0, nil }

func TestRuntimeFailuresReturnOne(t *testing.T) {
	var stderr bytes.Buffer
	for _, args := range [][]string{{"version"}, {"version", "--output", "json"}, {"--help"}, nil} {
		for _, output := range []interface{ Write([]byte) (int, error) }{failingWriter{}, shortWriter{}} {
			stderr.Reset()
			if code := Run(context.Background(), args, Options{Out: output, Err: &stderr}); code != 1 || stderr.String() != "tessera: cannot write output\n" {
				t.Fatalf("args=%v code=%d stderr=%q", args, code, stderr.String())
			}
		}
	}
	for _, output := range []interface{ Write([]byte) (int, error) }{failingWriter{}, shortWriter{}} {
		if code := Run(context.Background(), []string{"unknown-command"}, Options{Err: output}); code != 1 {
			t.Fatalf("failed diagnostic write returned %d, want 1", code)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout bytes.Buffer
	stderr.Reset()
	if code := Run(ctx, []string{"version"}, Options{Out: &stdout, Err: &stderr}); code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "context canceled") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestSignalCancelsTheCommandContext(t *testing.T) {
	signals := make(chan os.Signal, 1)
	cmd := newCommand(defaults(Options{Signals: signals}))
	cmd.SetArgs([]string{"version"})
	done := make(chan context.Context, 1)
	version, _, err := cmd.Find([]string{"version"})
	if err != nil {
		t.Fatal(err)
	}
	version.RunE = func(cmd *cobra.Command, _ []string) error {
		done <- cmd.Context()
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatal(err)
	}
	commandCtx := <-done
	signals <- os.Interrupt
	select {
	case <-commandCtx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("signal did not cancel the command context")
	}
}
