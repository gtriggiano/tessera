package engine

import (
	"bytes"
	"reflect"
	"sync"
	"testing"

	"github.com/gtriggiano/tessera/internal/enrichment"
	"github.com/gtriggiano/tessera/internal/expr"
	"github.com/gtriggiano/tessera/internal/mmdbtest"
)

// databaseStore holds the geo database of geo and the asn database of asn.
func databaseStore(t testing.TB, geo, asn *mmdbtest.Writer) *enrichment.Store {
	t.Helper()
	store, err := enrichment.NewStore(enrichment.Kinds)
	if err != nil {
		t.Fatal(err)
	}
	for k, w := range map[enrichment.Kind]*mmdbtest.Writer{enrichment.Geo: geo, enrichment.ASN: asn} {
		replaceDatabase(t, store, k, w)
	}
	return store
}

func replaceDatabase(t testing.TB, store *enrichment.Store, k enrichment.Kind, w *mmdbtest.Writer) {
	t.Helper()
	data, err := w.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	db, err := enrichment.Read(k, bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Replace(string(k), db); err != nil {
		t.Fatal(err)
	}
}

func TestDatabases(t *testing.T) {
	e := newEngine(t, Policy{DefaultAction: Deny, Rules: []Rule{
		{Name: "hosting", When: `asn.number == 64500`, Action: Deny},
		{Name: "lazio", When: `geo.country.code == "IT" && geo.subdivisions.exists(s, s.code == "62")`, Action: Allow},
		{Name: "europe", When: `geo.country.in_eu`, Action: Allow},
		{Name: "unknown", When: `!geo.found`, Action: Allow},
	}}, Options{Databases: databaseStore(t, mmdbtest.CityDatabase(6), mmdbtest.ASNDatabase())})
	for client, want := range map[string]Decision{
		"198.51.100.7":  {Action: Deny, Enforced: true, Rule: "hosting", Reason: ReasonRule, Response: &forbidden},
		"192.0.2.7":     {Action: Allow, Enforced: true, Rule: "lazio", Reason: ReasonRule},
		"2001:db8::7":   {Action: Allow, Enforced: true, Rule: "europe", Reason: ReasonRule},
		"203.0.113.200": {Action: Allow, Enforced: true, Rule: "unknown", Reason: ReasonRule},
		"203.0.113.7":   {Action: Deny, Enforced: true, Rule: DefaultRule, Reason: ReasonDefault, Response: &forbidden},
		// Without a client address, the deny rule that reads asn denies.
		"": {Action: Deny, Enforced: true, Rule: "hosting", Reason: ReasonError, Cause: expr.CauseClientIPMissing, Errors: []RuleError{{"hosting", expr.CauseClientIPMissing}}, Response: &forbidden},
	} {
		if got := summary(e.Decide(req(client))); !reflect.DeepEqual(got, want) {
			t.Errorf("client %q: %+v\nwant %+v", client, got, want)
		}
	}
	// What Explain reads of geo and asn is what the conditions read.
	x := e.Explain(req("2001:db8::7"))
	if g, err := x.Input.Geo(); err != nil || g.Country.Code != "DE" || len(g.Subdivisions) != 1 {
		t.Errorf("Geo: %+v, %v", g, err)
	}
	if got := x.Attributes; !reflect.DeepEqual(got, []string{"asn.number", "geo.country.code", "geo.country.in_eu", "geo.subdivisions"}) {
		t.Errorf("attributes %v", got)
	}
}

// A lookup that fails makes the conditions that depend on it indeterminate,
// with the cause database_error: a deny rule denies, an allow rule does not
// match.
func TestDatabaseError(t *testing.T) {
	store := databaseStore(t, mmdbtest.BrokenDatabase("geo"), mmdbtest.BrokenDatabase("asn"))
	for _, tc := range []struct {
		name  string
		rules []Rule
		want  Decision
	}{
		{"deny rule denies", []Rule{
			{Name: "not-italy", When: `geo.country.code != "IT"`, Action: Deny},
			{Name: "everyone", When: "true", Action: Allow},
		}, Decision{Action: Deny, Enforced: true, Rule: "not-italy", Reason: ReasonError, Cause: expr.CauseDatabaseError, Errors: []RuleError{{"not-italy", expr.CauseDatabaseError}}, Response: &forbidden}},
		{"allow rule does not match", []Rule{
			{Name: "network", When: `asn.number == 64496`, Action: Allow},
		}, Decision{Action: Deny, Enforced: true, Rule: DefaultRule, Reason: ReasonDefault, IndeterminateRules: []string{"network"}, Errors: []RuleError{{"network", expr.CauseDatabaseError}}, Response: &forbidden}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEngine(t, Policy{DefaultAction: Deny, Rules: tc.rules}, Options{Databases: store})
			if got := summary(e.Decide(req("198.51.100.7"))); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("%+v\nwant %+v", got, tc.want)
			}
			o := e.Explain(req("198.51.100.7")).Outcomes[0]
			if o.Result != ResultIndeterminate || o.Cause != expr.CauseDatabaseError {
				t.Errorf("outcome %+v", o)
			}
		})
	}
}

