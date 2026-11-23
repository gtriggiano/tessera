package config

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestUpstreamHeadersAndDecisionFields(t *testing.T) {
	c := mustParse(t, `
enrichment:
  geo:
    database: geo.mmdb
log:
  decision_fields:
    agent: request.headers["user-agent"]
    country_code: geo.country.code
upstream:
  headers:
    X-Client-Country: geo.country.code
    x-request-query: request.query
    x-auth: request.headers["authorization"]
policy:
  default_action: deny
`, nil)
	wantFields := map[string]Expression{"agent": `request.headers["user-agent"]`, "country_code": "geo.country.code"}
	wantHeaders := map[string]Expression{"X-Client-Country": "geo.country.code", "x-request-query": "request.query", "x-auth": `request.headers["authorization"]`}
	if !reflect.DeepEqual(c.Log.DecisionFields, wantFields) || !reflect.DeepEqual(c.Upstream.Headers, wantHeaders) {
		t.Fatalf("got %v and %v", c.Log.DecisionFields, c.Upstream.Headers)
	}
	// The database is read by the header and the field, without any rule.
	if w := c.Warnings(); len(w) != 0 {
		t.Errorf("warnings %v", w)
	}
	// A name of the limit length, and the largest numbers of each.
	var fields, headers strings.Builder
	for i := range MaxDecisionFields {
		fmt.Fprintf(&fields, "    f%d: request.host\n", i)
	}
	for i := range MaxUpstreamHeaders {
		fmt.Fprintf(&headers, "    x-h%d: request.host\n", i)
	}
	c = mustParse(t, "log:\n  decision_fields:\n"+fields.String()+"upstream:\n  headers:\n"+headers.String()+"policy: {default_action: deny}\n", nil)
	if len(c.Log.DecisionFields) != MaxDecisionFields || len(c.Upstream.Headers) != MaxUpstreamHeaders {
		t.Errorf("%d fields, %d headers", len(c.Log.DecisionFields), len(c.Upstream.Headers))
	}
	mustParse(t, "log:\n  decision_fields:\n    "+strings.Repeat("a", MaxDecisionFieldNameLength)+": request.host\npolicy: {default_action: deny}\n", nil)
}

// A set or a database read only by an upstream header or a decision field
// is used: only the others are warnings.
func TestUsesOfHeadersAndFields(t *testing.T) {
	const base = `
enrichment:
  geo: {database: geo.mmdb}
  asn: {database: asn.mmdb}
sets:
  partners: {type: string, values: [api.example.com]}
  office: {type: cidr, values: [192.0.2.0/24]}
policy: {default_action: deny}
`
	for _, tc := range []struct {
		name, yaml string
		unused     []string
	}{
		{"nothing reads them", "", []string{"enrichment.geo", "enrichment.asn", "sets.partners", "sets.office"}},
		{"upstream headers", "upstream:\n  headers:\n    x-partner: 'sets.partners.contains(request.host) ? \"yes\" : \"\"'\n    x-country: geo.country.code\n", []string{"enrichment.asn", "sets.office"}},
		{"decision fields", "log:\n  decision_fields:\n    office: 'sets.office.contains(client.ip) ? \"yes\" : \"no\"'\n    asn: string(asn.number)\n", []string{"enrichment.geo", "sets.partners"}},
	} {
		c := mustParse(t, base+tc.yaml, nil)
		var got []string
		for _, w := range c.Warnings() {
			got = append(got, w.Path)
		}
		if !reflect.DeepEqual(got, tc.unused) {
			t.Errorf("%s: warnings on %v, want %v", tc.name, got, tc.unused)
		}
	}
}

