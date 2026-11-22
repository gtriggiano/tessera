package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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
		Server:   Server{Listen: ":9001", MaxConnections: 1024, MaxConcurrentStreams: 1024, DrainDelay: 5 * time.Second, ShutdownTimeout: 20 * time.Second},
		Admin:    Admin{Listen: ":9090"},
		Log:      Log{Level: "info", Format: "logfmt", Decisions: "deny"},
		ClientIP: ClientIP{From: "peer"},
		Policy:   Policy{Mode: "enforce", DefaultAction: "deny", DenyResponse: Response{Status: 403, Headers: map[string]string{}}, Rules: []Rule{}},
	}
	if c.Server != want.Server || c.Admin != want.Admin || c.Log != want.Log || c.ClientIP != want.ClientIP || !reflect.DeepEqual(c.Policy, want.Policy) {
		t.Fatalf("got %+v", *c)
	}
}

func TestEveryKey(t *testing.T) {
	c := mustParse(t, `
server:
  listen: 127.0.0.1:9101
  max_connections: 256
  max_concurrent_streams: 64
  max_connection_age: 30m
  drain_delay: 0s
  shutdown_timeout: 1m30s
admin:
  listen: "[::1]:9102"
log:
  level: debug
  format: json
  decisions: all
client_ip:
  from: x_forwarded_for
  trusted_hops: 2
policy:
  default_action: allow
`, nil)
	want := Config{
		Server:   Server{Listen: "127.0.0.1:9101", MaxConnections: 256, MaxConcurrentStreams: 64, MaxConnectionAge: 30 * time.Minute, ShutdownTimeout: 90 * time.Second},
		Admin:    Admin{Listen: "[::1]:9102"},
		Log:      Log{Level: "debug", Format: "json", Decisions: "all"},
		ClientIP: ClientIP{From: "x_forwarded_for", TrustedHops: 2},
		Policy:   Policy{Mode: "enforce", DefaultAction: "allow", DenyResponse: Response{Status: 403, Headers: map[string]string{}}, Rules: []Rule{}},
	}
	if c.Server != want.Server || c.Admin != want.Admin || c.Log != want.Log || c.ClientIP != want.ClientIP || !reflect.DeepEqual(c.Policy, want.Policy) {
		t.Fatalf("got %+v", *c)
	}
}

