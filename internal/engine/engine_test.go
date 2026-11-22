package engine

import (
	"fmt"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gtriggiano/tessera/internal/expr"
	"github.com/gtriggiano/tessera/internal/request"
)

var (
	forbidden = Response{Status: 403}
	notFound  = Response{Status: 404, Body: "Not found.", Headers: []Header{{Name: "Cache-Control", Value: "no-store"}, {Name: "content-type", Value: "text/plain"}}}
)

func newEngine(t testing.TB, p Policy, o Options) *Engine {
	t.Helper()
	if p.DenyResponse.Status == 0 {
		p.DenyResponse = forbidden
	}
	// A deny rule without a response of its own sends the default one, as a
	// configuration resolves it.
	p.Rules = slices.Clone(p.Rules)
	for i := range p.Rules {
		if p.Rules[i].Action == Deny && p.Rules[i].Response.Status == 0 {
			p.Rules[i].Response = p.DenyResponse
		}
	}
	e, err := New(p, o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

func req(clientIP string) *request.Request {
	r := &request.Request{
		Method:  "GET",
		Host:    "api.example.com",
		Path:    "/v1/orders",
		Scheme:  "https",
		Headers: map[string]string{"user-agent": "curl/8.7.1", "x-count": "seven"},
	}
	if clientIP != "" {
		r.ClientIP = netip.MustParseAddr(clientIP)
	}
	return r
}

// summary is what the tests compare of a decision.
func summary(d Decision) Decision {
	d.Duration = 0
	return d
}

func TestFirstRuleDecides(t *testing.T) {
	e := newEngine(t, Policy{DefaultAction: Deny, Rules: []Rule{
		{Name: "blocked", When: `client.ip.inCidr("198.51.100.0/24")`, Action: Deny, Response: notFound},
		{Name: "office", When: `client.ip.inCidr("192.0.2.0/24")`, Action: Allow},
		{Name: "api", When: `request.host == "api.example.com"`, Action: Deny},
	}}, Options{})
	for _, tc := range []struct {
		client string
		want   Decision
	}{
		{"198.51.100.7", Decision{Action: Deny, Enforced: true, Rule: "blocked", Reason: ReasonRule, Response: &Response{Status: 404, Body: "Not found.", Headers: []Header{{"cache-control", "no-store"}, {"content-type", "text/plain"}}}}},
		{"192.0.2.10", Decision{Action: Allow, Enforced: true, Rule: "office", Reason: ReasonRule}},
		{"203.0.113.9", Decision{Action: Deny, Enforced: true, Rule: "api", Reason: ReasonRule, Response: &forbidden}},
	} {
		if got := summary(e.Decide(req(tc.client))); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("client %s: %+v\nwant %+v", tc.client, got, tc.want)
		}
	}
}

// Failure table: no rule matches.
func TestNoRuleMatches(t *testing.T) {
	for _, action := range []Action{Allow, Deny} {
		e := newEngine(t, Policy{DefaultAction: action, DenyResponse: notFound, Rules: []Rule{
			{Name: "other-host", When: `request.host == "www.example.com"`, Action: Allow},
		}}, Options{})
		want := Decision{Action: action, Enforced: true, Rule: DefaultRule, Reason: ReasonDefault}
		if action == Deny {
			want.Response = e.denyResponse
		}
		if got := summary(e.Decide(req("192.0.2.10"))); !reflect.DeepEqual(got, want) {
			t.Errorf("default %s: %+v\nwant %+v", action, got, want)
		}
	}
	// Without rules.
	if d := newEngine(t, Policy{DefaultAction: Allow}, Options{}).Decide(req("")); d.Action != Allow || d.Rule != DefaultRule || d.Reason != ReasonDefault {
		t.Errorf("no rules: %+v", d)
	}
}

// Failure table: the client address is not available. A deny rule that reads
// it denies, an allow rule does not match, and a rule before them with
// !has(client.ip) decides otherwise.
func TestClientAddressMissing(t *testing.T) {
	inOffice := `client.ip.inCidr("192.0.2.0/24")`
	for _, tc := range []struct {
		name  string
		rules []Rule
		want  Decision
	}{
		{"deny rule denies", []Rule{
			{Name: "blocked", When: `!client.ip.inCidr("192.0.2.0/24")`, Action: Deny},
			{Name: "everyone", Action: Allow, When: "true"},
		}, Decision{Action: Deny, Enforced: true, Rule: "blocked", Reason: ReasonError, Cause: expr.CauseClientIPMissing, Errors: []RuleError{{"blocked", expr.CauseClientIPMissing}}, Response: &forbidden}},
		{"allow rule does not match", []Rule{
			{Name: "office", When: inOffice, Action: Allow},
		}, Decision{Action: Deny, Enforced: true, Rule: DefaultRule, Reason: ReasonDefault, IndeterminateRules: []string{"office"}, Errors: []RuleError{{"office", expr.CauseClientIPMissing}}, Response: &forbidden}},
		{"a later rule still allows", []Rule{
			{Name: "office", When: inOffice, Action: Allow},
			{Name: "public", When: `request.path.startsWith("/v1/")`, Action: Allow},
		}, Decision{Action: Allow, Enforced: true, Rule: "public", Reason: ReasonRule, IndeterminateRules: []string{"office"}, Errors: []RuleError{{"office", expr.CauseClientIPMissing}}}},
		{"has(client.ip) decides first", []Rule{
			{Name: "no-address", When: `!has(client.ip)`, Action: Deny, Response: notFound},
			{Name: "office", When: inOffice, Action: Allow},
		}, Decision{Action: Deny, Enforced: true, Rule: "no-address", Reason: ReasonRule, Response: &Response{Status: 404, Body: "Not found.", Headers: []Header{{"cache-control", "no-store"}, {"content-type", "text/plain"}}}}},
		{"&& absorbs it", []Rule{
			{Name: "office", When: `request.host == "www.example.com" && ` + inOffice, Action: Deny},
		}, Decision{Action: Allow, Enforced: true, Rule: DefaultRule, Reason: ReasonDefault}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEngine(t, Policy{DefaultAction: tc.want.defaultAction(), Rules: tc.rules}, Options{})
			if got := summary(e.Decide(req(""))); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("%+v\nwant %+v", got, tc.want)
			}
		})
	}
}

