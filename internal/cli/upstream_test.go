package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/gtriggiano/tessera/internal/mmdbtest"
)

// upstreamConfig denies AS 64500 and allows the others, and declares
// upstream headers and decision fields.
const upstreamConfig = `enrichment:
  geo:
    database: geo.mmdb
  asn:
    database: asn.mmdb
log:
  decision_fields:
    agent: request.headers["user-agent"]
policy:
  mode: %s
  default_action: allow
  rules:
    - name: hosting
      action: deny
      when: asn.number == 64500
upstream:
  headers:
    X-Client-Country: geo.country.code
    x-client-asn: 'asn.found ? string(asn.number) : ""'
    x-agent-length: string(size(request.headers["user-agent"]))
`

// The upstream headers of a request that Envoy sends on, in a block of their
// own after the decision: a value, or removed; none for a denial.
func TestEvalUpstream(t *testing.T) {
	enforce := writeDatabases(t, fmt.Sprintf(upstreamConfig, "enforce"), mmdbtest.CityDatabase(6), mmdbtest.ASNDatabase())
	audit := writeDatabases(t, fmt.Sprintf(upstreamConfig, "audit"), mmdbtest.CityDatabase(6), mmdbtest.ASNDatabase())
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"allowed", []string{"--config", enforce, "--ip", "192.0.2.7", "--header", "user-agent: curl/8.7.1"}, `asn.number  64496

hosting  false

decision: allow (rule default, reason default)

x-agent-length    "10"
x-client-asn      "64496"
x-client-country  "IT"
`},
		// An empty value and a failure both remove the header.
		{"allowed, headers removed", []string{"--config", enforce, "--ip", "203.0.113.200"}, `asn.number  0

hosting  false

decision: allow (rule default, reason default)

x-agent-length    removed
x-client-asn      removed
x-client-country  removed
`},
		{"denied", []string{"--config", enforce, "--ip", "198.51.100.7"}, `asn.number  64500

hosting  true -> deny

decision: deny (rule hosting, reason rule)
response: status 403
`},
		{"denied in audit mode", []string{"--config", audit, "--ip", "198.51.100.7"}, `asn.number  64500

hosting  true -> deny

decision: deny (rule hosting, reason rule), not enforced: policy.mode is audit
response: status 403

x-agent-length    removed
x-client-asn      "64500"
x-client-country  "US"
`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errOut := execute(append([]string{"eval"}, tc.args...))
			if code != 0 || errOut != "" || out != tc.want {
				t.Fatalf("code=%d stderr=%q stdout:\n%s\nwant:\n%s", code, errOut, out, tc.want)
			}
		})
	}

	// In JSON: the field upstream, with set and remove, only for a request
	// that Envoy sends on.
	for _, tc := range []struct {
		name, ip string
		want     string
	}{
		{"allowed", "203.0.113.7", `{"set":{"x-client-country":"JP"},"remove":["x-agent-length","x-client-asn"]}`},
		{"every header set", "192.0.2.7", `{"set":{"x-agent-length":"0","x-client-asn":"64496","x-client-country":"IT"},"remove":[]}`},
		{"denied", "198.51.100.7", ``},
	} {
		args := []string{"eval", "--config", enforce, "--ip", tc.ip, "--output", "json"}
		if tc.ip == "192.0.2.7" {
			args = append(args, "--header", "user-agent:")
		}
		code, out, errOut := execute(args)
		var got struct {
			Upstream json.RawMessage `json:"upstream"`
		}
		if code != 0 || errOut != "" || json.Unmarshal([]byte(out), &got) != nil || string(got.Upstream) != tc.want {
			t.Errorf("%s: code=%d stderr=%q upstream %s, want %s", tc.name, code, errOut, got.Upstream, tc.want)
		}
	}

	// Without upstream headers, no block and no field.
	plain := writeDatabases(t, databasesConfig, mmdbtest.CityDatabase(6), mmdbtest.ASNDatabase())
	code, out, _ := execute([]string{"eval", "--config", plain, "--ip", "192.0.2.7", "--output", "json"})
	var got map[string]any
	if code != 0 || json.Unmarshal([]byte(out), &got) != nil || got["upstream"] != nil {
		t.Errorf("without upstream headers: %s", out)
	}
}

