package expr

import (
	"strings"
	"testing"

	"github.com/gtriggiano/tessera/internal/enrichment"
	"github.com/gtriggiano/tessera/internal/mmdbtest"
	"github.com/gtriggiano/tessera/internal/sets"
)

// romeArea is an area around Rome, where the example geo database puts
// 192.0.2.0/24; Berlin, San Francisco and Tokyo are outside it.
const romeArea = `{"type":"Polygon","coordinates":[[[12.35,41.80],[12.62,41.80],[12.62,42.00],[12.35,42.00],[12.35,41.80]]]}`

// testAreas returns the store of the area set stores, whose copy is romeArea,
// of the sets of testSets, all with on_unavailable answer, and of the example
// databases, with an Env that declares them all.
func testAreas(t testing.TB, answer sets.OnUnavailable, geo *enrichment.Database) (*sets.Store, *enrichment.Store, *Env) {
	t.Helper()
	setStore, opts := testSets(t, answer)
	decls := append(setStore.Declarations(), sets.Declaration{Name: "stores", Type: sets.Area, Source: sets.SourceFile, OnUnavailable: answer})
	store, err := sets.NewStore(decls)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range setStore.Declarations() {
		e, _ := setStore.Get(d.Name)
		if err := store.Replace(d.Name, e.Set); err != nil {
			t.Fatal(err)
		}
	}
	area, err := sets.Read(sets.Area, strings.NewReader(romeArea), 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Replace("stores", area); err != nil {
		t.Fatal(err)
	}
	dbs, err := enrichment.NewStore(enrichment.Kinds)
	if err != nil {
		t.Fatal(err)
	}
	for _, db := range []*enrichment.Database{geo, loadDatabase(t, enrichment.ASN, mmdbtest.ASNDatabase())} {
		if err := dbs.Replace(string(db.Kind()), db); err != nil {
			t.Fatal(err)
		}
	}
	opts.Sets, opts.Databases = decls, enrichment.Kinds
	return store, dbs, newEnv(t, opts)
}

func evalArea(t *testing.T, env *Env, store *sets.Store, dbs *enrichment.Store, text, clientIP string) (bool, error) {
	t.Helper()
	return mustCompile(t, env, text).Eval(NewInput(sampleRequest(clientIP), store.Snapshot(), dbs.Snapshot(), nil))
}

// The exit criterion of the areas: a point inside and one outside.
func TestAreaContains(t *testing.T) {
	store, dbs, env := testAreas(t, sets.Fail, loadDatabase(t, enrichment.Geo, mmdbtest.CityDatabase(6)))
	for _, tc := range []struct {
		client string
		want   bool
	}{
		{"192.0.2.7", true},            // Rome, inside
		{"::ffff:192.0.2.7", true},     // the same, IPv4-mapped
		{"2001:db8::7", false},         // Berlin, outside
		{"198.51.100.7", false},        // San Francisco, outside
		{"203.0.113.7", false},         // Tokyo: a record without a location
		{"203.0.113.70", false},        // a record without a country nor a location
		{"203.0.113.200", false},       // an address that the database does not have
		{"2001:db8:ffff::7", false},    // nor this one
		{"2001:db8:0:ffff::7", false},  // Berlin again
		{"2001:db8:1::7", false},       // not in the database
		{"198.51.100.255", false},      // San Francisco again
		{"192.0.2.255", true},          // Rome again
		{"::ffff:198.51.100.7", false}, // San Francisco, IPv4-mapped
	} {
		got, err := evalArea(t, env, store, dbs, `sets.stores.contains(geo.location)`, tc.client)
		if got != tc.want || err != nil {
			t.Errorf("client %s: %v, %v; want %v", tc.client, got, err, tc.want)
		}
	}
	// Without a location, contains is false without an error, so its
	// negation is true; every other read of the absent location still
	// fails, and has() still says it is absent.
	for text, want := range map[string]bool{
		`!sets.stores.contains(geo.location)`:                            true,
		`sets.stores.contains(geo.location) || geo.country.code == "JP"`: true,
		`sets.stores.contains(geo.location) && true`:                     false,
		`!has(geo.location) && !sets.stores.contains(geo.location)`:      true,
		`sets.stores.contains(true ? geo.location : geo.location)`:       false,
	} {
		if got, err := evalArea(t, env, store, dbs, text, "203.0.113.7"); got != want || err != nil {
			t.Errorf("without a location: %s = %v, %v; want %v", text, got, err, want)
		}
	}
	for _, text := range []string{`geo.location.lat > 0.0`, `geo.location == geo.location`, `sets.stores.contains(geo.location) || geo.location.lat > 0.0`} {
		if _, err := evalArea(t, env, store, dbs, text, "203.0.113.7"); cause(err) != CauseExpression {
			t.Errorf("without a location: %s: %v, want the cause expression", text, err)
		}
	}
	// Without a client address, the condition is indeterminate for that
	// reason, not false.
	for _, text := range []string{`sets.stores.contains(geo.location)`, `!sets.stores.contains(geo.location)`} {
		if _, err := evalArea(t, env, store, dbs, text, ""); cause(err) != CauseClientIPMissing {
			t.Errorf("without a client address: %s: %v", text, err)
		}
	}
}

// A lookup that fails is a database error, not an absent location.
func TestAreaDatabaseError(t *testing.T) {
	store, dbs, env := testAreas(t, sets.Fail, loadDatabase(t, enrichment.Geo, mmdbtest.BrokenDatabase("geo")))
	for _, text := range []string{`sets.stores.contains(geo.location)`, `!sets.stores.contains(geo.location)`} {
		if _, err := evalArea(t, env, store, dbs, text, "198.51.100.7"); cause(err) != CauseDatabaseError {
			t.Errorf("%s: %v, want the cause database_error", text, err)
		}
	}
}

// An unavailable area set answers what on_unavailable says for a point; an
// absent location is in no area, whatever the set.
func TestAreaUnavailable(t *testing.T) {
	for _, tc := range []struct {
		answer sets.OnUnavailable
		want   bool
		cause  Cause
	}{
		{sets.Fail, false, CauseSetUnavailable},
		{sets.Match, true, ""},
		{sets.NoMatch, false, ""},
	} {
		store, dbs, env := testAreas(t, tc.answer, loadDatabase(t, enrichment.Geo, mmdbtest.CityDatabase(6)))
		if err := store.SetAvailable("stores", false); err != nil {
			t.Fatal(err)
		}
		for _, client := range []string{"192.0.2.7", "2001:db8::7"} {
			got, err := evalArea(t, env, store, dbs, `sets.stores.contains(geo.location)`, client)
			if got != tc.want || cause(err) != tc.cause {
				t.Errorf("on_unavailable %s, client %s: %v, %v; want %v, cause %q", tc.answer, client, got, err, tc.want, tc.cause)
			}
		}
		if got, err := evalArea(t, env, store, dbs, `sets.stores.contains(geo.location)`, "203.0.113.7"); got || err != nil {
			t.Errorf("on_unavailable %s, without a location: %v, %v", tc.answer, got, err)
		}
	}
}

// contains is strict for every argument but the absent location: the other
// overloads, and the standard contains on strings, still fail with the
// error of their argument.
func TestContainsStaysStrict(t *testing.T) {
	store, dbs, env := testAreas(t, sets.Fail, loadDatabase(t, enrichment.Geo, mmdbtest.CityDatabase(6)))
	for _, tc := range []struct {
		text   string
		client string
		cause  Cause
	}{
		{`sets.office.contains(client.ip)`, "", CauseClientIPMissing},
		{`!sets.office.contains(client.ip)`, "", CauseClientIPMissing},
		{`sets.asns.contains(asn.number)`, "", CauseClientIPMissing},
		{`sets.asns.contains(int(request.headers["x-asn"]))`, "192.0.2.7", CauseExpression},
		{`sets.hosts.contains(request.headers["x-host"])`, "192.0.2.7", CauseExpression},
		{`sets.hosts.contains(geo.city)`, "", CauseClientIPMissing},
		{`!sets.hosts.contains(geo.city)`, "", CauseClientIPMissing},
		{`request.headers["x-host"].contains("example")`, "192.0.2.7", CauseExpression},
		{`!request.headers["x-host"].contains("example")`, "192.0.2.7", CauseExpression},
		{`string(client.ip).contains("192")`, "", CauseClientIPMissing},
		{`request.host.contains(request.headers["x-part"])`, "192.0.2.7", CauseExpression},
		{`string(geo.location.lat).contains("41")`, "203.0.113.7", CauseExpression},
		// Dispatched at run time, among every overload of contains.
		{`dyn(request.headers["x-host"]).contains("example")`, "192.0.2.7", CauseExpression},
		{`dyn(string(client.ip)).contains("192")`, "", CauseClientIPMissing},
		{`request.host.contains(dyn(request.headers["x-part"]))`, "192.0.2.7", CauseExpression},
	} {
		got, err := evalArea(t, env, store, dbs, tc.text, tc.client)
		if got || cause(err) != tc.cause {
			t.Errorf("%s, client %q: %v, %v; want the cause %s", tc.text, tc.client, got, err, tc.cause)
		}
	}
	// With their arguments, they answer.
	for _, text := range []string{
		`sets.office.contains(client.ip) && sets.asns.contains(asn.number) && sets.hosts.contains(request.host)`,
		`request.host.contains("example") && string(client.ip).contains("192")`,
	} {
		if got, err := evalArea(t, env, store, dbs, text, "192.0.2.7"); !got || err != nil {
			t.Errorf("%s: %v, %v", text, got, err)
		}
	}
}

func TestAreaCompileErrors(t *testing.T) {
	_, _, env := testAreas(t, sets.Fail, loadDatabase(t, enrichment.Geo, mmdbtest.CityDatabase(6)))
	for text, want := range map[string]string{
		`sets.stores.contains(client.ip)`:        "found no matching overload for 'contains' applied to 'tessera.AreaSet.(tessera.IP)'",
		`sets.stores.contains(geo.location.lat)`: "found no matching overload for 'contains' applied to 'tessera.AreaSet.(double)'",
		`sets.office.contains(geo.location)`:     "found no matching overload for 'contains' applied to 'tessera.CIDRSet.(tessera.GeoPoint)'",
		`has(sets.stores)`:                       errSetUse.Error(),
		`sets.stores == sets.stores`:             errSetUse.Error(),
	} {
		if got := compileIssue(t, env, text); got.Message != want {
			t.Errorf("%s: %q, want %q", text, got.Message, want)
		}
	}
	// Without enrichment.geo, geo.location is not available.
	noGeo := newEnv(t, Options{Sets: []sets.Declaration{{Name: "stores", Type: sets.Area, Source: sets.SourceFile, OnUnavailable: sets.Fail}}})
	if got := compileIssue(t, noGeo, `sets.stores.contains(geo.location)`); got.Message != "geo is not available: the configuration has no enrichment.geo" || got.Column != 22 {
		t.Errorf("without enrichment.geo: %+v", got)
	}
	c := mustCompile(t, env, `sets.stores.contains(geo.location)`)
	if got := c.Sets(); len(got) != 1 || got[0] != "stores" {
		t.Errorf("Sets() = %v", got)
	}
	if got := c.Attributes(); len(got) != 1 || got[0] != "geo.location" {
		t.Errorf("Attributes() = %v", got)
	}
}

// contains on an area set costs one unit, whatever the size of the area.
func TestAreaContainsCost(t *testing.T) {
	store, dbs, env := testAreas(t, sets.Fail, loadDatabase(t, enrichment.Geo, mmdbtest.CityDatabase(6)))
	c := mustCompile(t, env, `sets.stores.contains(geo.location)`)
	for _, client := range []string{"192.0.2.7", "203.0.113.7"} {
		_, details, err := c.prg.Eval(NewInput(sampleRequest(client), store.Snapshot(), dbs.Snapshot(), nil))
		if err != nil || details == nil || details.ActualCost() == nil || *details.ActualCost() > 10 {
			t.Errorf("client %s: cost %v, %v", client, details, err)
		}
	}
}