// defaultAction is the default action the cases of TestClientAddressMissing
// use: deny, except when the decision shows the default action allowing.
func (d Decision) defaultAction() Action {
	if d.Reason == ReasonDefault {
		return d.Action
	}
	return Deny
}

// Failure table: an error in an expression makes the condition indeterminate
// only when its result depends on the error. The cost limit is such an
// error, and nothing absorbs it.
func TestExpressionError(t *testing.T) {
	big := req("192.0.2.10")
	big.Headers["x-big"] = strings.Repeat("a", 58<<10)
	for _, tc := range []struct {
		when       string
		r          *request.Request
		indetermin bool
		matched    bool
	}{
		{`int(request.headers["x-count"]) > 5`, req("192.0.2.10"), true, false},
		{`int(request.headers["x-count"]) > 5 || request.method == "GET"`, req("192.0.2.10"), false, true},
		{`request.method == "POST" && int(request.headers["x-count"]) > 5`, req("192.0.2.10"), false, false},
		{`request.headers["authorization"] == "secret"`, req("192.0.2.10"), true, false},
		{`"authorization" in request.headers && request.headers["authorization"] == "secret"`, req("192.0.2.10"), false, false},
		{`request.headers["x-big"].matches("^(a|b)*c$")`, big, true, false},
		{`request.headers["x-big"].matches("^(a|b)*c$") || true`, big, true, false},
	} {
		for _, action := range []Action{Allow, Deny} {
			opposite := map[Action]Action{Allow: Deny, Deny: Allow}[action]
			e := newEngine(t, Policy{DefaultAction: opposite, Rules: []Rule{{Name: "r", When: tc.when, Action: action}}}, Options{})
			d := e.Decide(tc.r)
			var want Decision
			switch {
			case tc.indetermin && action == Deny:
				want = Decision{Action: Deny, Rule: "r", Reason: ReasonError, Cause: expr.CauseExpression}
			case tc.indetermin:
				want = Decision{Action: Deny, Rule: DefaultRule, Reason: ReasonDefault, IndeterminateRules: []string{"r"}}
			case tc.matched:
				want = Decision{Action: action, Rule: "r", Reason: ReasonRule}
			default:
				want = Decision{Action: opposite, Rule: DefaultRule, Reason: ReasonDefault}
			}
			if d.Action != want.Action || d.Rule != want.Rule || d.Reason != want.Reason || d.Cause != want.Cause || !reflect.DeepEqual(d.IndeterminateRules, want.IndeterminateRules) {
				t.Errorf("%s rule %s: %+v\nwant %+v", action, tc.when, summary(d), want)
			}
			if tc.indetermin != (len(d.Errors) == 1) {
				t.Errorf("%s rule %s: errors %v", action, tc.when, d.Errors)
			}
		}
	}
}

