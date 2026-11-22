package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// secret is a value that no error may repeat.
const secret = "s3cr3t-value"

func env(vars map[string]string) LookupEnv {
	return func(name string) (string, bool) {
		v, ok := vars[name]
		return v, ok
	}
}

func parse(t *testing.T, yaml string, vars map[string]string) (*Config, error) {
	t.Helper()
	return Parse([]byte(yaml), env(vars))
}

func mustParse(t *testing.T, yaml string, vars map[string]string) *Config {
	t.Helper()
	c, err := parse(t, yaml, vars)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return c
}

func TestDefaults(t *testing.T) {
	c := mustParse(t, "policy:\n  default_action: deny\n", nil)
	want := Config{
		Server: Server{Listen: ":9001", MaxConcurrentStreams: 1024, DrainDelay: 5 * time.Second, ShutdownTimeout: 20 * time.Second},
		Admin:  Admin{Listen: ":9090"},
		Log:    Log{Level: "info", Format: "logfmt"},
		Policy: Policy{DefaultAction: "deny"},
	}
	if c.Server != want.Server || c.Admin != want.Admin || c.Log != want.Log || c.Policy != want.Policy {
		t.Fatalf("got %+v", *c)
	}
}

func TestEveryKey(t *testing.T) {
	c := mustParse(t, `
server:
  listen: 127.0.0.1:9101
  max_concurrent_streams: 64
  max_connection_age: 30m
  drain_delay: 0s
  shutdown_timeout: 1m30s
admin:
  listen: "[::1]:9102"
log:
  level: debug
  format: json
policy:
  default_action: allow
`, nil)
	want := Config{
		Server: Server{Listen: "127.0.0.1:9101", MaxConcurrentStreams: 64, MaxConnectionAge: 30 * time.Minute, ShutdownTimeout: 90 * time.Second},
		Admin:  Admin{Listen: "[::1]:9102"},
		Log:    Log{Level: "debug", Format: "json"},
		Policy: Policy{DefaultAction: "allow"},
	}
	if c.Server != want.Server || c.Admin != want.Admin || c.Log != want.Log || c.Policy != want.Policy {
		t.Fatalf("got %+v", *c)
	}
}