func TestInvalidUpstreamHeaders(t *testing.T) {
	const head = "policy: {default_action: deny}\nupstream:\n  headers:\n"
	var many strings.Builder
	for i := range MaxUpstreamHeaders + 1 {
		fmt.Fprintf(&many, "    x-h%d: request.host\n", i)
	}
	for _, tc := range []struct{ name, yaml, want string }{
		{"not a mapping", "policy: {default_action: deny}\nupstream: {headers: [x-a]}\n", "upstream.headers (line 2, column 21): expected a mapping"},
		{"unknown key", "policy: {default_action: deny}\nupstream: {header: {}}\n", "upstream.header (line 2, column 12): unknown key"},
		{"too many", head + many.String(), "upstream.headers (line 4, column 5): more than 64 headers"},
		{"name", head + "    \"x " + secret + "\": request.host\n", "upstream.headers (line 4, column 5): expected a header name"},
		{"pseudo-header", head + "    \":authority\": request.host\n", "upstream.headers (line 4, column 5): expected a header name"},
		{"host", head + "    Host: request.host\n", "upstream.headers (line 4, column 5): this header is not allowed: it changes the meaning of the request, holds its credentials, or belongs to Envoy"},
		{"authorization", head + "    authorization: request.host\n", "upstream.headers (line 4, column 5): this header is not allowed: it changes the meaning of the request, holds its credentials, or belongs to Envoy"},
		{"proxy-authorization", head + "    x-a: request.host\n    Proxy-Authorization: request.host\n", "upstream.headers (line 5, column 5): this header is not allowed: it changes the meaning of the request, holds its credentials, or belongs to Envoy"},
		{"cookie", head + "    x-a: request.host\n    COOKIE: request.host\n", "upstream.headers (line 5, column 5): this header is not allowed: it changes the meaning of the request, holds its credentials, or belongs to Envoy"},
		{"envoy header", head + "    X-Envoy-Original-Path: request.path\n", "upstream.headers (line 4, column 5): this header is not allowed: it changes the meaning of the request, holds its credentials, or belongs to Envoy"},
		{"same header in two cases", head + "    x-a: request.host\n    X-A: request.path\n", "upstream.headers (line 5, column 5): the same header twice, in different cases"},
		{"duplicate", head + "    x-a: request.host\n    x-a: request.path\n", "upstream.headers (line 5, column 5): duplicate key"},
		{"value not a string", head + "    x-a: 1\n", "upstream.headers (line 4, column 10): expected a string"},
		{"empty value", head + "    x-a: \"\"\n", "upstream.headers (line 4, column 11): the expression is empty"},
		{"unknown attribute", head + "    x-a: request.hots\n", "upstream.headers (line 4, column 17): undefined field 'hots'"},
		{"not a string", head + "    x-a: request.host == \"x\"\n", "upstream.headers (line 4, column 10): the expression must be a string, not bool"},
		{"quoted", head + "    x-a: 'request.hots + \"x\"'\n", "upstream.headers (line 4, column 18): undefined field 'hots'"},
		{"literal block", head + "    x-a: |\n      request.host +\n        request.hots\n", "upstream.headers (line 6, column 16): undefined field 'hots'"},
		{"geo without its database", head + "    x-country: geo.country.code\n", "upstream.headers (line 4, column 16): geo is not available: the configuration has no enrichment.geo"},
		{"unknown set", head + "    x-a: 'sets.partners.contains(request.host) ? \"y\" : \"n\"'\n", "upstream.headers (line 4, column 15): undefined field 'partners'"},
		{"for a set of the header", "policy: {default_action: deny}\nupstream:\n  headers:\n    x-a: string(size(request.path) + " + "\"" + secret + "\")\n", "upstream.headers (line 4, column 36): found no matching overload for '_+_' applied to '(int, string)'"},
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
	// An expression with a substitution: nothing from it in the message.
	_, err := parse(t, head+"    x-a: request.headers[\"x-key\"] == ${KEY}\n", map[string]string{"KEY": secret})
	if err == nil || err.Error() != "upstream.headers (line 4, column 10): invalid expression" {
		t.Errorf("substitution: %v", err)
	}
}

func TestInvalidDecisionFields(t *testing.T) {
	const head = "policy: {default_action: deny}\nlog:\n  decision_fields:\n"
	var many strings.Builder
	for i := range MaxDecisionFields + 1 {
		fmt.Fprintf(&many, "    f%d: request.host\n", i)
	}
	const name = "expected a name of lowercase letters, digits and underscores that starts with a letter, at most 63 characters"
	for _, tc := range []struct{ name, yaml, want string }{
		{"not a mapping", "policy: {default_action: deny}\nlog: {decision_fields: [a]}\n", "log.decision_fields (line 2, column 24): expected a mapping"},
		{"too many", head + many.String(), "log.decision_fields (line 4, column 5): more than 32 fields"},
		{"uppercase", head + "    Country: geo.country.code\n", "log.decision_fields (line 4, column 5): " + name},
		{"digit first", head + "    1st: request.host\n", "log.decision_fields (line 4, column 5): " + name},
		{"hyphen", head + "    user-agent: request.host\n", "log.decision_fields (line 4, column 5): " + name},
		{"not repeated", head + "    \"" + secret + " x\": request.host\n", "log.decision_fields (line 4, column 5): " + name},
		{"too long", head + "    " + strings.Repeat("a", MaxDecisionFieldNameLength+1) + ": request.host\n", "log.decision_fields (line 4, column 5): " + name},
		{"value not a string", head + "    a: [request.host]\n", "log.decision_fields (line 4, column 8): expected a string"},
		{"not a string", head + "    a: size(request.path)\n", "log.decision_fields (line 4, column 8): the expression must be a string, not int"},
		{"unknown attribute", head + "    a: request.hots\n", "log.decision_fields (line 4, column 15): undefined field 'hots'"},
		{"query", head + "    q: request.query\n", "log.decision_fields (line 4, column 8): a decision field cannot read request.query"},
		{"authorization", head + "    a: request.headers[\"authorization\"]\n", "log.decision_fields (line 4, column 8): a decision field cannot read a secret header"},
		{"cookie as a field", head + "    a: request.host + request.headers.cookie\n", "log.decision_fields (line 4, column 23): a decision field cannot read a secret header"},
		{"cookie with in", head + "    a: '\"cookie\" in request.headers ? \"y\" : \"n\"'\n", "log.decision_fields (line 4, column 21): a decision field cannot read a secret header"},
		{"every header", head + "    a: 'request.headers.exists(h, h == \"x\") ? \"y\" : \"n\"'\n", "log.decision_fields (line 4, column 9): a decision field can read request.headers only by a literal name, such as request.headers[\"user-agent\"]"},
		{"dynamic key", head + "    a: request.headers[request.host]\n", "log.decision_fields (line 4, column 8): a decision field can read request.headers only by a literal name, such as request.headers[\"user-agent\"]"},
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
	// No field takes the name of a field of the event.
	for _, reserved := range decisionEventFields {
		_, err := parse(t, head+"    "+reserved+": request.host\n", nil)
		if want := "log.decision_fields (line 4, column 5): the decision event already has a field of this name"; err == nil || err.Error() != want {
			t.Errorf("%s: %v", reserved, err)
		}
	}
}

func TestRedactedHeadersAndFields(t *testing.T) {
	c := mustParse(t, `
log:
  decision_fields:
    tenant: request.headers["${TENANT_HEADER}"]
    agent: request.headers["user-agent"]
upstream:
  headers:
    X-Tenant: '"${TENANT}"'
    x-host: request.host
policy: {default_action: deny}
`, map[string]string{"TENANT_HEADER": "x-tenant", "TENANT": secret})
	got, err := c.Redacted()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"  decision_fields:\n    agent: request.headers[\"user-agent\"]\n    tenant: '[REDACTED]'\n",
		"upstream:\n  headers:\n    X-Tenant: '[REDACTED]'\n    x-host: request.host\n",
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("printed:\n%s\nwant it to contain:\n%s", got, want)
		}
	}
	if strings.Contains(string(got), secret) || strings.Contains(string(got), "x-tenant") {
		t.Fatal("a substituted value is printed")
	}
	// Once its redacted values are written again, it reads back the same.
	printed := strings.NewReplacer("tenant: '[REDACTED]'", `tenant: request.headers["x-tenant"]`, "X-Tenant: '[REDACTED]'", `X-Tenant: '"`+secret+`"'`).Replace(string(got))
	again := mustParse(t, printed, nil)
	if !reflect.DeepEqual(again.Log, c.Log) || !reflect.DeepEqual(again.Upstream, c.Upstream) {
		t.Fatalf("reads back as %+v %+v", again.Log, again.Upstream)
	}
}