// panicking is a condition whose evaluation panics.
type panicking struct{}

func (panicking) Eval(*expr.Input) (bool, error) { panic("corrupt state") }

// Failure table: a panic during an evaluation is recovered, makes the
// condition indeterminate, is reported with its stack, and the engine goes on
// deciding.
func TestPanicDuringEvaluation(t *testing.T) {
	var mu sync.Mutex
	var reported []*expr.Panic
	o := Options{OnPanic: func(p *expr.Panic) {
		mu.Lock()
		defer mu.Unlock()
		reported = append(reported, p)
	}}
	for _, tc := range []struct {
		action Action
		want   Decision
	}{
		{Deny, Decision{Action: Deny, Enforced: true, Rule: "broken", Reason: ReasonError, Cause: expr.CausePanic, Errors: []RuleError{{"broken", expr.CausePanic}}, Response: &forbidden}},
		{Allow, Decision{Action: Allow, Enforced: true, Rule: "public", Reason: ReasonRule, IndeterminateRules: []string{"broken"}, Errors: []RuleError{{"broken", expr.CausePanic}}}},
	} {
		reported = nil
		e := newEngine(t, Policy{DefaultAction: Deny, Rules: []Rule{
			{Name: "broken", When: `request.host == "x"`, Action: tc.action},
			{Name: "public", When: `request.path.startsWith("/v1/")`, Action: Allow},
		}}, o)
		e.rules[0].when = panicking{}
		for range 3 {
			if got := summary(e.Decide(req("192.0.2.10"))); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("%s rule that panics: %+v\nwant %+v", tc.action, got, tc.want)
			}
		}
		if len(reported) != 3 || reported[0].Rule != "broken" || reported[0].Value != "corrupt state" || !strings.Contains(string(reported[0].Stack), "engine_test.go") {
			t.Fatalf("%s rule: reported %d panics: %v", tc.action, len(reported), reported)
		}
	}
}

// Failure table: a policy in audit mode allows every request, and reports
// the decision it computed.
func TestPolicyInAuditMode(t *testing.T) {
	e := newEngine(t, Policy{Audit: true, DefaultAction: Deny, Rules: []Rule{
		{Name: "blocked", When: `client.ip.inCidr("198.51.100.0/24")`, Action: Deny},
		{Name: "office", When: `client.ip.inCidr("192.0.2.0/24")`, Action: Allow},
	}}, Options{})
	for client, want := range map[string]Decision{
		"198.51.100.7": {Action: Deny, Enforced: false, Rule: "blocked", Reason: ReasonRule, Response: &forbidden},
		"192.0.2.10":   {Action: Allow, Enforced: true, Rule: "office", Reason: ReasonRule},
		"203.0.113.9":  {Action: Deny, Enforced: false, Rule: DefaultRule, Reason: ReasonDefault, Response: &forbidden},
		"":             {Action: Deny, Enforced: false, Rule: "blocked", Reason: ReasonError, Cause: expr.CauseClientIPMissing, Errors: []RuleError{{"blocked", expr.CauseClientIPMissing}}, Response: &forbidden},
	} {
		if got := summary(e.Decide(req(client))); !reflect.DeepEqual(got, want) {
			t.Errorf("client %q: %+v\nwant %+v", client, got, want)
		}
	}
}

