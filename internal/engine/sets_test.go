package engine

import (
	"reflect"
	"sync"
	"testing"

	"github.com/gtriggiano/tessera/internal/expr"
	"github.com/gtriggiano/tessera/internal/sets"
)

// setStore declares blocked (cidr) and partners (string), with on_unavailable
// answer, both loaded; unavailable marks partners unavailable.
func setStore(t testing.TB, answer sets.OnUnavailable, unavailable bool) *sets.Store {
	t.Helper()
	store, err := sets.NewStore([]sets.Declaration{
		{Name: "blocked", Type: sets.CIDR, OnUnavailable: sets.Fail},
		{Name: "partners", Type: sets.String, OnUnavailable: answer},
	})
	if err != nil {
		t.Fatal(err)
	}
	blocked, _ := sets.FromValues(sets.CIDR, []string{"198.51.100.0/24"}, 10)
	partners, _ := sets.FromValues(sets.String, []string{"api.example.com"}, 10)
	if err := store.Replace("blocked", blocked); err != nil {
		t.Fatal(err)
	}
	if err := store.Replace("partners", partners); err != nil {
		t.Fatal(err)
	}
	if unavailable {
		if err := store.SetAvailable("partners", false); err != nil {
			t.Fatal(err)
		}
	}
	return store
}

func TestSets(t *testing.T) {
	e := newEngine(t, Policy{DefaultAction: Deny, Rules: []Rule{
		{Name: "blocked", When: `sets.blocked.contains(client.ip)`, Action: Deny},
		{Name: "partners", When: `sets.partners.contains(request.host)`, Action: Allow},
	}}, Options{Sets: setStore(t, sets.Fail, false)})
	if d := summary(e.Decide(req("198.51.100.7"))); d.Action != Deny || d.Rule != "blocked" || d.Reason != ReasonRule {
		t.Errorf("a blocked address: %+v", d)
	}
	if d := summary(e.Decide(req("192.0.2.10"))); d.Action != Allow || d.Rule != "partners" {
		t.Errorf("a partner host: %+v", d)
	}
	r := req("192.0.2.10")
	r.Host = "www.example.com"
	if d := summary(e.Decide(r)); d.Action != Deny || d.Rule != DefaultRule {
		t.Errorf("another host: %+v", d)
	}
}

