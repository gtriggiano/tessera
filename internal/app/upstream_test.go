package app

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"

	"github.com/gtriggiano/tessera/internal/mmdbtest"
)

// upstreamConfig reads the example databases from dir, denies 198.51.100.0/24
// and allows the others, declares upstream headers and decision fields, and
// logs every decision in format.
func upstreamConfig(dir, format string) string {
	return `server:
  listen: ` + grpcPlaceholder + `
  drain_delay: 0s
admin:
  listen: ` + adminPlaceholder + `
log:
  format: ` + format + `
  decisions: all
  decision_fields:
    country: geo.country.code
    agent: request.headers["user-agent"]
client_ip:
  from: x_forwarded_for
  trusted_hops: 0
enrichment:
  geo:
    database: ` + filepath.Join(dir, "geo.mmdb") + `
  asn:
    database: ` + filepath.Join(dir, "asn.mmdb") + `
policy:
  default_action: allow
  rules:
    - name: blocked
      action: deny
      when: has(client.ip) && client.ip.inCidr("198.51.100.0/24")
upstream:
  headers:
    X-Client-Country: geo.country.code
    x-client-asn: 'asn.found ? string(asn.number) : ""'
    x-client-city: geo.city
    x-tag: request.headers["x-tag"]
`
}

func writeExampleDatabases(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeDatabase(t, filepath.Join(dir, "geo.mmdb"), mmdbtest.CityDatabase(6))
	writeDatabase(t, filepath.Join(dir, "asn.mmdb"), mmdbtest.ASNDatabase())
	return dir
}

