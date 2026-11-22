package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

var testBuild = BuildInfo{Version: "v0.1.0", Revision: "abc123", GoVersion: "go1.test", BuildTime: "2026-11-06T12:00:00Z"}

func execute(args []string) (int, string, string) {
	return executeWithEnv(args, nil)
}

// executeWithEnv runs args with vars as the only environment variables.
func executeWithEnv(args []string, vars map[string]string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	lookup := func(name string) (string, bool) {
		v, ok := vars[name]
		return v, ok
	}
	code := Run(context.Background(), args, Options{Out: &stdout, Err: &stderr, Build: testBuild, LookupEnv: lookup})
	return code, stdout.String(), stderr.String()
}

func TestHelpAndVersionSucceed(t *testing.T) {
	for _, args := range [][]string{
		nil, {"--help"}, {"help"}, {"help", "version"},
		{"version"}, {"version", "--help"}, {"version", "--output", "json"}, {"version", "--output=text"},
		{"config"}, {"config", "--help"}, {"help", "config", "validate"}, {"config", "print", "--help"},
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
		{"config", secret}, {"config", "validate", secret}, {"config", "print", "--" + secret},
		{"config", "validate", "--config", secret}, {"config", "print", "--config"},
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

// writeConfig writes a configuration file in a new directory and returns its
// path.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConfigValidate(t *testing.T) {
	valid := writeConfig(t, "policy:\n  default_action: deny\n")
	invalid := writeConfig(t, "policy:\n  default_action: secret-accidentally-pasted-value\n")
	for _, tc := range []struct {
		name     string
		args     []string
		vars     map[string]string
		code     int
		out, err string
	}{
		{"flag", []string{"config", "validate", "--config", valid}, nil, 0, "configuration valid\n", ""},
		{"flag before the command", []string{"config", "--config", valid, "validate"}, nil, 0, "configuration valid\n", ""},
		{"environment", []string{"config", "validate"}, map[string]string{"TESSERA_CONFIG": valid}, 0, "configuration valid\n", ""},
		{"flag over environment", []string{"config", "validate", "--config", valid}, map[string]string{"TESSERA_CONFIG": invalid}, 0, "configuration valid\n", ""},
		{"no file", []string{"config", "validate"}, nil, 2, "", "tessera: no configuration file; use --config or TESSERA_CONFIG\n"},
		{"empty flag", []string{"config", "validate", "--config", ""}, map[string]string{"TESSERA_CONFIG": valid}, 2, "", "tessera: configuration file path must not be empty\n"},
		{"empty environment", []string{"config", "validate"}, map[string]string{"TESSERA_CONFIG": ""}, 2, "", "tessera: configuration file path must not be empty\n"},
		{"missing file", []string{"config", "validate", "--config", valid + ".missing"}, nil, 2, "", "tessera: configuration file: not found\n"},
		{"invalid", []string{"config", "validate", "--config", invalid}, nil, 2, "", "tessera: policy.default_action (line 2, column 19): expected allow or deny\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errOut := executeWithEnv(tc.args, tc.vars)
			if code != tc.code || out != tc.out || errOut != tc.err {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
			}
		})
	}
}

func TestConfigPrint(t *testing.T) {
	path := writeConfig(t, "policy:\n  default_action: deny\nlog:\n  level: ${LEVEL}\n")
	code, out, errOut := executeWithEnv([]string{"config", "print", "--config", path}, map[string]string{"LEVEL": "debug"})
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if !strings.Contains(out, "  level: '[REDACTED]'\n") || strings.Contains(out, "debug") || !strings.Contains(out, "  default_action: deny\n") || !strings.Contains(out, "  shutdown_timeout: 20s\n") {
		t.Fatalf("stdout=%q", out)
	}
	code, out, errOut = executeWithEnv([]string{"config", "print", "--config", path}, nil)
	if code != 2 || out != "" || errOut != "tessera: log.level (line 4, column 10): environment variable LEVEL is not set\n" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
}

func TestConfigOutputFailuresReturnOne(t *testing.T) {
	path := writeConfig(t, "policy:\n  default_action: deny\n")
	for _, command := range []string{"validate", "print"} {
		var stderr bytes.Buffer
		if code := Run(context.Background(), []string{"config", command, "--config", path}, Options{Out: failingWriter{}, Err: &stderr}); code != 1 || stderr.String() != "tessera: cannot write output\n" {
			t.Fatalf("%s: code=%d stderr=%q", command, code, stderr.String())
		}
	}
}

// config validate compiles the conditions of the rules: an unknown attribute
// or a wrong type stops it with status 2, at its position in the file.
func TestConfigValidateExpressions(t *testing.T) {
	const head = "policy:\n  default_action: deny\n  rules:\n    - name: office\n      action: allow\n"
	for _, tc := range []struct {
		name, when string
		code       int
		err        string
	}{
		{"valid", "      when: client.ip.inCidr(\"192.0.2.0/24\") && request.path.startsWith(\"/admin\")\n", 0, ""},
		{"unknown attribute", "      when: geo.country == \"IT\"\n", 2, "tessera: policy.rules[0].when (line 6, column 13): undeclared reference to 'geo'\n"},
		{"unknown field", "      when: |\n        has(client.ip) &&\n          request.hots == \"www.example.com\"\n", 2, "tessera: policy.rules[0].when (line 8, column 18): undefined field 'hots'\n"},
		{"wrong type", "      when: request.host == 443\n", 2, "tessera: policy.rules[0].when (line 6, column 26): found no matching overload for '_==_' applied to '(string, int)'\n"},
		{"not a bool", "      when: request.path\n", 2, "tessera: policy.rules[0].when (line 6, column 13): the condition must be a bool, not string\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errOut := executeWithEnv([]string{"config", "validate", "--config", writeConfig(t, head+tc.when)}, nil)
			wantOut := ""
			if tc.code == 0 {
				wantOut = "configuration valid\n"
			}
			if code != tc.code || out != wantOut || errOut != tc.err {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
			}
		})
	}
}