func TestInvalidConfigurations(t *testing.T) {
	for _, tc := range []struct{ name, yaml, want string }{
		{"empty", "", "configuration: the file is empty"},
		{"comments only", "# nothing\n", "configuration: the file is empty"},
		{"syntax", "policy:\n  default_action: [\n", "configuration (line 2, column 1): invalid YAML syntax"},
		{"two documents", "policy: {default_action: deny}\n---\npolicy: {default_action: deny}\n", "configuration: more than one YAML document"},
		{"root list", "- policy\n", "configuration: the root is not a mapping"},
		{"root scalar", secret + "\n", "configuration: the root is not a mapping"},
		{"missing policy", "log:\n  level: info\n", "policy.default_action: required"},
		{"missing default action", "policy: {}\n", "policy.default_action (line 1, column 9): required"},
		{"unknown top-level key", "policy: {default_action: deny}\nsets: {}\n", "sets (line 2, column 1): unknown key"},
		{"unknown nested key", "policy:\n  default_action: deny\n  rules: []\n", "policy.rules (line 3, column 3): unknown key"},
		{"unknown key not echoed", "policy: {default_action: deny}\n" + secret + ": 1\n", "<unknown> (line 2, column 1): unknown key"},
		{"duplicate key", "policy:\n  default_action: deny\n  default_action: allow\n", "policy.default_action (line 3, column 3): duplicate key"},
		{"null", "policy: {default_action: deny}\nserver: ~\n", "server (line 2, column 9): null is not allowed; omit the key to use its default"},
		{"empty value", "policy:\n  default_action:\n", "policy.default_action (line 2, column 18): null is not allowed; omit the key to use its default"},
		{"custom tag on the root", "!config\npolicy: {default_action: deny}\n", "configuration (line 1, column 1): custom YAML tags are not allowed"},
		{"standard tag on a mapping", "policy: !!str {default_action: deny}\n", "policy (line 1, column 9): expected a mapping"},
		{"custom tag", "policy: {default_action: !secret deny}\n", "policy.default_action (line 1, column 26): custom YAML tags are not allowed"},
		{"alias", "policy:\n  default_action: &a deny\nlog:\n  level: *a\n", "log.level (line 4, column 10): YAML aliases are not allowed"},
		{"merge key", "base: &b {default_action: deny}\npolicy:\n  <<: *b\n", "base (line 1, column 1): unknown key"},
		{"merge key in a block", "policy:\n  <<: {default_action: deny}\n", "policy (line 2, column 3): keys must be plain strings; merge keys are not allowed"},
		{"non-string key", "policy: {default_action: deny}\n1: x\n", "configuration (line 2, column 1): keys must be plain strings; merge keys are not allowed"},
		{"mapping expected", "policy: deny\n", "policy (line 1, column 9): expected a mapping"},
		{"string expected", "policy:\n  default_action: 1\n", "policy.default_action (line 2, column 19): expected a string"},
		{"integer expected", "policy: {default_action: deny}\nserver:\n  max_concurrent_streams: \"16\"\n", "server.max_concurrent_streams (line 3, column 27): expected a decimal integer"},
		{"hex integer", "policy: {default_action: deny}\nserver:\n  max_concurrent_streams: 0x10\n", "server.max_concurrent_streams (line 3, column 27): expected a decimal integer"},
		{"integer out of range", "policy: {default_action: deny}\nserver:\n  max_concurrent_streams: 9223372036854775808\n", "server.max_concurrent_streams (line 3, column 27): integer out of range"},
		{"duration without units", "policy: {default_action: deny}\nserver:\n  drain_delay: \"0\"\n", "server.drain_delay (line 3, column 16): expected a duration with units, such as 5s"},
		{"duration as integer", "policy: {default_action: deny}\nserver:\n  drain_delay: 5\n", "server.drain_delay (line 3, column 16): expected a duration with units, such as 5s"},
		{"duration in days", "policy: {default_action: deny}\nserver:\n  drain_delay: 1d\n", "server.drain_delay (line 3, column 16): expected a duration with units, such as 5s"},
		{"default action", "policy:\n  default_action: " + secret + "\n", "policy.default_action (line 2, column 19): expected allow or deny"},
		{"default action case", "policy:\n  default_action: Deny\n", "policy.default_action (line 2, column 19): expected allow or deny"},
		{"log level", "policy: {default_action: deny}\nlog: {level: trace}\n", "log.level (line 2, column 14): expected debug, info, warn or error"},
		{"log format", "policy: {default_action: deny}\nlog: {format: text}\n", "log.format (line 2, column 15): expected logfmt or json"},
		{"listen without port", "policy: {default_action: deny}\nserver: {listen: " + secret + "}\n", "server.listen (line 2, column 18): expected host:port or unix: followed by a path"},
		{"listen port zero", "policy: {default_action: deny}\nserver: {listen: \":0\"}\n", "server.listen (line 2, column 18): expected host:port or unix: followed by a path"},
		{"listen port too large", "policy: {default_action: deny}\nserver: {listen: \":65536\"}\n", "server.listen (line 2, column 18): expected host:port or unix: followed by a path"},
		{"listen port leading zero", "policy: {default_action: deny}\nserver: {listen: \":09001\"}\n", "server.listen (line 2, column 18): expected host:port or unix: followed by a path"},
		{"listen named port", "policy: {default_action: deny}\nserver: {listen: \"localhost:http\"}\n", "server.listen (line 2, column 18): expected host:port or unix: followed by a path"},
		{"listen numeric host", "policy: {default_action: deny}\nserver: {listen: \"127.1:9001\"}\n", "server.listen (line 2, column 18): expected host:port or unix: followed by a path"},
		{"listen bad host", "policy: {default_action: deny}\nserver: {listen: \"-bad-.example.com:9001\"}\n", "server.listen (line 2, column 18): expected host:port or unix: followed by a path"},
		{"empty socket path", "policy: {default_action: deny}\nserver: {listen: \"unix:\"}\n", "server.listen (line 2, column 18): expected host:port or unix: followed by a path"},
		{"control character in socket path", "policy: {default_action: deny}\nserver: {listen: \"unix:/run/a\\nb\"}\n", "server.listen (line 2, column 18): expected host:port or unix: followed by a path"},
		{"admin listen socket", "policy: {default_action: deny}\nadmin: {listen: \"unix:/run/admin.sock\"}\n", "admin.listen (line 2, column 17): expected host:port"},
		{"same port", "policy: {default_action: deny}\nserver: {listen: \"127.0.0.1:9000\"}\nadmin: {listen: \"localhost:9000\"}\n", "admin.listen (line 3, column 17): must not share its address and port with server.listen"},
		{"same port as default admin", "policy: {default_action: deny}\nserver: {listen: \"192.0.2.10:9090\"}\n", "server.listen (line 2, column 18): must not share its address and port with admin.listen"},
		{"streams zero", "policy: {default_action: deny}\nserver: {max_concurrent_streams: 0}\n", "server.max_concurrent_streams (line 2, column 34): expected an integer from 1 to 4294967295"},
		{"streams too many", "policy: {default_action: deny}\nserver: {max_concurrent_streams: 4294967296}\n", "server.max_concurrent_streams (line 2, column 34): expected an integer from 1 to 4294967295"},
		{"negative connection age", "policy: {default_action: deny}\nserver: {max_connection_age: -1s}\n", "server.max_connection_age (line 2, column 30): must not be negative"},
		{"negative drain delay", "policy: {default_action: deny}\nserver: {drain_delay: -1s}\n", "server.drain_delay (line 2, column 23): must not be negative"},
		{"zero shutdown timeout", "policy: {default_action: deny}\nserver: {shutdown_timeout: 0s}\n", "server.shutdown_timeout (line 2, column 28): must be positive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parse(t, tc.yaml, nil)
			if err == nil {
				t.Fatal("Parse succeeded")
			}
			if err.Error() != tc.want {
				t.Fatalf("error %q, want %q", err, tc.want)
			}
		})
	}
}