// A rule in audit mode never decides: a match is reported, an error is
// recorded, and the evaluation goes on, even after a rule without condition.
func TestRuleInAuditMode(t *testing.T) {
	e := newEngine(t, Policy{DefaultAction: Allow, Rules: []Rule{
		{Name: "watch-all", When: "true", Action: Deny, Audit: true},
		{Name: "watch-office", When: `client.ip.inCidr("192.0.2.0/24")`, Action: Allow, Audit: true},
		{Name: "watch-count", When: `int(request.headers["x-count"]) > 5`, Action: Deny, Audit: true},
		{Name: "api", When: `request.host == "api.example.com"`, Action: Deny},
	}}, Options{})
	want := Decision{
		Action: Deny, Enforced: true, Rule: "api", Reason: ReasonRule,
		AuditRules: []AuditMatch{{"watch-all", Deny}, {"watch-office", Allow}},
		Errors:     []RuleError{{"watch-count", expr.CauseExpression}},
		Response:   &forbidden,
	}
	if got := summary(e.Decide(req("192.0.2.10"))); !reflect.DeepEqual(got, want) {
		t.Errorf("%+v\nwant %+v", got, want)
	}
	want = Decision{
		Action: Deny, Enforced: true, Rule: "api", Reason: ReasonRule,
		AuditRules: []AuditMatch{{"watch-all", Deny}},
		Errors:     []RuleError{{"watch-office", expr.CauseClientIPMissing}, {"watch-count", expr.CauseExpression}},
		Response:   &forbidden,
	}
	if got := summary(e.Decide(req(""))); !reflect.DeepEqual(got, want) {
		t.Errorf("without an address: %+v\nwant %+v", got, want)
	}
}