// checkWith decides a request with the headers of a client.
func checkWith(t *testing.T, conn *grpc.ClientConn, headers map[string]string) *authv3.CheckResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := authv3.NewAuthorizationClient(conn).Check(ctx, &authv3.CheckRequest{Attributes: &authv3.AttributeContext{
		Request: &authv3.AttributeContext_Request{Http: &authv3.AttributeContext_HttpRequest{
			Method: "GET", Host: "api.example.com", Path: "/", Headers: headers,
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// upstreamHeaders returns the headers that resp sets, as name=value, and the
// names that it removes; it fails on a header that does not replace the one
// of the client with both fields: append false, and append_action
// OVERWRITE_IF_EXISTS_OR_ADD.
func upstreamHeaders(t *testing.T, resp *authv3.CheckResponse) (set, removed []string) {
	t.Helper()
	ok := resp.GetOkResponse()
	for _, h := range ok.GetHeaders() {
		if h.GetAppend() == nil || h.GetAppend().GetValue() || h.GetAppendAction() != corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD {
			t.Errorf("header %s does not replace the one of the client: %v", h.GetHeader().GetKey(), h)
		}
		set = append(set, h.GetHeader().GetKey()+"="+h.GetHeader().GetValue())
	}
	return set, ok.GetHeadersToRemove()
}

// The upstream headers of a request that tessera allows replace those of
// the client, or remove them; a denial has none; a header that fails is
// counted, and the decision does not change. The decision events carry the
// decision fields after duration_ms.
func TestServeUpstreamHeadersAndDecisionFields(t *testing.T) {
	h := launch(t, upstreamConfig(writeExampleDatabases(t), "logfmt"), Options{})
	grpcAddress, adminAddress := h.addresses()
	metrics := scrape(t, adminAddress)
	for _, header := range []string{"x-client-country", "x-client-asn", "x-client-city", "x-tag"} {
		for _, cause := range []string{"client_ip_missing", "expression", "panic", "set_unavailable", "database_error"} {
			if v, ok := metrics[`tessera_upstream_header_errors_total{cause="`+cause+`",header="`+header+`"}`]; !ok || v != 0 {
				t.Errorf("series of %s, %s at startup: %v, %v", header, cause, v, ok)
			}
		}
	}

	conn := dialGRPC(t, grpcAddress)
	client := map[string]string{"x-client-country": "XX", "x-client-asn": "64511", "x-client-city": "Nowhere", "user-agent": "curl/8.7.1"}
	with := func(more map[string]string) map[string]string {
		out := map[string]string{}
		for k, v := range client {
			out[k] = v
		}
		for k, v := range more {
			out[k] = v
		}
		return out
	}
	for _, tc := range []struct {
		name         string
		headers      map[string]string
		code         codes.Code
		set, removed []string
	}{
		{"allowed, in the databases", with(map[string]string{"x-forwarded-for": "192.0.2.7"}), codes.OK,
			[]string{"x-client-asn=64496", "x-client-city=Rome", "x-client-country=IT"}, []string{"x-tag"}},
		{"allowed, in no database", with(map[string]string{"x-forwarded-for": "203.0.113.200", "x-tag": "blue"}), codes.OK,
			[]string{"x-tag=blue"}, []string{"x-client-asn", "x-client-city", "x-client-country"}},
		{"allowed, without a client address", with(map[string]string{"x-tag": "a\x01b"}), codes.OK,
			nil, []string{"x-client-asn", "x-client-city", "x-client-country", "x-tag"}},
		{"denied", with(map[string]string{"x-forwarded-for": "198.51.100.7", "x-tag": "blue"}), codes.PermissionDenied, nil, nil},
	} {
		resp := checkWith(t, conn, tc.headers)
		if got := codes.Code(resp.GetStatus().GetCode()); got != tc.code {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.code)
		}
		set, removed := upstreamHeaders(t, resp)
		if !reflect.DeepEqual(set, tc.set) || !reflect.DeepEqual(removed, tc.removed) {
			t.Errorf("%s: sets %q and removes %q, want %q and %q", tc.name, set, removed, tc.set, tc.removed)
		}
		if tc.code != codes.OK && len(resp.GetDeniedResponse().GetHeaders()) != 0 {
			t.Errorf("%s: the denial has headers %v", tc.name, resp.GetDeniedResponse().GetHeaders())
		}
	}
	// The metrics are recorded before the response: an empty value is no
	// error.
	metrics = scrape(t, adminAddress)
	for series, want := range map[string]float64{
		`tessera_upstream_header_errors_total{cause="expression",header="x-tag"}`:                   2,
		`tessera_upstream_header_errors_total{cause="client_ip_missing",header="x-client-country"}`: 1,
		`tessera_upstream_header_errors_total{cause="client_ip_missing",header="x-client-asn"}`:     1,
		`tessera_upstream_header_errors_total{cause="client_ip_missing",header="x-client-city"}`:    1,
		`tessera_upstream_header_errors_total{cause="expression",header="x-client-asn"}`:            0,
		`tessera_upstream_header_errors_total{cause="client_ip_missing",header="x-tag"}`:            0,
		`tessera_decisions_total{action="allow",enforced="true",reason="default",rule="default"}`:   3,
	} {
		if got := metrics[series]; got != want {
			t.Errorf("%s = %v, want %v", series, got, want)
		}
	}

	waitUntil(t, "the decision events", func() bool { return strings.Count(h.logs.String(), " msg=decision ") == 4 })
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	var decisions []string
	for _, event := range h.events() {
		if strings.Contains(event, " msg=decision ") {
			decisions = append(decisions, durationRE.ReplaceAllString(event, "duration_ms=D"))
		}
	}
	wantEvents(t, decisions, []string{
		`level=info msg=decision request_id="" host=api.example.com method=GET path=/ client_ip=192.0.2.7 action=allow enforced=true rule=default reason=default duration_ms=D agent=curl/8.7.1 country=IT`,
		`level=info msg=decision request_id="" host=api.example.com method=GET path=/ client_ip=203.0.113.200 action=allow enforced=true rule=default reason=default duration_ms=D agent=curl/8.7.1 country=""`,
		`level=info msg=decision request_id="" host=api.example.com method=GET path=/ client_ip="" action=allow enforced=true rule=default reason=default duration_ms=D agent=curl/8.7.1 country=""`,
		`level=warn msg=decision request_id="" host=api.example.com method=GET path=/ client_ip=198.51.100.7 action=deny enforced=true rule=blocked reason=rule duration_ms=D agent=curl/8.7.1 country=US`,
	})
}

var durationRE = regexp.MustCompile(`duration_ms=[0-9.e+-]+`)

func TestDecisionFieldsInJSON(t *testing.T) {
	h := launch(t, upstreamConfig(writeExampleDatabases(t), "json"), Options{})
	var grpcAddress string
	waitUntil(t, "the gRPC listener", func() bool {
		m := regexp.MustCompile(`"msg":"listening","listener":"grpc","address":"([^"]+)"`).FindStringSubmatch(h.logs.String())
		if m != nil {
			grpcAddress = m[1]
		}
		return m != nil && strings.Contains(h.logs.String(), `"msg":"ready"`)
	})
	checkWith(t, dialGRPC(t, grpcAddress), map[string]string{"x-forwarded-for": "2001:db8::7", "user-agent": `curl "8"`})
	waitUntil(t, "the decision event", func() bool { return strings.Contains(h.logs.String(), `"msg":"decision"`) })
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	for line := range strings.Lines(h.logs.String()) {
		if !strings.Contains(line, `"msg":"decision"`) {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		_, rest, _ := strings.Cut(line, `"duration_ms":`)
		if event["country"] != "DE" || event["agent"] != `curl "8"` || !regexp.MustCompile(`^[0-9.e+-]+,"agent":"curl \\"8\\"","country":"DE"}\n$`).MatchString(rest) {
			t.Errorf("decision event: %s", line)
		}
	}
}

// No event of serve holds the value of a secret header, built in or
// configured: conditions and upstream headers read them, and the decision
// events, with every decision field that can be declared beside them, are
// written in both formats.
func TestSecretHeadersAreNeverLogged(t *testing.T) {
	for _, format := range []string{"logfmt", "json"} {
		t.Run(format, func(t *testing.T) {
			h := launch(t, `server:
  listen: `+grpcPlaceholder+`
  drain_delay: 0s
admin:
  listen: `+adminPlaceholder+`
log:
  format: `+format+`
  decisions: all
  decision_fields:
    agent: request.headers["user-agent"]
    key_id: '"x-key-id" in request.headers ? request.headers["x-key-id"] : ""'
    target: request.host + request.path + request.id
secret_headers: [X-Api-Key, x-session]
policy:
  default_action: deny
  rules:
    - name: keys
      action: allow
      when: request.headers["x-api-key"].startsWith("k-")
    - name: broken
      action: deny
      when: int(request.headers["x-session"]) > 0
upstream:
  headers:
    x-key: request.headers["x-api-key"]
    x-credentials: request.headers["authorization"] + request.headers["proxy-authorization"] + request.headers["cookie"]
`, Options{})
			var grpcAddress string
			waitUntil(t, "the gRPC listener", func() bool {
				m := regexp.MustCompile(`(?:address=|"address":")([0-9.:\[\]]+)`).FindAllStringSubmatch(h.logs.String(), -1)
				if len(m) > 0 {
					grpcAddress = m[0][1]
				}
				return len(m) == 2 && strings.Contains(h.logs.String(), "ready")
			})
			conn := dialGRPC(t, grpcAddress)
			secrets := map[string]string{
				"x-api-key":           "k-s3cr3t-1",
				"x-session":           "s3cr3t-2",
				"authorization":       "Bearer s3cr3t-3",
				"proxy-authorization": "Basic s3cr3t-4",
				"cookie":              "id=s3cr3t-5",
				"user-agent":          "curl/8.7.1",
				"x-key-id":            "42",
			}
			if resp := checkWith(t, conn, secrets); codes.Code(resp.GetStatus().GetCode()) != codes.OK {
				t.Fatalf("allowed request: %v", resp)
			}
			secrets["x-api-key"] = "s3cr3t-6"
			if resp := checkWith(t, conn, secrets); codes.Code(resp.GetStatus().GetCode()) != codes.PermissionDenied {
				t.Fatalf("denied request: %v", resp)
			}
			waitUntil(t, "the decision events", func() bool { return strings.Count(h.logs.String(), "decision") >= 2 })
			if err := h.stop(); err != nil {
				t.Fatal(err)
			}
			logs := h.logs.String()
			if strings.Contains(logs, "s3cr3t") || strings.Contains(strings.ToLower(logs), "x-api-key") || strings.Contains(logs, "x-session") {
				t.Fatalf("an event holds a secret header or its name:\n%s", logs)
			}
			for _, want := range []string{"curl/8.7.1", "42", "api.example.com/"} {
				if !strings.Contains(logs, want) {
					t.Errorf("no decision field %q in:\n%s", want, logs)
				}
			}
		})
	}
}