// A set that is unavailable, with on_unavailable: fail, makes the conditions
// that depend on it indeterminate, with the cause set_unavailable; match and
// no_match give a certain answer.
func TestSetUnavailable(t *testing.T) {
	partner := `sets.partners.contains(request.host)`
	for _, tc := range []struct {
		name   string
		answer sets.OnUnavailable
		rules  []Rule
		want   Decision
	}{
		{"deny rule denies", sets.Fail, []Rule{
			{Name: "not-partners", When: "!" + partner, Action: Deny},
			{Name: "everyone", When: "true", Action: Allow},
		}, Decision{Action: Deny, Enforced: true, Rule: "not-partners", Reason: ReasonError, Cause: expr.CauseSetUnavailable, Errors: []RuleError{{"not-partners", expr.CauseSetUnavailable}}, Response: &forbidden}},
		{"allow rule does not match", sets.Fail, []Rule{
			{Name: "partners", When: partner, Action: Allow},
			{Name: "public", When: `request.path.startsWith("/public/")`, Action: Allow},
		}, Decision{Action: Deny, Enforced: true, Rule: DefaultRule, Reason: ReasonDefault, IndeterminateRules: []string{"partners"}, Errors: []RuleError{{"partners", expr.CauseSetUnavailable}}, Response: &forbidden}},
		{"a later rule still allows", sets.Fail, []Rule{
			{Name: "partners", When: partner, Action: Allow},
			{Name: "orders", When: `request.path.startsWith("/v1/")`, Action: Allow},
		}, Decision{Action: Allow, Enforced: true, Rule: "orders", Reason: ReasonRule, IndeterminateRules: []string{"partners"}, Errors: []RuleError{{"partners", expr.CauseSetUnavailable}}}},
		{"&& absorbs it", sets.Fail, []Rule{
			{Name: "partners", When: `request.method == "POST" && ` + partner, Action: Deny},
		}, Decision{Action: Allow, Enforced: true, Rule: DefaultRule, Reason: ReasonDefault}},
		{"|| absorbs it", sets.Fail, []Rule{
			{Name: "partners", When: partner + ` || request.method == "GET"`, Action: Allow},
		}, Decision{Action: Allow, Enforced: true, Rule: "partners", Reason: ReasonRule}},
		{"an audit rule counts it", sets.Fail, []Rule{
			{Name: "watch-partners", When: partner, Action: Deny, Audit: true},
		}, Decision{Action: Allow, Enforced: true, Rule: DefaultRule, Reason: ReasonDefault, Errors: []RuleError{{"watch-partners", expr.CauseSetUnavailable}}}},
		{"another set stays available", sets.Fail, []Rule{
			{Name: "blocked", When: `sets.blocked.contains(client.ip)`, Action: Deny},
		}, Decision{Action: Allow, Enforced: true, Rule: DefaultRule, Reason: ReasonDefault}},
		{"match answers true", sets.Match, []Rule{
			{Name: "partners", When: partner, Action: Allow},
		}, Decision{Action: Allow, Enforced: true, Rule: "partners", Reason: ReasonRule}},
		{"no_match answers false", sets.NoMatch, []Rule{
			{Name: "not-partners", When: "!" + partner, Action: Deny},
		}, Decision{Action: Deny, Enforced: true, Rule: "not-partners", Reason: ReasonRule, Response: &forbidden}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEngine(t, Policy{DefaultAction: tc.want.defaultAction(), Rules: tc.rules}, Options{Sets: setStore(t, tc.answer, true)})
			if got := summary(e.Decide(req("192.0.2.10"))); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("%+v\nwant %+v", got, tc.want)
			}
			x := e.Explain(req("192.0.2.10"))
			if got := summary(x.Decision); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Explain: %+v\nwant %+v", got, tc.want)
			}
		})
	}
	// The outcome of a rule names the cause.
	e := newEngine(t, Policy{DefaultAction: Deny, Rules: []Rule{{Name: "partners", When: partner, Action: Allow}}}, Options{Sets: setStore(t, sets.Fail, true)})
	if got := e.Explain(req("192.0.2.10")).Outcomes; !reflect.DeepEqual(got, []Outcome{{Rule: "partners", Result: ResultIndeterminate, Cause: expr.CauseSetUnavailable}}) {
		t.Errorf("outcomes %+v", got)
	}
}

// An engine without a store declares no set: a condition that reads one
// does not compile.
func TestSetsWithoutStore(t *testing.T) {
	if _, err := New(Policy{DefaultAction: Deny, Rules: []Rule{{Name: "partners", When: `sets.partners.contains(request.host)`, Action: Allow}}}, Options{}); err == nil {
		t.Error("New accepted a condition on a set that is not declared")
	}
}

// Decisions run while the copies are replaced and marked unavailable: each
// decision sees one snapshot for all its rules. Run with -race.
func TestSetsReplacedDuringDecisions(t *testing.T) {
	store := setStore(t, sets.Fail, false)
	// Both rules read partners: within one snapshot the first allows when
	// the second would, so the second is never reached with the host in the
	// set, and an unavailable set stops both.
	e := newEngine(t, Policy{DefaultAction: Deny, Rules: []Rule{
		{Name: "partners", When: `sets.partners.contains(request.host)`, Action: Allow},
		{Name: "inconsistent", When: `sets.partners.contains(request.host)`, Action: Deny},
	}}, Options{Sets: store})
	with, _ := sets.FromValues(sets.String, []string{"api.example.com"}, 10)
	without, _ := sets.FromValues(sets.String, []string{"www.example.com"}, 10)
	var writer sync.WaitGroup
	stop := make(chan struct{})
	writer.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_ = store.Replace("partners", []*sets.Set{with, without}[i%2])
			if i%3 == 0 {
				_ = store.SetAvailable("partners", false)
			}
		}
	})
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			for range 1000 {
				if d := e.Decide(req("192.0.2.10")); d.Rule == "inconsistent" && d.Reason == ReasonRule {
					t.Errorf("one decision saw two copies: %+v", d)
					return
				}
			}
		})
	}
	workers.Wait()
	close(stop)
	writer.Wait()
}