func TestErrorType(t *testing.T) {
	_, err := parse(t, "policy:\n  default_action: maybe\n", nil)
	var e *Error
	if !errors.As(err, &e) || e.Path != "policy.default_action" || e.Line != 2 || e.Column != 19 {
		t.Fatalf("err=%#v", err)
	}
}

func TestValidListeners(t *testing.T) {
	for _, tc := range []struct{ server, admin string }{
		{":9001", ":9090"},
		{"0.0.0.0:9001", "127.0.0.1:9090"},
		{"[::]:9001", "[::1]:9090"},
		{"localhost:9001", "tessera.example.com:9090"},
		{"192.0.2.10:9000", "192.0.2.11:9000"},
		{"unix:/run/tessera/grpc.sock", ":9001"},
	} {
		c := mustParse(t, "policy: {default_action: deny}\nserver: {listen: \""+tc.server+"\"}\nadmin: {listen: \""+tc.admin+"\"}\n", nil)
		if c.Server.Listen != tc.server || c.Admin.Listen != tc.admin {
			t.Fatalf("got %q and %q", c.Server.Listen, c.Admin.Listen)
		}
	}
	for _, tc := range []struct{ server, admin string }{
		{":9000", "192.0.2.10:9000"},
		{"[::]:9000", "127.0.0.1:9000"},
		{"[::ffff:192.0.2.10]:9000", "192.0.2.10:9000"},
		{"127.0.0.1:9000", "[::1]:9000"},
		{"LOCALHOST:9000", "localhost:9000"},
	} {
		if _, err := parse(t, "policy: {default_action: deny}\nserver: {listen: \""+tc.server+"\"}\nadmin: {listen: \""+tc.admin+"\"}\n", nil); err == nil || !strings.Contains(err.Error(), "must not share") {
			t.Fatalf("%s and %s: err=%v", tc.server, tc.admin, err)
		}
	}
}

func TestEnvironmentSubstitution(t *testing.T) {
	vars := map[string]string{"LEVEL": "debug", "EMPTY": "", "PORT": "9101", "DELAY": "2s"}
	for _, tc := range []struct{ value, want string }{
		{"${LEVEL}", "debug"},
		{"${LEVEL:-warn}", "debug"},
		{"${UNSET:-warn}", "warn"},
		{"${EMPTY:-warn}", "warn"},
		{"de${EMPTY}bug", "debug"},
		{"${UNSET:-}debug", "debug"},
	} {
		c := mustParse(t, "policy: {default_action: deny}\nlog: {level: \""+tc.value+"\"}\n", vars)
		if c.Log.Level != tc.want {
			t.Fatalf("%s: got %q, want %q", tc.value, c.Log.Level, tc.want)
		}
	}
	c := mustParse(t, "policy: {default_action: deny}\nserver:\n  listen: \"unix:/run/$$${a}$b$$c${PORT}\"\n  drain_delay: ${DELAY}\n", vars)
	if c.Server.Listen != "unix:/run/$${a}$b$$c9101" || c.Server.DrainDelay != 2*time.Second {
		t.Fatalf("got %q and %v", c.Server.Listen, c.Server.DrainDelay)
	}
	for _, tc := range []struct{ value, want string }{
		{"${UNSET}", "log.level (line 2, column 14): environment variable UNSET is not set"},
		{"${LEVEL", "log.level (line 2, column 14): unterminated ${ substitution"},
		{"${1LEVEL}", "log.level (line 2, column 14): invalid environment variable name in a ${ substitution"},
		{"${}", "log.level (line 2, column 14): invalid environment variable name in a ${ substitution"},
		{"${SECRET}", "log.level (line 2, column 14): expected debug, info, warn or error"},
	} {
		_, err := parse(t, "policy: {default_action: deny}\nlog: {level: \""+tc.value+"\"}\n", map[string]string{"LEVEL": "debug", "SECRET": secret})
		if err == nil || err.Error() != tc.want {
			t.Fatalf("%s: err=%v, want %q", tc.value, err, tc.want)
		}
	}
	// A substituted value is not repeated in an error, whatever its type.
	for _, yaml := range []string{
		`server: {listen: "${SECRET}"}`,
		`server: {listen: "localhost:${SECRET}"}`,
		`server: {drain_delay: "${SECRET}"}`,
		`admin: {listen: "${SECRET}"}`,
		`policy: {default_action: "${SECRET}"}`,
	} {
		_, err := parse(t, "policy: {default_action: deny}\n"+yaml+"\n", map[string]string{"SECRET": secret})
		if err == nil || strings.Contains(err.Error(), secret) {
			t.Fatalf("%s: err=%v", yaml, err)
		}
	}
	// Substitution applies to values, not to keys or integers.
	if _, err := parse(t, "policy: {default_action: deny}\nserver: {max_concurrent_streams: ${N}}\n", map[string]string{"N": "8"}); err == nil {
		t.Fatal("integer substitution accepted")
	}
	if _, err := parse(t, "policy: {default_action: deny}\n${KEY}: {}\n", map[string]string{"KEY": "log"}); err == nil {
		t.Fatal("key substitution accepted")
	}
}

