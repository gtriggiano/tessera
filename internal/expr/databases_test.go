package expr

import (
	"bytes"
	"errors"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"

	"github.com/gtriggiano/tessera/internal/enrichment"
	"github.com/gtriggiano/tessera/internal/mmdbtest"
	"github.com/gtriggiano/tessera/internal/request"
)

// loadDatabase reads the database of w, of kind k.
func loadDatabase(t testing.TB, k enrichment.Kind, w *mmdbtest.Writer) *enrichment.Database {
	t.Helper()
	data, err := w.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	db, err := enrichment.Read(k, bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// testDatabases returns a store of both databases, with the copies geo and
// asn, and an Env that declares them.
func testDatabases(t testing.TB, geo, asn *enrichment.Database) (*enrichment.Store, *Env) {
	t.Helper()
	store, err := enrichment.NewStore(enrichment.Kinds)
	if err != nil {
		t.Fatal(err)
	}
	for _, db := range []*enrichment.Database{geo, asn} {
		if err := store.Replace(string(db.Kind()), db); err != nil {
			t.Fatal(err)
		}
	}
	return store, newEnv(t, Options{Databases: enrichment.Kinds})
}

func exampleDatabases(t testing.TB) (*enrichment.Store, *Env) {
	t.Helper()
	return testDatabases(t, loadDatabase(t, enrichment.Geo, mmdbtest.CityDatabase(6)), loadDatabase(t, enrichment.ASN, mmdbtest.ASNDatabase()))
}

func evalDB(t *testing.T, env *Env, store *enrichment.Store, text, clientIP string) (bool, error) {
	t.Helper()
	return mustCompile(t, env, text).Eval(NewInput(sampleRequest(clientIP), nil, store.Snapshot(), nil))
}

func TestGeoAndASN(t *testing.T) {
	store, env := exampleDatabases(t)
	for _, tc := range []struct {
		client string
		exprs  []string
	}{
		{"192.0.2.7", []string{
			`geo.found && geo.country.code == "IT" && geo.country.name == "Italy" && geo.country.in_eu`,
			`geo.continent.code == "EU" && geo.continent.name == "Europe"`,
			`size(geo.subdivisions) == 2 && geo.subdivisions[0].code == "62" && geo.subdivisions[0].name == "Lazio"`,
			`geo.subdivisions[1].code == "RM" && geo.subdivisions[1].name == "Rome"`,
			`geo.subdivisions.map(s, s.code) == ["62", "RM"] && geo.subdivisions.map(s, s.name) == ["Lazio", "Rome"]`,
			`geo.country.code == "IT" && geo.subdivisions.exists(s, s.code == "RM")`,
			`!geo.subdivisions.exists(s, s.code == "CA") && geo.subdivisions.all(s, s.name != "")`,
			`geo.city == "Rome" && geo.postal_code == "00118" && geo.timezone == "Europe/Rome"`,
			`has(geo.location) && geo.location.lat == 41.8919 && geo.location.lon == 12.5113`,
			`geo.location.lat > 41.0 && geo.location == geo.location`,
			`geo.country.code in ["IT", "FR"] || geo.country.in_eu`,
			`has(geo.country) && has(geo.country.code) && has(geo.subdivisions) && has(geo.postal_code)`,
			`asn.found && asn.number == 64496 && asn.organization == "Example Transit"`,
		}},
		{"::ffff:198.51.100.7", []string{
			`geo.country.code == "US" && geo.country.name == "United States" && !geo.country.in_eu`,
			`geo.continent.code == "NA" && geo.continent.name == "North America"`,
			`geo.subdivisions.map(s, s.code) == ["CA"] && geo.subdivisions[0].name == "California"`,
			`geo.postal_code == "94118" && geo.location.lon < 0.0`,
			`asn.number == 64500`,
		}},
		// A record without subdivisions, city, postal code and location,
		// then one without a country.
		{"203.0.113.7", []string{
			`geo.found && geo.country.code == "JP" && geo.city == "" && geo.postal_code == "" && !has(geo.location)`,
			`size(geo.subdivisions) == 0 && geo.subdivisions == [] && !geo.subdivisions.exists(s, s.code == "13")`,
			`!asn.found && asn.number == 0 && asn.organization == ""`,
		}},
		{"203.0.113.70", []string{
			`geo.found && geo.country.code == "" && geo.country.name == "" && !geo.country.in_eu`,
			`geo.continent.code == "" && geo.continent.name == ""`,
		}},
		// An address that no database has: not an error.
		{"203.0.113.200", []string{
			`!geo.found && geo.country.code == "" && geo.country.name == "" && !geo.country.in_eu`,
			`geo.continent.code == "" && geo.continent.name == "" && geo.subdivisions == []`,
			`geo.city == "" && geo.postal_code == "" && geo.timezone == "" && !has(geo.location)`,
			`!(geo.country.code in ["IT"]) && !geo.subdivisions.exists(s, s.code == "RM")`,
			`!asn.found && asn.number == 0`,
		}},
		{"2001:db8::7", []string{
			`geo.country.code == "DE" && geo.subdivisions[0].code == "BE" && geo.subdivisions[0].name == "State of Berlin"`,
			`geo.postal_code == "10178" && asn.number == 64511`,
		}},
	} {
		for _, text := range tc.exprs {
			if got, err := evalDB(t, env, store, text, tc.client); !got || err != nil {
				t.Errorf("client %s: %s = %v, %v; want true", tc.client, text, got, err)
			}
		}
	}

	// geo.location absent and read, and an index beyond the subdivisions,
	// are ordinary evaluation errors.
	for _, text := range []string{`geo.location.lat > 0.0`, `geo.location == geo.location`, `!(geo.location.lon > 0.0)`, `geo.subdivisions[0].code == "13"`} {
		if _, err := evalDB(t, env, store, text, "203.0.113.7"); cause(err) != CauseExpression {
			t.Errorf("%s for a record without it: %v, want cause expression", text, err)
		}
	}
	if got, err := evalDB(t, env, store, `has(geo.location) && geo.location.lat > 0.0`, "203.0.113.7"); got || err != nil {
		t.Errorf("guarded location: %v, %v", got, err)
	}
}

// The objects of geo compare by their fields: two values with the same
// fields are equal, whatever the decision they come from; geo itself
// compares by identity.
func TestGeoEquality(t *testing.T) {
	store, env := exampleDatabases(t)
	// Two subdivisions of one record with the same fields are equal.
	w := mmdbtest.New("GeoIP2-City", 1)
	if err := w.Insert(netip.MustParsePrefix("192.0.2.0/24"), map[string]any{
		"subdivisions": []any{
			map[string]any{"iso_code": "RM", "names": map[string]any{"en": "Rome"}},
			map[string]any{"iso_code": "RM", "names": map[string]any{"en": "Rome"}},
			map[string]any{"iso_code": "RM", "names": map[string]any{"en": "Roma"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	twins, twinsEnv := testDatabases(t, loadDatabase(t, enrichment.Geo, w), loadDatabase(t, enrichment.ASN, mmdbtest.ASNDatabase()))
	for _, text := range []string{
		`geo.subdivisions[0] == geo.subdivisions[1] && geo.subdivisions[0] != geo.subdivisions[2]`,
		`geo.subdivisions[2] in geo.subdivisions && [geo.subdivisions[1]] == [geo.subdivisions[0]]`,
	} {
		if got, err := evalDB(t, twinsEnv, twins, text, "192.0.2.7"); !got || err != nil {
			t.Errorf("%s: %v, %v", text, got, err)
		}
	}
	for _, text := range []string{
		`geo.country == geo.country && geo.continent == geo.continent && geo.location == geo.location`,
		`geo.subdivisions == geo.subdivisions && geo.subdivisions[0] != geo.subdivisions[1]`,
		`geo == geo && dyn(geo.country) != dyn(geo.continent) && dyn(geo.subdivisions[0]) != dyn(geo.location)`,
	} {
		if got, err := evalDB(t, env, store, text, "192.0.2.7"); !got || err != nil {
			t.Errorf("%s: %v, %v", text, got, err)
		}
	}

	// The values of two decisions: the objects equal by their fields,
	// geo does not.
	read := func(client string) *geoVal {
		v, _ := NewInput(sampleRequest(client), nil, store.Snapshot(), nil).ResolveName("geo")
		g, ok := v.(*geoVal)
		if !ok {
			t.Fatalf("geo for %s: %v", client, v)
		}
		return g
	}
	field := func(g *geoVal, name string) ref.Val {
		v, err := geoFields[name].get(g)
		if err != nil {
			t.Fatal(err)
		}
		return v.(ref.Val)
	}
	a, b, other := read("192.0.2.7"), read("192.0.2.8"), read("2001:db8::7")
	for _, name := range []string{"country", "continent", "subdivisions", "location"} {
		if field(a, name) == field(b, name) {
			t.Fatalf("%s: one value for two decisions", name)
		}
		if got := field(a, name).Equal(field(b, name)); got != types.True {
			t.Errorf("%s of the same record in two decisions: %v", name, got)
		}
	}
	for _, name := range []string{"country", "subdivisions", "location"} {
		if got := field(a, name).Equal(field(other, name)); got != types.False {
			t.Errorf("%s of two records: %v", name, got)
		}
	}
	if got := field(a, "continent").Equal(field(other, "continent")); got != types.True {
		t.Errorf("continent of two records in Europe: %v", got)
	}
	if got := a.Equal(b); got != types.False {
		t.Errorf("geo of two decisions: %v", got)
	}
	if got := field(a, "country").Equal(types.String("IT")); got != types.False {
		t.Errorf("a country and a string: %v", got)
	}
}

// Without a client address every read of geo or asn fails with the cause
// client_ip_missing, has() included: !has(geo.location) cannot let a deny
// rule open.
func TestDatabasesWithoutClientAddress(t *testing.T) {
	store, env := exampleDatabases(t)
	for _, text := range unreadable {
		_, err := evalDB(t, env, store, text, "")
		if cause(err) != CauseClientIPMissing || !errors.Is(err, ErrClientIPMissing) {
			t.Errorf("%s without a client address: %v, want cause client_ip_missing", text, err)
		}
	}
	// && and || absorb it when the other side decides.
	if got, err := evalDB(t, env, store, `has(client.ip) && geo.country.code == "IT"`, ""); got || err != nil {
		t.Errorf("guarded: %v, %v", got, err)
	}
}

// unreadable are conditions that read geo or asn in every way: none can
// be evaluated without the record of the client address.
var unreadable = []string{
	`geo.found`,
	`!geo.found`,
	`geo == geo`,
	`geo.country.code == "IT"`,
	`geo.country.name == ""`,
	`geo.country.in_eu`,
	`!geo.country.in_eu`,
	`geo.country == geo.country`,
	`has(geo.country)`,
	`has(geo.country.code)`,
	`geo.continent.code == "EU"`,
	`geo.continent.name == ""`,
	`geo.continent == geo.continent`,
	`geo.subdivisions == []`,
	`size(geo.subdivisions) == 0`,
	`has(geo.subdivisions)`,
	`geo.subdivisions.exists(s, s.code == "RM")`,
	`!geo.subdivisions.exists(s, s.code == "RM")`,
	`geo.subdivisions.all(s, s.code != "RM")`,
	`geo.subdivisions[0].name == "Lazio"`,
	`geo.city == ""`,
	`geo.postal_code == ""`,
	`has(geo.postal_code)`,
	`geo.timezone == ""`,
	`has(geo.location)`,
	`!has(geo.location)`,
	`has(geo.location.lat)`,
	`geo.location.lat > 0.0`,
	`geo.location == geo.location`,
	`asn.found`,
	`asn.number == 0`,
	`!asn.found`,
	`has(asn.organization)`,
}

// A lookup that fails is the cause database_error, never an address that
// the database does not have.
func TestDatabaseError(t *testing.T) {
	store, env := testDatabases(t, loadDatabase(t, enrichment.Geo, mmdbtest.BrokenDatabase("geo")), loadDatabase(t, enrichment.ASN, mmdbtest.BrokenDatabase("asn")))
	for _, text := range unreadable {
		_, err := evalDB(t, env, store, text, "198.51.100.7")
		if cause(err) != CauseDatabaseError || !errors.Is(err, ErrDatabase) || err.Error() != "indeterminate condition: database_error" {
			t.Errorf("%s: %v, want cause database_error", text, err)
		}
	}
	if got, err := evalDB(t, env, store, `geo.country.code == "IT" && asn.number == 64496`, "192.0.2.7"); !got || err != nil {
		t.Errorf("a valid record of the same database: %v, %v", got, err)
	}
	// Without a copy in the snapshot, the read fails the same way.
	_, err := mustCompile(t, env, `geo.found`).Eval(NewInput(sampleRequest("192.0.2.7"), nil, nil, nil))
	if cause(err) != CauseDatabaseError {
		t.Errorf("no database: %v", err)
	}
}

// A condition that does not read geo, or that decides before reading it,
// looks nothing up: with a database whose every lookup fails, it succeeds.
// The conditions of one Input that read geo look the address up once, in
// the copy of the snapshot that the Input was given.
func TestLookupOncePerDecision(t *testing.T) {
	store, env := testDatabases(t, loadDatabase(t, enrichment.Geo, mmdbtest.BrokenDatabase("geo")), loadDatabase(t, enrichment.ASN, mmdbtest.ASNDatabase()))
	in := NewInput(sampleRequest("198.51.100.7"), nil, store.Snapshot(), nil)
	for _, text := range []string{
		`request.host == "api.example.com"`,
		`request.host == "api.example.com" || geo.found`,
		`!(request.host == "www.example.com" && geo.country.code == "IT")`,
		`asn.number == 64500`,
	} {
		if got, err := mustCompile(t, env, text).Eval(in); !got || err != nil {
			t.Fatalf("%s: %v, %v", text, got, err)
		}
	}
	if in.geo != nil {
		t.Fatal("geo was looked up by conditions that do not read it")
	}
	if _, err := mustCompile(t, env, `geo.found`).Eval(in); cause(err) != CauseDatabaseError {
		t.Fatalf("geo.found: %v", err)
	}

	// The copy of the decision stays the one it started with.
	store, env = exampleDatabases(t)
	in = NewInput(sampleRequest("192.0.2.7"), nil, store.Snapshot(), nil)
	if got, err := mustCompile(t, env, `geo.city == "Rome"`).Eval(in); !got || err != nil {
		t.Fatalf("before the replacement: %v, %v", got, err)
	}
	first := in.geo
	if err := store.Replace("geo", loadDatabase(t, enrichment.Geo, mmdbtest.CountryDatabase())); err != nil {
		t.Fatal(err)
	}
	if got, err := mustCompile(t, env, `geo.city == "Rome" && has(geo.location)`).Eval(in); !got || err != nil || in.geo != first {
		t.Errorf("same decision, after the replacement: %v, %v", got, err)
	}
	if got, err := evalDB(t, env, store, `geo.city == ""`, "192.0.2.7"); !got || err != nil {
		t.Errorf("next decision: %v, %v; want the new copy", got, err)
	}
	// What eval shows is what the conditions read.
	if r, err := in.Geo(); err != nil || r.City != "Rome" {
		t.Errorf("Geo: %+v, %v", r, err)
	}
	if r, err := in.ASN(); err != nil || r.Number != 64496 {
		t.Errorf("ASN: %+v, %v", r, err)
	}
	if _, err := NewInput(sampleRequest(""), nil, store.Snapshot(), nil).Geo(); !errors.Is(err, ErrClientIPMissing) {
		t.Errorf("Geo without an address: %v", err)
	}
}

func TestDatabasesCompileErrors(t *testing.T) {
	_, env := exampleDatabases(t)
	for _, tc := range []struct {
		expr         string
		line, column int
		prefix       string
	}{
		{`geo.countr.code == "IT"`, 1, 4, "undefined field 'countr'"},
		{`geo.country.iso_code == "IT"`, 1, 12, "undefined field 'iso_code'"},
		{`geo.country.code == 1`, 1, 18, "found no matching overload for '_==_' applied to '(string, int)'"},
		{`geo.location.lat == 41`, 1, 18, "found no matching overload for '_==_' applied to '(double, int)'"},
		{`geo.location == "41,12"`, 1, 14, "found no matching overload for '_==_' applied to '(tessera.GeoPoint, string)'"},
		{`geo.continent == geo.country`, 1, 15, "found no matching overload for '_==_' applied to '(tessera.GeoContinent, tessera.GeoCountry)'"},
		{`"RM" in geo.subdivisions`, 1, 6, "found no matching overload for '@in' applied to '(string, list(tessera.GeoSubdivision))'"},
		{`geo.subdivisions[0] == "62"`, 1, 21, "found no matching overload for '_==_' applied to '(tessera.GeoSubdivision, string)'"},
		{`geo.subdivisions.code == "62"`, 1, 17, "type 'list(tessera.GeoSubdivision)' does not support field selection"},
		{`asn.number == "64496"`, 1, 12, "found no matching overload for '_==_' applied to '(int, string)'"},
		{`geo.country`, 1, 1, "the condition must be a bool, not tessera.GeoCountry"},
		{`geo.subdivisions`, 1, 1, "the condition must be a bool, not list(tessera.GeoSubdivision)"},
		// The attributes of the earlier shape of geo.
		{`geo.country == "IT"`, 1, 13, "found no matching overload for '_==_' applied to '(tessera.GeoCountry, string)'"},
		{`geo.continent == "EU"`, 1, 15, "found no matching overload for '_==_' applied to '(tessera.GeoContinent, string)'"},
		{`geo.subdivision == "IT-62"`, 1, 4, "undefined field 'subdivision'"},
		{`geo.in_eu`, 1, 4, "undefined field 'in_eu'"},
		{`geo.location == tessera.Point{lat: 1.0, lon: 2.0}`, 1, 30, "undeclared reference to 'tessera.Point'"},
	} {
		is := compileIssue(t, env, tc.expr)
		if is.Line != tc.line || is.Column != tc.column || !strings.HasPrefix(is.Message, tc.prefix) {
			t.Errorf("%q: line %d, column %d: %q; want line %d, column %d: %q…", tc.expr, is.Line, is.Column, is.Message, tc.line, tc.column, tc.prefix)
		}
	}
	// Each variable needs its database.
	geoOnly := newEnv(t, Options{Databases: []enrichment.Kind{enrichment.Geo}})
	if _, err := geoOnly.Compile(`geo.found`); err != nil {
		t.Errorf("geo with its database: %v", err)
	}
	for _, tc := range []struct {
		env          *Env
		expr         string
		line, column int
		message      string
	}{
		{geoOnly, `geo.found && asn.number == 64496`, 1, 14, "asn is not available: the configuration has no enrichment.asn"},
		{newEnv(t, Options{}), "request.host == \"x\" &&\n  has(geo.location)", 2, 7, "geo is not available: the configuration has no enrichment.geo"},
	} {
		is := compileIssue(t, tc.env, tc.expr)
		if is.Line != tc.line || is.Column != tc.column || is.Message != tc.message {
			t.Errorf("%q: line %d, column %d: %q; want line %d, column %d: %q", tc.expr, is.Line, is.Column, is.Message, tc.line, tc.column, tc.message)
		}
	}
	// An iteration variable named geo is not the variable geo.
	for _, text := range []string{`[1, 2].exists(geo, geo == 2)`, `["a"].exists(asn, asn == "a")`} {
		if _, err := newEnv(t, Options{}).Compile(text); err != nil {
			t.Errorf("%s: %v", text, err)
		}
	}
}

func TestDatabasesRead(t *testing.T) {
	_, env := exampleDatabases(t)
	for text, want := range map[string]struct{ attributes, databases string }{
		`geo.country.code == "IT" && asn.number == 64496`:                        {`asn.number geo.country.code`, `asn geo`},
		`geo.country.in_eu || geo.continent.name == "Europe"`:                    {`geo.continent.name geo.country.in_eu`, `geo`},
		`geo.country == geo.country && has(geo.continent.code)`:                  {`geo.continent.code geo.country`, `geo`},
		`[geo.country][0].code == "IT" && has(geo.continent)`:                    {`geo.continent geo.country`, `geo`},
		`geo.country.code == "IT" && geo.subdivisions.exists(s, s.code == "RM")`: {`geo.country.code geo.subdivisions`, `geo`},
		`size(geo.subdivisions) > 1 && geo.subdivisions[1].name == "Rome"`:       {`geo.subdivisions`, `geo`},
		`geo.subdivisions.map(s, s.name)[0] == geo.city`:                         {`geo.city geo.subdivisions`, `geo`},
		`geo.postal_code.startsWith("00") && geo.timezone != ""`:                 {`geo.postal_code geo.timezone`, `geo`},
		`has(geo.location) && geo.location.lat > 1.0`:                            {`geo.location geo.location.lat`, `geo`},
		`has(geo.location.lon)`:                                                  {`geo.location.lon`, `geo`},
		`geo.location == geo.location`:                                           {`geo.location`, `geo`},
		`geo == geo`:                                                             {``, `geo`},
		`!asn.found || asn.organization.startsWith("Example")`:                   {`asn.found asn.organization`, `asn`},
		`[1].exists(geo, geo == 1) && client.ip.inCidr("192.0.2.0/24")`:          {`client.ip`, ``},
	} {
		c := mustCompile(t, env, text)
		if got := strings.Join(c.Attributes(), " "); got != want.attributes {
			t.Errorf("%s: attributes %q, want %q", text, got, want.attributes)
		}
		var kinds []string
		for _, k := range c.Databases() {
			kinds = append(kinds, string(k))
		}
		if got := strings.Join(kinds, " "); got != want.databases {
			t.Errorf("%s: databases %q, want %q", text, got, want.databases)
		}
	}
}

// Decisions run while the copies are replaced: each reads one copy. Run with
// -race.
func TestDatabasesConcurrentReplacement(t *testing.T) {
	store, env := exampleDatabases(t)
	city, country := store.Snapshot().Database(enrichment.Geo), loadDatabase(t, enrichment.Geo, mmdbtest.CountryDatabase())
	// Both read geo: in one decision they agree.
	first := mustCompile(t, env, `geo.city == "Rome"`)
	second := mustCompile(t, env, `has(geo.location)`)
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
			for range 2000 {
				in := NewInput(&request.Request{ClientIP: sampleRequest("192.0.2.7").ClientIP}, nil, store.Snapshot(), nil)
				a, errA := first.Eval(in)
				b, errB := second.Eval(in)
				if a != b || errA != nil || errB != nil {
					t.Errorf("one decision saw two copies: %v %v, %v %v", a, errA, b, errB)
					return
				}
			}
		})
	}
	workers.Wait()
	close(stop)
	writer.Wait()
}
