package engine

import (
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/gtriggiano/tessera/internal/enrichment"
	"github.com/gtriggiano/tessera/internal/expr"
	"github.com/gtriggiano/tessera/internal/mmdbtest"
	"github.com/gtriggiano/tessera/internal/sets"
)

// upstreamPolicy allows the requests of 192.0.2.0/24 and 2001:db8::/32,
// denies the others, and declares headers of every kind of value.
func upstreamPolicy(audit bool) Policy {
	return Policy{Audit: audit, DefaultAction: Deny, Rules: []Rule{
		{Name: "documentation", When: `has(client.ip) && (client.ip.inCidr("192.0.2.0/24") || client.ip.inCidr("2001:db8::/32"))`, Action: Allow},
	}, UpstreamHeaders: []NamedExpression{
		{"X-Client-Country", `geo.country.code`},
		{"x-client-asn", `asn.found ? string(asn.number) : ""`},
		{"x-client-ip", `string(client.ip)`},
		{"x-host", `request.host`},
		{"x-tag", `request.headers["x-tag"]`},
		{"x-long", `request.headers["x-long"]`},
		{"x-partner", `sets.partners.contains(request.host) ? "yes" : "no"`},
	}}
}

func TestUpstreamHeaders(t *testing.T) {
	long := strings.Repeat("a", MaxUpstreamHeaderBytes)
	partners := setStore(t, sets.Fail, false)
	dbs := databaseStore(t, mmdbtest.CityDatabase(6), mmdbtest.ASNDatabase())
	for _, tc := range []struct {
		name    string
		audit   bool
		client  string
		headers map[string]string
		want    Decision
	}{
		{
			"allowed", false, "192.0.2.7", map[string]string{"x-tag": "a\tb", "x-long": long},
			Decision{Action: Allow, Enforced: true, Rule: "documentation", Reason: ReasonRule, Upstream: []UpstreamHeader{
				{Name: "x-client-asn", Value: "64496"},
				{Name: "x-client-country", Value: "IT"},
				{Name: "x-client-ip", Value: "192.0.2.7"},
				{Name: "x-host", Value: "api.example.com"},
				{Name: "x-long", Value: long},
				{Name: "x-partner", Value: "yes"},
				{Name: "x-tag", Value: "a\tb"},
			}},
		},
		{
			// An empty value removes the header, and is no error; a value
			// that cannot be sent, and a failure, remove it with a cause.
			"allowed, values removed", false, "2001:db8:1::7", map[string]string{"x-tag": "a\x01b", "x-long": long + "a"},
			Decision{Action: Allow, Enforced: true, Rule: "documentation", Reason: ReasonRule, Upstream: []UpstreamHeader{
				{Name: "x-client-asn", Value: ""},
				{Name: "x-client-country", Value: ""},
				{Name: "x-client-ip", Value: "2001:db8:1::7"},
				{Name: "x-host", Value: "api.example.com"},
				{Name: "x-long", Cause: expr.CauseExpression},
				{Name: "x-partner", Value: "yes"},
				{Name: "x-tag", Cause: expr.CauseExpression},
			}},
		},
		{
			// A value that is not UTF-8, or that has a DEL, cannot be sent.
			"allowed, values not sent", false, "192.0.2.8", map[string]string{"x-tag": "a\xffb", "x-long": "a\x7fb"},
			Decision{Action: Allow, Enforced: true, Rule: "documentation", Reason: ReasonRule, Upstream: []UpstreamHeader{
				{Name: "x-client-asn", Value: "64496"},
				{Name: "x-client-country", Value: "IT"},
				{Name: "x-client-ip", Value: "192.0.2.8"},
				{Name: "x-host", Value: "api.example.com"},
				{Name: "x-long", Cause: expr.CauseExpression},
				{Name: "x-partner", Value: "yes"},
				{Name: "x-tag", Cause: expr.CauseExpression},
			}},
		},
		{
			"denied", false, "198.51.100.7", map[string]string{"x-tag": "a"},
			Decision{Action: Deny, Enforced: true, Rule: DefaultRule, Reason: ReasonDefault, Response: &forbidden},
		},
		{
			// A denial that the policy in audit mode lets through gets the
			// headers, computed for the client address that it has not.
			"denied in audit mode", true, "", map[string]string{},
			Decision{Action: Deny, Enforced: false, Rule: DefaultRule, Reason: ReasonDefault, Response: &forbidden, Upstream: []UpstreamHeader{
				{Name: "x-client-asn", Cause: expr.CauseClientIPMissing},
				{Name: "x-client-country", Cause: expr.CauseClientIPMissing},
				{Name: "x-client-ip", Cause: expr.CauseClientIPMissing},
				{Name: "x-host", Value: "api.example.com"},
				{Name: "x-long", Cause: expr.CauseExpression},
				{Name: "x-partner", Value: "yes"},
				{Name: "x-tag", Cause: expr.CauseExpression},
			}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEngine(t, upstreamPolicy(tc.audit), Options{Sets: partners, Databases: dbs})
			r := req(tc.client)
			r.Headers = tc.headers
			if got := summary(e.Decide(r)); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("%+v\nwant %+v", got, tc.want)
			}
			if got := summary(e.Explain(r).Decision); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Explain: %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

// A header that cannot be computed is removed with its cause, and the
// decision does not change: an unavailable set, a database that cannot be
// read, a panic.
func TestUpstreamHeaderFailures(t *testing.T) {
	var mu sync.Mutex
	var reported []*expr.Panic
	e := newEngine(t, Policy{DefaultAction: Allow, UpstreamHeaders: []NamedExpression{
		{"x-city", `geo.city`},
		{"x-partner", `sets.partners.contains(request.host) ? "yes" : "no"`},
		{"x-broken", `request.host`},
	}}, Options{
		Sets:      setStore(t, sets.Fail, true),
		Databases: databaseStore(t, mmdbtest.BrokenDatabase("geo"), mmdbtest.ASNDatabase()),
		OnPanic: func(p *expr.Panic) {
			mu.Lock()
			defer mu.Unlock()
			reported = append(reported, p)
		},
	})
	e.headers[0].value = panickingString{}
	want := Decision{Action: Allow, Enforced: true, Rule: DefaultRule, Reason: ReasonDefault, Upstream: []UpstreamHeader{
		{Name: "x-broken", Cause: expr.CausePanic},
		{Name: "x-city", Cause: expr.CauseDatabaseError},
		{Name: "x-partner", Cause: expr.CauseSetUnavailable},
	}}
	if got := summary(e.Decide(req("198.51.100.7"))); !reflect.DeepEqual(got, want) {
		t.Errorf("%+v\nwant %+v", got, want)
	}
	if len(reported) != 1 || reported[0].Rule != "" || reported[0].Value != "corrupt state" || !strings.Contains(string(reported[0].Stack), "upstream_test.go") {
		t.Errorf("reported %v, want one panic without a rule", reported)
	}
}

// panickingString is a string expression whose evaluation panics.
type panickingString struct{}

func (panickingString) Eval(*expr.Input) (string, error) { panic("corrupt state") }

// recordingString records the activations it is evaluated with.
type recordingString struct{ inputs *[]*expr.Input }

func (r recordingString) Eval(in *expr.Input) (string, error) {
	*r.inputs = append(*r.inputs, in)
	return "x", nil
}

// recordingCondition records the activations it is evaluated with.
type recordingCondition struct{ inputs *[]*expr.Input }

func (r recordingCondition) Eval(in *expr.Input) (bool, error) {
	*r.inputs = append(*r.inputs, in)
	return true, nil
}

// The upstream headers and the decision fields are computed after the
// rules, with the activation of the decision.
func TestHeadersAndFieldsShareTheDecision(t *testing.T) {
	e := newEngine(t, Policy{DefaultAction: Deny, Rules: []Rule{{Name: "all", When: `request.host != ""`, Action: Allow}},
		UpstreamHeaders: []NamedExpression{{"x-a", `request.host`}},
		DecisionFields:  []NamedExpression{{"a", `request.host`}},
	}, Options{})
	var inputs []*expr.Input
	e.rules[0].when = recordingCondition{&inputs}
	e.headers[0].value = recordingString{&inputs}
	e.fields[0].value = recordingString{&inputs}
	d := e.Decide(req("192.0.2.7"))
	if len(inputs) != 2 {
		t.Fatalf("%d evaluations before the fields, want the rule and the header", len(inputs))
	}
	if fields := e.DecisionFields(d); !reflect.DeepEqual(fields, []Field{{"a", "x"}}) {
		t.Errorf("fields: %v", fields)
	}
	if len(inputs) != 3 || inputs[0] != inputs[1] || inputs[1] != inputs[2] {
		t.Errorf("the rule, the header and the field had %d activations, want the same one", len(inputs))
	}
	// A denial computes no upstream header: only its rule is evaluated.
	inputs = nil
	e.rules[0].when = recordingCondition{&inputs}
	e.rules[0].action = Deny
	if d := e.Decide(req("192.0.2.7")); d.Action != Deny || d.Upstream != nil || len(inputs) != 1 {
		t.Errorf("denial: %+v, %d evaluations", d, len(inputs))
	}
}

func TestDecisionFields(t *testing.T) {
	var mu sync.Mutex
	var reported []*expr.Panic
	dbs := databaseStore(t, mmdbtest.CityDatabase(6), mmdbtest.ASNDatabase())
	e := newEngine(t, Policy{DefaultAction: Deny, Rules: []Rule{
		{Name: "europe", When: `geo.country.in_eu`, Action: Allow},
	}, DecisionFields: []NamedExpression{
		{"country", `geo.country.code`},
		{"asn", `string(asn.number)`},
		{"agent", `request.headers["user-agent"]`},
		{"missing", `request.headers["x-missing"]`},
		{"broken", `request.host`},
	}}, Options{Databases: dbs, OnPanic: func(p *expr.Panic) {
		mu.Lock()
		defer mu.Unlock()
		reported = append(reported, p)
	}})
	e.fields[2].value = panickingString{} // broken, third in the order of the names
	d := e.Decide(req("192.0.2.7"))
	// The databases change after the decision: the fields read what the
	// decision read.
	replaceDatabase(t, dbs, enrichment.Geo, mmdbtest.BrokenDatabase("geo"))
	replaceDatabase(t, dbs, enrichment.ASN, mmdbtest.BrokenDatabase("asn"))
	want := []Field{{"agent", "curl/8.7.1"}, {"asn", "64496"}, {"broken", ""}, {"country", "IT"}, {"missing", ""}}
	if got := e.DecisionFields(d); !reflect.DeepEqual(got, want) {
		t.Errorf("%v\nwant %v", got, want)
	}
	if len(reported) != 1 || reported[0].Rule != "" {
		t.Errorf("reported %v, want one panic without a rule", reported)
	}
	// A failure leaves the field empty: here every lookup fails.
	d = e.Decide(req("198.51.100.7"))
	want = []Field{{"agent", "curl/8.7.1"}, {"asn", ""}, {"broken", ""}, {"country", ""}, {"missing", ""}}
	if got := e.DecisionFields(d); !reflect.DeepEqual(got, want) {
		t.Errorf("failures: %v\nwant %v", got, want)
	}
	// A denial has them too; a decision of no engine, and an engine without
	// fields, have none.
	if got := e.DecisionFields(e.Decide(req(""))); len(got) != len(want) {
		t.Errorf("denial: %v", got)
	}
	if got := e.DecisionFields(Decision{Action: Allow, Enforced: true}); got != nil {
		t.Errorf("decision of no engine: %v", got)
	}
	plain := newEngine(t, Policy{DefaultAction: Allow}, Options{})
	if d := plain.Decide(req("192.0.2.7")); plain.DecisionFields(d) != nil || d.Upstream != nil {
		t.Errorf("policy without fields and headers: %v, %v", plain.DecisionFields(d), d.Upstream)
	}
}

// A decision field that reads what tessera never logs does not compile.
func TestNewRejectsAnInvalidDecisionField(t *testing.T) {
	for _, p := range []Policy{
		{DefaultAction: Allow, DecisionFields: []NamedExpression{{"query", `request.query`}}},
		{DefaultAction: Allow, DecisionFields: []NamedExpression{{"bool", `request.host == "x"`}}},
		{DefaultAction: Allow, UpstreamHeaders: []NamedExpression{{"x-bool", `request.host == "x"`}}},
	} {
		if _, err := New(p, Options{}); err == nil {
			t.Errorf("%+v: no error", p)
		}
	}
	// An upstream header can read the query.
	if _, err := New(Policy{DefaultAction: Allow, UpstreamHeaders: []NamedExpression{{"x-query", `request.query`}}}, Options{}); err != nil {
		t.Error(err)
	}
}

// The engine knows, from the compiled policy, which upstream headers can hold
// the value of authorization or cookie.
func TestHeaderReadsSecrets(t *testing.T) {
	e := newEngine(t, Policy{DefaultAction: Allow, SecretHeaders: []string{"X-Api-Key"}, UpstreamHeaders: []NamedExpression{
		{"X-Auth", `request.headers["authorization"]`},
		{"x-session", `request.headers.Cookie`},
		{"x-proxy", `request.headers["proxy-authorization"]`},
		{"x-key", `request.headers["x-api-key"]`},
		{"x-agent", `request.headers["user-agent"]`},
	}, DecisionFields: []NamedExpression{{"agent", `request.headers["user-agent"]`}}}, Options{})
	for name, want := range map[string]bool{"x-auth": true, "x-session": true, "x-proxy": true, "x-key": true, "x-agent": false, "x-other": false} {
		if got := e.HeaderReadsSecrets(name); got != want {
			t.Errorf("%s: %v, want %v", name, got, want)
		}
	}
	// A decision field cannot read a configured secret header.
	if _, err := New(Policy{DefaultAction: Allow, SecretHeaders: []string{"X-Api-Key"}, DecisionFields: []NamedExpression{{"key", `request.headers["x-api-key"]`}}}, Options{}); err == nil {
		t.Error("a decision field reads a configured secret header")
	}
}