// A request that a rule decides before any rule reads geo or asn looks
// nothing up: with databases whose every lookup for the address fails, the
// decision has no error.
func TestRuleDecidedEarlierDoesNotConsultTheDatabase(t *testing.T) {
	e := newEngine(t, Policy{DefaultAction: Deny, Rules: []Rule{
		{Name: "office", When: `client.ip.inCidr("198.51.100.0/24") && request.method == "GET"`, Action: Allow},
		{Name: "countries", When: `geo.country.code in ["IT", "FR"]`, Action: Deny},
		{Name: "networks", When: `asn.number == 64496`, Action: Allow},
	}}, Options{Databases: databaseStore(t, mmdbtest.BrokenDatabase("geo"), mmdbtest.BrokenDatabase("asn"))})
	want := Decision{Action: Allow, Enforced: true, Rule: "office", Reason: ReasonRule}
	if got := summary(e.Decide(req("198.51.100.7"))); !reflect.DeepEqual(got, want) {
		t.Errorf("%+v\nwant %+v", got, want)
	}
	// The next rule does consult it, and fails.
	r := req("198.51.100.7")
	r.Method = "POST"
	if got := summary(e.Decide(r)); got.Reason != ReasonError || got.Cause != expr.CauseDatabaseError || got.Rule != "countries" {
		t.Errorf("a decision that reads geo: %+v", got)
	}
}

// One decision reads one copy of each database, whatever replaces it
// meanwhile. Run with -race.
func TestDatabasesReplacedDuringDecisions(t *testing.T) {
	store := databaseStore(t, mmdbtest.CityDatabase(6), mmdbtest.ASNDatabase())
	// Within one copy the first rule allows when the second would deny: the
	// second never decides.
	e := newEngine(t, Policy{DefaultAction: Allow, Rules: []Rule{
		{Name: "rome", When: `geo.city == "Rome"`, Action: Allow},
		{Name: "inconsistent", When: `has(geo.location)`, Action: Deny},
	}}, Options{Databases: store})
	city := store.Snapshot().Database(enrichment.Geo)
	replaceDatabase(t, store, enrichment.Geo, mmdbtest.CountryDatabase())
	country := store.Snapshot().Database(enrichment.Geo)
	stop := make(chan struct{})
	var writer sync.WaitGroup
	writer.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_ = store.Replace("geo", []*enrichment.Database{city, country}[i%2])
		}
	})
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			for range 500 {
				if d := e.Decide(req("192.0.2.7")); d.Rule == "inconsistent" || len(d.Errors) > 0 {
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

// An engine without databases does not compile a condition that reads one.
func TestDatabasesWithoutStore(t *testing.T) {
	if _, err := New(Policy{DefaultAction: Deny, Rules: []Rule{{Name: "geo", When: `geo.found`, Action: Allow}}}, Options{}); err == nil {
		t.Error("New accepted a condition on geo without a database")
	}
}