// Upstream headers and decision fields are uses of the sets and the
// databases; their errors stop the configuration at their position.
func TestConfigValidateUpstream(t *testing.T) {
	path := writeDatabases(t, `enrichment:
  geo: {database: geo.mmdb}
  asn: {database: asn.mmdb}
sets:
  partners: {type: string, values: [api.example.com]}
log:
  decision_fields:
    country: geo.country.code
upstream:
  headers:
    x-partner: 'sets.partners.contains(request.host) ? "yes" : ""'
policy:
  default_action: deny
`, mmdbtest.CityDatabase(6), mmdbtest.ASNDatabase())
	code, out, errOut := execute([]string{"config", "validate", "--config", path})
	if code != 0 || out != "configuration valid\n" || errOut != "tessera: warning: enrichment.asn (line 3, column 3): the database is not used by any rule, upstream header or decision field\n" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	for _, tc := range []struct{ yaml, want string }{
		{"upstream:\n  headers:\n    x-a: request.host == \"x\"\npolicy: {default_action: deny}\n", "tessera: upstream.headers (line 3, column 10): the expression must be a string, not bool\n"},
		{"upstream:\n  headers:\n    X-Envoy-Peer: request.host\npolicy: {default_action: deny}\n", "tessera: upstream.headers (line 3, column 5): this header is not allowed: it changes the meaning of the request, holds its credentials, or belongs to Envoy\n"},
		{"log:\n  decision_fields:\n    q: request.query\npolicy: {default_action: deny}\n", "tessera: log.decision_fields (line 3, column 8): a decision field cannot read request.query\n"},
		{"log:\n  decision_fields:\n    rule: request.host\npolicy: {default_action: deny}\n", "tessera: log.decision_fields (line 3, column 5): the decision event already has a field of this name\n"},
	} {
		code, out, errOut := execute([]string{"config", "validate", "--config", writeConfig(t, tc.yaml)})
		if code != 2 || out != "" || errOut != tc.want {
			t.Errorf("code=%d stdout=%q stderr=%q, want %q", code, out, errOut, tc.want)
		}
	}
}