func TestRedacted(t *testing.T) {
	c := mustParse(t, "policy:\n  default_action: ${ACTION:-deny}\nlog:\n  level: debug\n  format: ${FORMAT}\n", map[string]string{"FORMAT": "json"})
	got, err := c.Redacted()
	if err != nil {
		t.Fatal(err)
	}
	want := `server:
  listen: :9001
  max_concurrent_streams: 1024
  max_connection_age: 0s
  drain_delay: 5s
  shutdown_timeout: 20s
admin:
  listen: :9090
log:
  level: debug
  format: '[REDACTED]'
policy:
  default_action: '[REDACTED]'
`
	if string(got) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	// The printed configuration is itself valid, and reads back the same.
	printed := strings.NewReplacer("format: '[REDACTED]'", "format: json", "default_action: '[REDACTED]'", "default_action: deny").Replace(string(got))
	again := mustParse(t, printed, nil)
	if again.Server != c.Server || again.Admin != c.Admin || again.Log != c.Log || again.Policy != c.Policy {
		t.Fatalf("got %+v", *again)
	}
	// A literal ${ is printed escaped, so that it reads back as written.
	c = mustParse(t, "policy: {default_action: deny}\nserver: {listen: \"unix:/run/$${x}.sock\"}\n", nil)
	if got, err = c.Redacted(); err != nil {
		t.Fatal(err)
	}
	again = mustParse(t, string(got), map[string]string{"x": "substituted"})
	if !strings.Contains(string(got), "  listen: unix:/run/$${x}.sock\n") || again.Server.Listen != "unix:/run/${x}.sock" {
		t.Fatalf("got:\n%s", got)
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	c, err := Load(write("relative.yaml", "policy: {default_action: deny}\nserver: {listen: \"unix:run/grpc.sock\"}\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := "unix:" + filepath.Join(dir, "run", "grpc.sock"); c.Server.Listen != want {
		t.Fatalf("got %q, want %q", c.Server.Listen, want)
	}
	c, err = Load(write("absolute.yaml", "policy: {default_action: deny}\nserver: {listen: \"unix:/run/grpc.sock\"}\n"), nil)
	if err != nil || c.Server.Listen != "unix:/run/grpc.sock" {
		t.Fatalf("listen=%q err=%v", c.Server.Listen, err)
	}
	c, err = Load(write("tcp.yaml", "policy: {default_action: deny}\n"), nil)
	if err != nil || c.Server.Listen != ":9001" {
		t.Fatalf("listen=%q err=%v", c.Server.Listen, err)
	}
	largest := "policy: {default_action: deny}\n#"
	largest += strings.Repeat("x", MaxFileBytes-len(largest))
	if _, err := Load(write("largest.yaml", largest), nil); err != nil {
		t.Fatalf("file of %d bytes: %v", len(largest), err)
	}
	big := "policy: {default_action: deny}\n" + strings.Repeat("#", MaxFileBytes)
	for _, tc := range []struct{ path, want string }{
		{filepath.Join(dir, secret+".yaml"), "configuration file: not found"},
		{dir, "configuration file: not a regular file"},
		{write("big.yaml", big), "configuration: larger than 1 MiB"},
		{write("invalid.yaml", "policy: {default_action: "+secret+"}\n"), "policy.default_action (line 1, column 26): expected allow or deny"},
	} {
		if _, err := Load(tc.path, nil); err == nil || err.Error() != tc.want {
			t.Fatalf("err=%v, want %q", err, tc.want)
		}
	}
}