func TestClientIP(t *testing.T) {
	for _, tc := range []struct {
		yaml string
		want ClientIP
	}{
		{"client_ip: {}\n", ClientIP{From: "peer"}},
		{"client_ip: {from: peer}\n", ClientIP{From: "peer"}},
		{"client_ip: {from: header, header: x-envoy-external-address}\n", ClientIP{From: "header", Header: "x-envoy-external-address"}},
		{"client_ip: {from: header, header: X-Client-Address}\n", ClientIP{From: "header", Header: "X-Client-Address"}},
		{"client_ip: {from: header, header: \"!#$%&'*+-.^_`|~09az\"}\n", ClientIP{From: "header", Header: "!#$%&'*+-.^_`|~09az"}},
		{"client_ip: {from: x_forwarded_for, trusted_hops: 0}\n", ClientIP{From: "x_forwarded_for"}},
		{"client_ip: {from: x_forwarded_for, trusted_hops: 3}\n", ClientIP{From: "x_forwarded_for", TrustedHops: 3}},
	} {
		c := mustParse(t, "policy: {default_action: deny}\n"+tc.yaml, nil)
		if c.ClientIP != tc.want {
			t.Fatalf("%s: got %+v, want %+v", tc.yaml, c.ClientIP, tc.want)
		}
		// The printed configuration has the keys of the source only, and
		// reads back the same.
		printed, err := c.Redacted()
		if err != nil {
			t.Fatal(err)
		}
		if again := mustParse(t, string(printed), nil); again.ClientIP != tc.want {
			t.Fatalf("%s: printed\n%s\nreads back as %+v", tc.yaml, printed, again.ClientIP)
		}
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
		{"unknown nested key", "policy:\n  default_action: deny\n  routes: []\n", "policy.routes (line 3, column 3): unknown key"},
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
		{"log decisions", "policy: {default_action: deny}\nlog: {decisions: allow}\n", "log.decisions (line 2, column 18): expected none, deny or all"},
		{"listen without port", "policy: {default_action: deny}\nserver: {listen: " + secret + "}\n", "server.listen (line 2, column 18): expected host:port or unix: followed by a path"},
		{"listen port zero", "policy: {default_action: deny}\nserver: {listen: \":0\"}\n", "server.listen (line 2, column 18): expected host:port or unix: followed by a path"},
		{"listen port too large", "policy: {default_action: deny}\nserver: {listen: \":65536\"}\n", "server.listen (line 2, column 18): expected host:port or unix: followed by a path"},
		{"listen port leading zero", "policy: {default_action: deny}\nserver: {listen: \":09001\"}\n", "server.listen (line 2, column 18): expected host:port or unix: followed by a path"},
		{"listen named port", "policy: {default_action: deny}\nserver: {listen: \"localhost:http\"}\n", "server.listen (line 2, column 18): expected host:port or unix: followed by a path"},
		{"listen numeric host", "policy: {default_action: deny}\nserver: {listen: \"127.1:9001\"}\n", "server.listen (line 2, column 18): expected host:port or unix: followed by a path"},
		{"listen bad host", "policy: {default_action: deny}\nserver: {listen: \"-bad-.example.com:9001\"}\n", "server.listen (line 2, column 18): expected host:port or unix: followed by a path"},
		{"empty socket path", "policy: {default_action: deny}\nserver: {listen: \"unix:\"}\n", "server.listen (line 2, column 18): expected host:port or unix: followed by a path"},
		{"control character in socket path", "policy: {default_action: deny}\nserver: {listen: \"unix:/run/a\\nb\"}\n", "server.listen (line 2, column 18): expected host:port or unix: followed by a path"},
		{"abstract socket", "policy: {default_action: deny}\nserver: {listen: \"unix:@tessera\"}\n", "server.listen (line 2, column 18): abstract Unix sockets are not supported; write a path"},
		{"admin listen socket", "policy: {default_action: deny}\nadmin: {listen: \"unix:/run/admin.sock\"}\n", "admin.listen (line 2, column 17): expected host:port"},
		{"same port", "policy: {default_action: deny}\nserver: {listen: \"127.0.0.1:9000\"}\nadmin: {listen: \"localhost:9000\"}\n", "admin.listen (line 3, column 17): must not share its address and port with server.listen"},
		{"same port as default admin", "policy: {default_action: deny}\nserver: {listen: \"192.0.2.10:9090\"}\n", "server.listen (line 2, column 18): must not share its address and port with admin.listen"},
		{"connections zero", "policy: {default_action: deny}\nserver: {max_connections: 0}\n", "server.max_connections (line 2, column 27): expected an integer from 1 to 65536"},
		{"connections too many", "policy: {default_action: deny}\nserver: {max_connections: 65537}\n", "server.max_connections (line 2, column 27): expected an integer from 1 to 65536"},
		{"streams zero", "policy: {default_action: deny}\nserver: {max_concurrent_streams: 0}\n", "server.max_concurrent_streams (line 2, column 34): expected an integer from 1 to 4294967295"},
		{"streams too many", "policy: {default_action: deny}\nserver: {max_concurrent_streams: 4294967296}\n", "server.max_concurrent_streams (line 2, column 34): expected an integer from 1 to 4294967295"},
		{"negative connection age", "policy: {default_action: deny}\nserver: {max_connection_age: -1s}\n", "server.max_connection_age (line 2, column 30): must not be negative"},
		{"negative drain delay", "policy: {default_action: deny}\nserver: {drain_delay: -1s}\n", "server.drain_delay (line 2, column 23): must not be negative"},
		{"zero shutdown timeout", "policy: {default_action: deny}\nserver: {shutdown_timeout: 0s}\n", "server.shutdown_timeout (line 2, column 28): must be positive"},
		{"client address source", "policy: {default_action: deny}\nclient_ip: {from: " + secret + "}\n", "client_ip.from (line 2, column 19): expected peer, header or x_forwarded_for"},
		{"client address source case", "policy: {default_action: deny}\nclient_ip: {from: Peer}\n", "client_ip.from (line 2, column 19): expected peer, header or x_forwarded_for"},
		{"header without its name", "policy: {default_action: deny}\nclient_ip: {from: header}\n", "client_ip.header (line 2, column 12): required when client_ip.from is header"},
		{"empty header name", "policy: {default_action: deny}\nclient_ip: {from: header, header: \"\"}\n", "client_ip.header (line 2, column 35): expected a header name"},
		{"header name with a space", "policy: {default_action: deny}\nclient_ip: {from: header, header: \"x-" + secret + " a\"}\n", "client_ip.header (line 2, column 35): expected a header name"},
		{"pseudo-header", "policy: {default_action: deny}\nclient_ip: {from: header, header: \":authority\"}\n", "client_ip.header (line 2, column 35): expected a header name"},
		{"header with the peer", "policy: {default_action: deny}\nclient_ip: {header: x-real-ip}\n", "client_ip.header (line 2, column 21): allowed only when client_ip.from is header"},
		{"header with X-Forwarded-For", "policy: {default_action: deny}\nclient_ip: {from: x_forwarded_for, trusted_hops: 1, header: x-real-ip}\n", "client_ip.header (line 2, column 61): allowed only when client_ip.from is header"},
		{"X-Forwarded-For without its hops", "policy: {default_action: deny}\nclient_ip: {from: x_forwarded_for}\n", "client_ip.trusted_hops (line 2, column 12): required when client_ip.from is x_forwarded_for"},
		{"negative hops", "policy: {default_action: deny}\nclient_ip: {from: x_forwarded_for, trusted_hops: -1}\n", "client_ip.trusted_hops (line 2, column 50): must not be negative"},
		{"hops with the peer", "policy: {default_action: deny}\nclient_ip: {from: peer, trusted_hops: 0}\n", "client_ip.trusted_hops (line 2, column 39): allowed only when client_ip.from is x_forwarded_for"},
		{"hops with a header", "policy: {default_action: deny}\nclient_ip: {from: header, header: x-real-ip, trusted_hops: 1}\n", "client_ip.trusted_hops (line 2, column 60): allowed only when client_ip.from is x_forwarded_for"},
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
  max_connections: 1024
  max_concurrent_streams: 1024
  max_connection_age: 0s
  drain_delay: 5s
  shutdown_timeout: 20s
admin:
  listen: :9090
log:
  level: debug
  format: '[REDACTED]'
  decisions: deny
client_ip:
  from: peer
policy:
  mode: enforce
  default_action: '[REDACTED]'
  deny_response:
    status: 403
    body: ""
    headers: {}
  rules: []
`
	if string(got) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	// The printed configuration is itself valid, and reads back the same.
	printed := strings.NewReplacer("format: '[REDACTED]'", "format: json", "default_action: '[REDACTED]'", "default_action: deny").Replace(string(got))
	again := mustParse(t, printed, nil)
	if again.Server != c.Server || again.Admin != c.Admin || again.Log != c.Log || again.ClientIP != c.ClientIP || !reflect.DeepEqual(again.Policy, c.Policy) {
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
	// A socket path is checked once it is absolute.
	longest := "/" + strings.Repeat("s", MaxSocketPathBytes-1)
	if c, err := Load(write("longest.yaml", "policy: {default_action: deny}\nserver: {listen: \"unix:"+longest+"\"}\n"), nil); err != nil || c.Server.Listen != "unix:"+longest {
		t.Fatalf("socket path of %d bytes: listen=%q err=%v", len(longest), c.Server.Listen, err)
	}
	tooLong := fmt.Sprintf("server.listen (line 2, column 18): the socket path, made absolute, is longer than %d bytes", MaxSocketPathBytes)
	if _, err := Load(write("too-long.yaml", "policy: {default_action: deny}\nserver: {listen: \"unix:"+longest+"s\"}\n"), nil); err == nil || err.Error() != tooLong {
		t.Fatalf("socket path of %d bytes: err=%v", len(longest)+1, err)
	}
	relative := strings.Repeat("s", MaxSocketPathBytes-len(dir))
	if _, err := Load(write("relative-too-long.yaml", "policy: {default_action: deny}\nserver: {listen: \"unix:"+relative+"\"}\n"), nil); err == nil || err.Error() != tooLong {
		t.Fatalf("relative socket path of %d bytes: err=%v", len(relative), err)
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

const fullPolicy = `
policy:
  mode: audit
  default_action: deny
  deny_response:
    status: 451
    body: Not available.
    headers:
      Content-Type: text/plain; charset=utf-8
      cache-control: no-store
  rules:
    - name: blocked-networks
      description: Networks blocked during an incident.
      action: deny
      when: client.ip.inCidr("198.51.100.0/24")
    - name: admin
      action: deny
      when: |
        request.path.startsWith("/admin")
          && !client.ip.inCidr("192.0.2.0/24")
      response:
        status: 404
    - name: bots
      action: deny
      mode: audit
      when: request.headers["user-agent"].contains("bot")
      response:
        body: ""
        headers: {}
    - name: office
      action: allow
      when: has(client.ip) && client.ip.inCidr("192.0.2.0/24")
    - name: everyone-else
      action: allow
`

func TestPolicy(t *testing.T) {
	c := mustParse(t, fullPolicy, nil)
	inherited := Response{Status: 451, Body: "Not available.", Headers: map[string]string{"Content-Type": "text/plain; charset=utf-8", "cache-control": "no-store"}}
	want := Policy{
		Mode:          "audit",
		DefaultAction: "deny",
		DenyResponse:  inherited,
		Rules: []Rule{
			{Name: "blocked-networks", Description: "Networks blocked during an incident.", When: `client.ip.inCidr("198.51.100.0/24")`, Action: "deny", Mode: "enforce", Response: inherited},
			{Name: "admin", When: "request.path.startsWith(\"/admin\")\n  && !client.ip.inCidr(\"192.0.2.0/24\")\n", Action: "deny", Mode: "enforce", Response: Response{Status: 404, Body: inherited.Body, Headers: inherited.Headers}},
			{Name: "bots", When: `request.headers["user-agent"].contains("bot")`, Action: "deny", Mode: "audit", Response: Response{Status: 451, Body: "", Headers: map[string]string{}}},
			{Name: "office", When: `has(client.ip) && client.ip.inCidr("192.0.2.0/24")`, Action: "allow", Mode: "enforce"},
			{Name: "everyone-else", When: "true", Action: "allow", Mode: "enforce"},
		},
	}
	got := c.Policy
	for i := range got.Rules {
		got.Rules[i].written = want.Rules[i].written
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

func TestInvalidPolicies(t *testing.T) {
	const deny = "policy:\n  default_action: deny\n"
	rule := func(lines string) string { return deny + "  rules:\n    - name: r\n      action: deny\n" + lines }
	for _, tc := range []struct{ name, yaml, want string }{
		{"mode", "policy: {default_action: deny, mode: Audit}\n", "policy.mode (line 1, column 38): expected enforce or audit"},
		{"status too low", deny + "  deny_response: {status: 399}\n", "policy.deny_response.status (line 3, column 27): expected an integer from 400 to 599"},
		{"status too high", deny + "  deny_response: {status: 600}\n", "policy.deny_response.status (line 3, column 27): expected an integer from 400 to 599"},
		{"body too long", deny + "  deny_response: {body: " + strings.Repeat("a", MaxBodyBytes+1) + "}\n", "policy.deny_response.body (line 3, column 25): longer than 8192 bytes"},
		{"headers not a mapping", deny + "  deny_response: {headers: [a]}\n", "policy.deny_response.headers (line 3, column 28): expected a mapping"},
		{"header value not a string", deny + "  deny_response:\n    headers:\n      x-a: 1\n", "policy.deny_response.headers (line 5, column 12): expected a string"},
		{"header value null", deny + "  deny_response:\n    headers:\n      x-a:\n", "policy.deny_response.headers (line 5, column 11): null is not allowed"},
		{"header name", deny + "  deny_response:\n    headers:\n      \"x " + secret + "\": a\n", "policy.deny_response.headers (line 5, column 7): expected a header name"},
		{"duplicate header", deny + "  deny_response:\n    headers:\n      x-a: a\n      x-a: b\n", "policy.deny_response.headers (line 6, column 7): duplicate key"},
		{"header in two cases", deny + "  deny_response:\n    headers:\n      x-a: a\n      X-A: b\n", "policy.deny_response.headers (line 6, column 7): the same header twice, in different cases"},
		{"forbidden header", deny + "  deny_response:\n    headers:\n      Content-Length: \"3\"\n", "policy.deny_response.headers (line 5, column 7): this header is not allowed: Envoy sets it, or it describes the connection"},
		{"header value", deny + "  deny_response:\n    headers:\n      x-a: \"a\\nb\"\n", "policy.deny_response.headers (line 5, column 7): the value of this header has a control character"},
		{"rules not a list", deny + "  rules: {}\n", "policy.rules (line 3, column 10): expected a list"},
		{"rule not a mapping", deny + "  rules: [r]\n", "policy.rules[0] (line 3, column 11): expected a mapping"},
		{"unknown rule key", rule("      then: deny\n"), "policy.rules[0].then (line 6, column 7): unknown key"},
		{"missing name", deny + "  rules:\n    - action: deny\n", "policy.rules[0].name (line 4, column 7): required"},
		{"name", deny + "  rules:\n    - name: Office\n      action: deny\n", "policy.rules[0].name (line 4, column 13): expected lowercase letters, digits and hyphens, at most 63 characters"},
		{"long name", deny + "  rules:\n    - name: " + strings.Repeat("a", 64) + "\n      action: deny\n", "policy.rules[0].name (line 4, column 13): expected lowercase letters, digits and hyphens, at most 63 characters"},
		{"reserved name", deny + "  rules:\n    - name: default\n      action: deny\n", "policy.rules[0].name (line 4, column 13): default is reserved for the decision of policy.default_action"},
		{"repeated name", rule("      when: request.host == \"a.example.com\"\n    - name: r\n      action: allow\n"), "policy.rules[1].name (line 7, column 13): the name of policy.rules[0] is repeated"},
		{"description", deny + "  rules:\n    - name: r\n      description: " + strings.Repeat("a", MaxDescriptionBytes+1) + "\n      action: deny\n", "policy.rules[0].description (line 5, column 20): longer than 1024 bytes"},
		{"missing action", deny + "  rules:\n    - name: r\n", "policy.rules[0].action (line 4, column 7): required"},
		{"action", deny + "  rules:\n    - name: r\n      action: block\n", "policy.rules[0].action (line 5, column 15): expected allow or deny"},
		{"rule mode", rule("      mode: observe\n"), "policy.rules[0].mode (line 6, column 13): expected enforce or audit"},
		{"response on an allow rule", deny + "  rules:\n    - name: r\n      action: allow\n      response: {status: 403}\n", "policy.rules[0].response (line 6, column 17): allowed only when action is deny"},
		{"rule status", rule("      response: {status: 302}\n"), "policy.rules[0].response.status (line 6, column 26): expected an integer from 400 to 599"},
		{"rule header", rule("      response:\n        headers: {connection: close}\n"), "policy.rules[0].response.headers (line 7, column 19): this header is not allowed: Envoy sets it, or it describes the connection"},
		{"empty when", rule("      when: \"\"\n"), "policy.rules[0].when (line 6, column 14): the expression is empty"},
		{"when not a string", rule("      when: true\n"), "policy.rules[0].when (line 6, column 13): expected a string"},
		{"unknown attribute", rule("      when: request.hots == \"a.example.com\"\n"), "policy.rules[0].when (line 6, column 20): undefined field 'hots'"},
		{"wrong type", rule("      when: request.host == 1\n"), "policy.rules[0].when (line 6, column 26): found no matching overload for '_==_' applied to '(string, int)'"},
		{"not a bool", rule("      when: size(request.path)\n"), "policy.rules[0].when (line 6, column 13): the condition must be a bool, not int"},
		{"invalid network", rule("      when: client.ip.inCidr(\"192.0.2.1/24\")\n"), "policy.rules[0].when (line 6, column 30): invalid network: the network has bits set after the prefix length: write its first address, such as 192.0.2.0/24"},
		{"too many rules", deny + "  rules:\n" + strings.Repeat("    - {name: r, action: deny, when: \"false\"}\n", MaxRules+1), "policy.rules (line 4, column 5): more than 1000 rules"},
		{"unreachable", deny + "  rules:\n    - name: a\n      action: deny\n    - name: b\n      action: allow\n", "policy.rules[0] (line 4, column 7): the rule matches every request, so the rules after it are never evaluated: give it a condition, or set its mode to audit"},
		{"unreachable after true", deny + "  rules:\n    - name: a\n      action: allow\n      when: \"true\"\n    - name: b\n      action: allow\n", "policy.rules[0].when (line 6, column 13): the rule matches every request, so the rules after it are never evaluated: give it a condition, or set its mode to audit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parse(t, tc.yaml, nil)
			if err == nil {
				t.Fatal("Parse succeeded")
			}
			if err.Error() != tc.want {
				t.Fatalf("error %q\nwant  %q", err, tc.want)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatal("the error repeats a configured value")
			}
		})
	}
	// A rule that matches every request is allowed last, and in audit mode.
	for _, yaml := range []string{
		deny + "  rules:\n    - name: a\n      action: deny\n      when: request.host == \"a.example.com\"\n    - name: b\n      action: allow\n",
		deny + "  rules:\n    - name: a\n      action: deny\n      mode: audit\n    - name: b\n      action: allow\n",
	} {
		mustParse(t, yaml, nil)
	}
}

// An error in an expression is placed where the file writes it, when the
// file holds the expression as it is.
func TestExpressionErrorPositions(t *testing.T) {
	const head = "policy:\n  default_action: deny\n  rules:\n    - name: r\n      action: deny\n"
	for _, tc := range []struct{ name, when, want string }{
		{"plain", "      when: request.hots == \"x\"\n", "(line 6, column 20): undefined field 'hots'"},
		{"single-quoted", "      when: 'request.hots == \"x\"'\n", "(line 6, column 21): undefined field 'hots'"},
		{"double-quoted", "      when: \"request.hots == 'x'\"\n", "(line 6, column 21): undefined field 'hots'"},
		{"characters before", "      when: request.host == \"é🙂\" || request.hots == \"x\"\n", "(line 6, column 44): undefined field 'hots'"},
		{"literal block", "      when: |\n        request.host == \"x\" &&\n          request.hots == \"x\"\n", "(line 8, column 18): undefined field 'hots'"},
		{"literal block, first line", "      when: |-\n        request.hots == \"x\"\n", "(line 7, column 16): undefined field 'hots'"},
		// The text in the file is not the expression: the position of the
		// value, and the position in the expression.
		{"escape", "      when: \"request.hots == \\\"x\\\"\"\n", "(line 6, column 13): undefined field 'hots' (line 1, column 8 of the expression)"},
		{"folded block", "      when: >\n        request.host == \"x\" &&\n        request.hots == \"x\"\n", "(line 6, column 13): undefined field 'hots' (line 1, column 31 of the expression)"},
		{"plain on two lines", "      when: request.host == \"x\" &&\n        request.hots == \"x\"\n", "(line 6, column 13): undefined field 'hots' (line 1, column 31 of the expression)"},
	} {
		_, err := parse(t, head+tc.when, nil)
		if want := "policy.rules[0].when " + tc.want; err == nil || err.Error() != want {
			t.Errorf("%s: error %v\nwant %s", tc.name, err, want)
		}
	}
	// An expression with a substitution: nothing from it in the message.
	_, err := parse(t, head+"      when: request.headers[\"x-key\"] == ${KEY} &&\n", map[string]string{"KEY": secret})
	if err == nil || err.Error() != "policy.rules[0].when (line 6, column 13): invalid expression" {
		t.Errorf("substitution: error %v", err)
	}
}

func TestRedactedPolicy(t *testing.T) {
	c := mustParse(t, `
policy:
  default_action: deny
  deny_response:
    body: ${BODY}
    headers:
      x-token: ${TOKEN}
  rules:
    - name: admin
      action: deny
      when: request.path.startsWith("/admin")
      response:
        status: 404
        headers:
          x-reason: ${REASON:-admin}
    - name: office
      action: allow
      when: client.ip.inCidr("192.0.2.0/24")
    - name: rest
      action: deny
`, map[string]string{"BODY": "denied", "TOKEN": secret})
	got, err := c.Redacted()
	if err != nil {
		t.Fatal(err)
	}
	want := `policy:
  mode: enforce
  default_action: deny
  deny_response:
    status: 403
    body: '[REDACTED]'
    headers:
      x-token: '[REDACTED]'
  rules:
    - name: admin
      description: ""
      action: deny
      mode: enforce
      when: request.path.startsWith("/admin")
      response:
        status: 404
        headers:
          x-reason: '[REDACTED]'
    - name: office
      description: ""
      action: allow
      mode: enforce
      when: client.ip.inCidr("192.0.2.0/24")
    - name: rest
      description: ""
      action: deny
      mode: enforce
      when: "true"
`
	if _, policy, _ := strings.Cut(string(got), "\npolicy:\n"); "policy:\n"+policy != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if strings.Contains(string(got), secret) {
		t.Fatal("a substituted value is printed")
	}
	// Once its redacted values are written again, the printed configuration
	// reads back the same.
	printed := strings.NewReplacer("body: '[REDACTED]'", "body: denied", "x-token: '[REDACTED]'", "x-token: "+secret, "x-reason: '[REDACTED]'", "x-reason: admin").Replace(string(got))
	again := mustParse(t, printed, nil)
	if !reflect.DeepEqual(again.Policy, c.Policy) {
		t.Fatalf("reads back as %+v\nwant %+v", again.Policy, c.Policy)
	}
}

// The order of the keys of a rule is a convention of the examples: the file
// may write them in any order.
func TestRuleKeysInAnyOrder(t *testing.T) {
	canonical := mustParse(t, `
policy:
  default_action: allow
  rules:
    - name: admin
      description: The admin pages.
      action: deny
      mode: audit
      when: request.path.startsWith("/admin")
      response:
        status: 404
        body: Not found.
        headers: {x-reason: admin}
`, nil)
	reversed := mustParse(t, `
policy:
  default_action: allow
  rules:
    - response:
        headers: {x-reason: admin}
        body: Not found.
        status: 404
      when: request.path.startsWith("/admin")
      mode: audit
      action: deny
      description: The admin pages.
      name: admin
`, nil)
	if !reflect.DeepEqual(canonical.Policy, reversed.Policy) {
		t.Fatalf("in another order: %+v\nwant %+v", reversed.Policy, canonical.Policy)
	}
}