// eval redacts the value of an upstream header that can hold the value of
// authorization or cookie, in the text and in JSON; a removed header stays
// removed, and the other values stay in clear.
func TestEvalRedactsSecretHeaders(t *testing.T) {
	path := writeConfig(t, `policy:
  default_action: allow
upstream:
  headers:
    x-auth: request.headers["authorization"]
    x-session: request.headers.cookie
    x-forwarded-value: request.headers[request.headers["x-source"]]
    x-agent: request.headers["user-agent"]
`)
	args := []string{"eval", "--config", path, "--header", "authorization: Bearer s3cr3t", "--header", "cookie: s=s3cr3t",
		"--header", "x-source: cookie", "--header", "user-agent: curl/8.7.1"}
	code, out, errOut := execute(args)
	want := `decision: allow (rule default, reason default)

x-agent            "curl/8.7.1"
x-auth             [REDACTED]
x-forwarded-value  [REDACTED]
x-session          [REDACTED]
`
	if code != 0 || errOut != "" || out != want {
		t.Fatalf("code=%d stderr=%q stdout:\n%s\nwant:\n%s", code, errOut, out, want)
	}
	code, out, _ = execute(append(args, "--output", "json"))
	var got struct {
		Upstream json.RawMessage `json:"upstream"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &got) != nil || string(got.Upstream) != `{"set":{"x-agent":"curl/8.7.1","x-auth":"[REDACTED]","x-forwarded-value":"[REDACTED]","x-session":"[REDACTED]"},"remove":[]}` {
		t.Fatalf("code=%d upstream %s", code, got.Upstream)
	}
	if strings.Contains(out, "s3cr3t") {
		t.Fatal("the JSON output holds a secret")
	}
	// Without the headers, the values are removed, and shown so.
	code, out, _ = execute([]string{"eval", "--config", path, "--header", "user-agent: curl/8.7.1"})
	want = `decision: allow (rule default, reason default)

x-agent            "curl/8.7.1"
x-auth             removed
x-forwarded-value  removed
x-session          removed
`
	if code != 0 || out != want {
		t.Fatalf("code=%d stdout:\n%s\nwant:\n%s", code, out, want)
	}
	code, out, _ = execute([]string{"eval", "--config", path, "--output", "json"})
	if code != 0 || !strings.Contains(out, `"upstream":{"set":{},"remove":["x-agent","x-auth","x-forwarded-value","x-session"]}`) {
		t.Fatalf("code=%d stdout=%s", code, out)
	}
}

// The secret headers, proxy-authorization and those of secret_headers, are
// redacted among the attributes and in the upstream headers that read them,
// in the text and in JSON.
func TestEvalRedactsConfiguredSecretHeaders(t *testing.T) {
	path := writeConfig(t, `secret_headers: [X-Api-Key]
policy:
  default_action: deny
  rules:
    - name: clients
      action: allow
      when: |
        request.headers["proxy-authorization"] != "" && request.headers["x-api-key"] != ""
          && request.headers["x-api-key-id"] != ""
upstream:
  headers:
    x-proxy: request.headers["proxy-authorization"]
    x-key: request.headers["x-api-key"]
    x-key-id: request.headers["x-api-key-id"]
`)
	args := []string{"eval", "--config", path, "--header", "proxy-authorization: Basic s3cr3t", "--header", "x-api-key: k-s3cr3t", "--header", "x-api-key-id: 42"}
	code, out, errOut := execute(args)
	want := `request.headers["proxy-authorization"]  [REDACTED]
request.headers["x-api-key"]            [REDACTED]
request.headers["x-api-key-id"]         "42"

clients  true -> allow

decision: allow (rule clients, reason rule)

x-key     [REDACTED]
x-key-id  "42"
x-proxy   [REDACTED]
`
	if code != 0 || errOut != "" || out != want {
		t.Fatalf("code=%d stderr=%q stdout:\n%s\nwant:\n%s", code, errOut, out, want)
	}
	code, out, _ = execute(append(args, "--output", "json"))
	if code != 0 || strings.Contains(out, "s3cr3t") ||
		!strings.Contains(out, `{"name":"request.headers[\"proxy-authorization\"]","value":"[REDACTED]"},{"name":"request.headers[\"x-api-key\"]","value":"[REDACTED]"},{"name":"request.headers[\"x-api-key-id\"]","value":"42"}`) ||
		!strings.Contains(out, `"upstream":{"set":{"x-key":"[REDACTED]","x-key-id":"42","x-proxy":"[REDACTED]"},"remove":[]}`) {
		t.Fatalf("code=%d stdout=%s", code, out)
	}
	// A rule that reads every header shows them all, the secret ones
	// redacted.
	every := writeConfig(t, "secret_headers: [x-api-key]\npolicy:\n  default_action: deny\n  rules:\n    - name: any\n      action: allow\n      when: request.headers.exists(h, h.startsWith(\"x-\"))\n")
	code, out, _ = execute([]string{"eval", "--config", every, "--header", "x-api-key: k-s3cr3t", "--header", "proxy-authorization: Basic s3cr3t", "--header", "x-tag: blue"})
	if code != 0 || strings.Contains(out, "s3cr3t") || !strings.Contains(out, "request.headers[\"x-tag\"]                \"blue\"\n") {
		t.Fatalf("code=%d stdout:\n%s", code, out)
	}
}

func TestConfigValidateSecretHeaders(t *testing.T) {
	for _, tc := range []struct{ yaml, want string }{
		{"secret_headers: [Proxy-Authorization]\npolicy: {default_action: deny}\n", "tessera: secret_headers[0] (line 1, column 18): this header is always secret\n"},
		{"secret_headers:\n  - x-api-key\n  - \"x s3cr3t\"\npolicy: {default_action: deny}\n", "tessera: secret_headers[1] (line 3, column 5): expected a header name\n"},
		{"secret_headers: [x-api-key]\nlog:\n  decision_fields:\n    key: request.headers[\"X-Api-Key\"]\npolicy: {default_action: deny}\n", "tessera: log.decision_fields (line 4, column 10): a decision field cannot read a secret header\n"},
	} {
		code, out, errOut := execute([]string{"config", "validate", "--config", writeConfig(t, tc.yaml)})
		if code != 2 || out != "" || errOut != tc.want || strings.Contains(errOut, "s3cr3t") {
			t.Errorf("code=%d stdout=%q stderr=%q, want %q", code, out, errOut, tc.want)
		}
	}
	code, out, errOut := execute([]string{"config", "validate", "--config", writeConfig(t, "secret_headers: [x-api-key]\npolicy: {default_action: deny}\n")})
	if code != 0 || out != "configuration valid\n" || errOut != "" {
		t.Errorf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
}