// A missing client address never turns a denial into an allow: for every
// policy made of rules that read client.ip without has(), a request that is
// allowed without an address is allowed with every address.
func TestMissingAddressNeverAllowsMore(t *testing.T) {
	conditions := []string{
		`client.ip.inCidr("192.0.2.0/24")`,
		`!client.ip.inCidr("192.0.2.0/24")`,
		`client.ip.inCidr("192.0.2.0/24") || request.path.startsWith("/public/")`,
		`client.ip.inCidr("2001:db8::/32") && request.method == "GET"`,
		`client.ip.family() == 6`,
		`string(client.ip) != "203.0.113.9"`,
		`request.host == "api.example.com"`,
	}
	addresses := []string{"192.0.2.10", "203.0.113.9", "198.51.100.7", "2001:db8::1", "2001:db8:1::5"}
	paths := []string{"/v1/orders", "/public/index.html"}
	var rules []Rule
	for i, c := range conditions {
		for _, action := range []Action{Allow, Deny} {
			for _, audit := range []bool{false, true} {
				rules = append(rules, Rule{Name: fmt.Sprintf("r%d-%s-%t", i, action, audit), When: c, Action: action, Audit: audit})
			}
		}
	}
	checked := 0
	for _, first := range rules {
		for _, second := range rules {
			for _, def := range []Action{Allow, Deny} {
				e := newEngine(t, Policy{DefaultAction: def, Rules: []Rule{first, second}}, Options{})
				for _, path := range paths {
					without := req("")
					without.Path = path
					if e.Decide(without).Action != Allow {
						continue
					}
					for _, a := range addresses {
						with := req(a)
						with.Path = path
						checked++
						if d := e.Decide(with); d.Action != Allow {
							t.Fatalf("rules %+v and %+v, default %s, path %s: allowed without an address, denied with %s by %s", first, second, def, path, a, d.Rule)
						}
					}
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no case checked")
	}
}

func TestNewRejectsAnInvalidPolicy(t *testing.T) {
	for name, p := range map[string]Policy{
		"default action": {DefaultAction: "maybe"},
		"rule action":    {DefaultAction: Deny, Rules: []Rule{{Name: "r", When: "true", Action: "block"}}},
		"condition":      {DefaultAction: Deny, Rules: []Rule{{Name: "r", When: "request.hots == 1", Action: Deny}}},
		"no condition":   {DefaultAction: Deny, Rules: []Rule{{Name: "r", Action: Deny}}},
	} {
		if _, err := New(p, Options{}); err == nil {
			t.Errorf("%s: New accepted the policy", name)
		}
	}
}

// Decisions run concurrently on one engine (run with -race).
func TestConcurrentDecisions(t *testing.T) {
	e := newEngine(t, Policy{DefaultAction: Deny, Rules: []Rule{
		{Name: "blocked", When: `client.ip.inCidr("198.51.100.0/24")`, Action: Deny},
		{Name: "office", When: `client.ip.inCidr("192.0.2.0/24") && request.path.startsWith("/v1/")`, Action: Allow},
	}}, Options{})
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Go(func() {
			for i := range 300 {
				client := fmt.Sprintf("192.0.2.%d", (w*300+i)%250+1)
				if d := e.Decide(req(client)); d.Action != Allow || d.Rule != "office" || d.Duration <= 0 {
					t.Errorf("client %s: %+v", client, d)
					return
				}
			}
		})
	}
	wg.Wait()
}

func TestExplain(t *testing.T) {
	e := newEngine(t, Policy{DefaultAction: Deny, Rules: []Rule{
		{Name: "blocked", When: `client.ip.inCidr("198.51.100.0/24")`, Action: Deny},
		{Name: "watch", When: `"user-agent" in request.headers`, Action: Deny, Audit: true},
		{Name: "count", When: `int(request.headers["x-count"]) > 5`, Action: Allow},
		{Name: "api", When: `request.host == "api.example.com" && request.method in ["GET"]`, Action: Allow},
		{Name: "never-reached", When: `request.path == "/"`, Action: Deny},
	}}, Options{})
	x := e.Explain(req("192.0.2.10"))
	want := []Outcome{
		{Rule: "blocked", Result: ResultFalse},
		{Rule: "watch", Result: ResultTrue},
		{Rule: "count", Result: ResultIndeterminate, Cause: expr.CauseExpression},
		{Rule: "api", Result: ResultTrue},
	}
	if !reflect.DeepEqual(x.Outcomes, want) {
		t.Errorf("outcomes %+v\nwant %+v", x.Outcomes, want)
	}
	if got := strings.Join(x.Attributes, " "); got != `client.ip request.headers["user-agent"] request.headers["x-count"] request.host request.method` {
		t.Errorf("attributes %s", got)
	}
	if d := x.Decision; d.Action != Allow || d.Rule != "api" || !reflect.DeepEqual(d.AuditRules, []AuditMatch{{"watch", Deny}}) || !reflect.DeepEqual(d.IndeterminateRules, []string{"count"}) || d.Duration <= 0 {
		t.Errorf("decision %+v", d)
	}
	if d := e.Decide(req("192.0.2.10")); !reflect.DeepEqual(summary(d), summary(x.Decision)) {
		t.Errorf("Decide %+v, Explain %+v", summary(d), summary(x.Decision))
	}
}

// The duration of a decision counts from when the request was received.
func TestDurationFromReceived(t *testing.T) {
	e := newEngine(t, Policy{DefaultAction: Allow}, Options{})
	r := req("192.0.2.10")
	r.Received = time.Now().Add(-5 * time.Millisecond)
	if d := e.Decide(r); d.Duration < 5*time.Millisecond {
		t.Errorf("duration %v, want at least 5ms from Received", d.Duration)
	}
	if x := e.Explain(r); x.Decision.Duration < 5*time.Millisecond {
		t.Errorf("explained duration %v", x.Decision.Duration)
	}
	if d := e.Decide(req("192.0.2.10")); d.Duration <= 0 || d.Duration > time.Second {
		t.Errorf("duration without Received: %v", d.Duration)
	}
}
