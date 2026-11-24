package expr

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/gtriggiano/tessera/internal/enrichment"
	"github.com/gtriggiano/tessera/internal/mmdbtest"
	"github.com/gtriggiano/tessera/internal/request"
	"github.com/gtriggiano/tessera/internal/sets"
)

func mustCompileString(t testing.TB, env *Env, text string) *StringExpr {
	t.Helper()
	s, err := env.CompileString(text)
	if err != nil {
		t.Fatalf("CompileString(%q): %v", text, err)
	}
	return s
}

// The value of an upstream header or of a decision field is a string
// expression: it reads every attribute that a condition reads, and its
// evaluation fails with the causes of a condition.
func TestStringExpressions(t *testing.T) {
	setStore, opts := testSets(t, sets.Fail)
	dbStore, dbEnv := exampleDatabases(t)
	opts.Databases = enrichment.Kinds
	env := newEnv(t, opts)
	snapshot := func() *sets.Snapshot { return setStore.Snapshot() }
	for _, tc := range []struct {
		text, client, want string
	}{
		{`request.host`, "192.0.2.10", "api.example.com"},
		{`request.query`, "192.0.2.10", "page=2"},
		{`request.headers["x-api-key"]`, "192.0.2.10", "k-123"},
		{`string(client.ip)`, "::ffff:192.0.2.10", "192.0.2.10"},
		{`has(client.ip) ? string(client.ip) : ""`, "", ""},
		{`sets.office.contains(client.ip) ? "office" : "other"`, "192.0.2.10", "office"},
		{`geo.country.code`, "192.0.2.7", "IT"},
		{`geo.country.name + "/" + geo.subdivisions[0].name + "/" + geo.subdivisions[1].code + "/" + geo.city`, "192.0.2.7", "Italy/Lazio/RM/Rome"},
		{`geo.postal_code + " " + geo.continent.name`, "192.0.2.7", "00118 Europe"},
		{`size(geo.subdivisions) > 0 ? geo.subdivisions[size(geo.subdivisions) - 1].code : ""`, "203.0.113.7", ""},
		{`asn.found ? string(asn.number) : ""`, "192.0.2.7", "64496"},
		{`asn.found ? string(asn.number) : ""`, "203.0.113.200", ""},
		{`has(geo.location) ? string(geo.location.lat) : "none"`, "203.0.113.7", "none"},
	} {
		got, err := mustCompileString(t, env, tc.text).Eval(NewInput(sampleRequest(tc.client), snapshot(), dbStore.Snapshot(), nil))
		if got != tc.want || err != nil {
			t.Errorf("%s for %q = %q, %v; want %q", tc.text, tc.client, got, err, tc.want)
		}
	}
	if err := setStore.SetAvailable("office", false); err != nil {
		t.Fatal(err)
	}
	broken, brokenEnv := testDatabases(t, loadDatabase(t, enrichment.Geo, mmdbtest.BrokenDatabase("geo")), loadDatabase(t, enrichment.ASN, mmdbtest.BrokenDatabase("asn")))
	for _, tc := range []struct {
		env    *Env
		dbs    *enrichment.Store
		text   string
		client string
		cause  Cause
	}{
		{env, dbStore, `string(client.ip)`, "", CauseClientIPMissing},
		{env, dbStore, `geo.country.code`, "", CauseClientIPMissing},
		{env, dbStore, `geo.postal_code`, "", CauseClientIPMissing},
		{env, dbStore, `request.headers["x-missing"]`, "192.0.2.10", CauseExpression},
		{env, dbStore, `string(geo.location.lat)`, "203.0.113.7", CauseExpression},
		{env, dbStore, `geo.subdivisions[0].code`, "203.0.113.7", CauseExpression},
		{env, dbStore, `sets.office.contains(client.ip) ? "a" : "b"`, "192.0.2.10", CauseSetUnavailable},
		{brokenEnv, broken, `geo.country.code`, "198.51.100.7", CauseDatabaseError},
		{brokenEnv, broken, `geo.subdivisions[0].name`, "198.51.100.7", CauseDatabaseError},
		{brokenEnv, broken, `asn.organization`, "198.51.100.7", CauseDatabaseError},
		{withPanics(t, dbEnv), dbStore, `client.ip.boom() ? "a" : "b"`, "192.0.2.10", CausePanic},
	} {
		got, err := mustCompileString(t, tc.env, tc.text).Eval(NewInput(sampleRequest(tc.client), snapshot(), tc.dbs.Snapshot(), nil))
		if got != "" || cause(err) != tc.cause {
			t.Errorf("%s for %q = %q, %v; want the cause %s", tc.text, tc.client, got, err, tc.cause)
		}
	}
	// && and || absorb a failure, as in a condition.
	if got, err := mustCompileString(t, env, `has(client.ip) && client.ip.family() == 4 ? "4" : "-"`).Eval(NewInput(sampleRequest(""), nil, nil, nil)); got != "-" || err != nil {
		t.Errorf("absorbed failure: %q, %v", got, err)
	}
}

func TestStringCompileErrors(t *testing.T) {
	env := newEnv(t, Options{})
	for _, tc := range []struct {
		expr         string
		line, column int
		prefix       string
	}{
		{`request.host == "x"`, 1, 1, "the expression must be a string, not bool"},
		{`size(request.path)`, 1, 1, "the expression must be a string, not int"},
		{`dyn(request.host)`, 1, 1, "the expression must be a string, not dyn"},
		{"request.host +\n  request.hots", 2, 10, "undefined field 'hots'"},
		{`geo.country.code`, 1, 1, "geo is not available: the configuration has no enrichment.geo"},
		{`request.host +`, 1, 15, "syntax error in the expression"},
		{``, 1, 1, "the expression is empty"},
	} {
		for name, compile := range map[string]func(string) (*StringExpr, error){"header": env.CompileString, "field": env.CompileDecisionField} {
			_, err := compile(tc.expr)
			var ce *CompileError
			if !errors.As(err, &ce) || len(ce.Issues) == 0 {
				t.Errorf("%s %q: %v, want a *CompileError", name, tc.expr, err)
				continue
			}
			is := ce.Issues[0]
			if is.Line != tc.line || is.Column != tc.column || !strings.HasPrefix(is.Message, tc.prefix) {
				t.Errorf("%s %q: line %d, column %d: %q; want line %d, column %d: %q…", name, tc.expr, is.Line, is.Column, is.Message, tc.line, tc.column, tc.prefix)
			}
		}
	}
	// A condition still needs a bool.
	if is := compileIssue(t, env, `request.host`); is.Message != "the condition must be a bool, not string" {
		t.Errorf("condition: %q", is.Message)
	}
}

// A decision field cannot read what tessera never logs: the query, the
// secret headers, in any spelling, and the map of the
// headers otherwise than by a literal name. An upstream header can.
func TestDecisionFieldReads(t *testing.T) {
	env := newEnv(t, Options{SecretHeaders: []string{"X-Api-Key", "token"}})
	for _, tc := range []struct {
		expr         string
		line, column int
		message      string
	}{
		{`request.query`, 1, 1, errFieldQuery.Error()},
		{`has(request.query) ? "y" : "n"`, 1, 5, errFieldQuery.Error()},
		{`request.path + "?" + request.query`, 1, 22, errFieldQuery.Error()},
		{`request.headers["authorization"]`, 1, 1, errFieldSecret.Error()},
		{`request.headers["Authorization"]`, 1, 1, errFieldSecret.Error()},
		{`request.headers['cookie']`, 1, 1, errFieldSecret.Error()},
		{`request.headers.authorization`, 1, 1, errFieldSecret.Error()},
		{`request.headers.cookie`, 1, 1, errFieldSecret.Error()},
		{`request.headers.Cookie`, 1, 1, errFieldSecret.Error()},
		{`request.headers["proxy-authorization"]`, 1, 1, errFieldSecret.Error()},
		{`request.headers["Proxy-Authorization"]`, 1, 1, errFieldSecret.Error()},
		{`"proxy-authorization" in request.headers ? "y" : "n"`, 1, 26, errFieldSecret.Error()},
		{`request.headers["x-api-key"]`, 1, 1, errFieldSecret.Error()},
		{`request.headers["X-API-KEY"]`, 1, 1, errFieldSecret.Error()},
		{`"x-api-key" in request.headers ? "y" : "n"`, 1, 16, errFieldSecret.Error()},
		{`request.headers.token`, 1, 1, errFieldSecret.Error()},
		{`has(request.headers.Token) ? "y" : "n"`, 1, 5, errFieldSecret.Error()},
		{`"cookie" in request.headers ? "y" : "n"`, 1, 13, errFieldSecret.Error()},
		{`"COOKIE" in request.headers ? "y" : "n"`, 1, 13, errFieldSecret.Error()},
		{`has(request.headers.authorization) ? "y" : "n"`, 1, 5, errFieldSecret.Error()},
		{"request.host +\n  request.headers.cookie", 2, 3, errFieldSecret.Error()},
		{`request.headers.exists(k, k == "x-tag") ? "y" : "n"`, 1, 1, errFieldHeaders.Error()},
		{`request.headers.all(k, k != "") ? "y" : "n"`, 1, 1, errFieldHeaders.Error()},
		{`request.headers[request.host]`, 1, 1, errFieldHeaders.Error()},
		{`request.headers["x-" + "tag"]`, 1, 1, errFieldHeaders.Error()},
		{`string(size(request.headers))`, 1, 13, errFieldHeaders.Error()},
		{`request.headers.size() > 0 ? "y" : "n"`, 1, 1, errFieldHeaders.Error()},
		{`{"h": request.headers}["h"]["cookie"]`, 1, 7, errFieldHeaders.Error()},
		{`request.host in request.headers ? "y" : "n"`, 1, 17, errFieldHeaders.Error()},
		{`[request][0].query`, 1, 2, errFieldRequest.Error()},
		{`[request].map(r, r.host)[0]`, 1, 2, errFieldRequest.Error()},
	} {
		_, err := env.CompileDecisionField(tc.expr)
		var ce *CompileError
		if !errors.As(err, &ce) || len(ce.Issues) == 0 {
			t.Errorf("%q: %v, want a *CompileError", tc.expr, err)
			continue
		}
		if is := ce.Issues[0]; is.Line != tc.line || is.Column != tc.column || is.Message != tc.message {
			t.Errorf("%q: line %d, column %d: %q; want line %d, column %d: %q", tc.expr, is.Line, is.Column, is.Message, tc.line, tc.column, tc.message)
		}
		// An upstream header has no such restriction.
		if _, err := env.CompileString(tc.expr); err != nil {
			t.Errorf("upstream header %q: %v", tc.expr, err)
		}
	}
	for _, text := range []string{
		`request.headers["user-agent"]`,
		`request.headers.accept`,
		`"x-tag" in request.headers ? request.headers["x-tag"] : ""`,
		`has(request.headers.accept) ? "y" : "n"`,
		`has(request.headers) ? "y" : "n"`,
		`request.host + request.path + request.method + request.scheme + request.id`,
		`string(client.ip)`,
		`["authorization"].exists(h, h == "cookie") ? "y" : "n"`,
		`[1].map(request, string(request))[0]`,
	} {
		if _, err := env.CompileDecisionField(text); err != nil {
			t.Errorf("%s: %v", text, err)
		}
	}
	// The messages are fixed texts.
	_, err := env.CompileDecisionField(`request.headers["s3cr3t"] + request.headers["s3cr3t" + "x"]`)
	if err == nil || strings.Contains(err.Error(), "s3cr3t") {
		t.Errorf("a message repeats a literal: %v", err)
	}
}

// A string expression reads the Input as the conditions do: one lookup of
// the client address per Input, in the databases of its snapshot.
func TestStringExpressionsShareTheInput(t *testing.T) {
	store, env := exampleDatabases(t)
	in := NewInput(sampleRequest("192.0.2.7"), nil, store.Snapshot(), nil)
	if ok, err := mustCompile(t, env, `geo.country.code == "IT"`).Eval(in); !ok || err != nil {
		t.Fatalf("condition: %v, %v", ok, err)
	}
	// The database changes: the Input keeps what it looked up.
	if err := store.Replace(string(enrichment.Geo), loadDatabase(t, enrichment.Geo, mmdbtest.BrokenDatabase("geo"))); err != nil {
		t.Fatal(err)
	}
	if got, err := mustCompileString(t, env, `geo.country.code`).Eval(in); got != "IT" || err != nil {
		t.Errorf("after the condition: %q, %v", got, err)
	}
	field, err := env.CompileDecisionField(`geo.city`)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := field.Eval(in); got != "Rome" || err != nil {
		t.Errorf("decision field: %q, %v", got, err)
	}
}

func TestStringExpressionReads(t *testing.T) {
	_, opts := testSets(t, sets.Fail)
	opts.Databases = enrichment.Kinds
	env := newEnv(t, opts)
	s := mustCompileString(t, env, `sets.hosts.contains(request.host) ? geo.country.code : string(asn.number)`)
	if got := s.Sets(); len(got) != 1 || got[0] != "hosts" {
		t.Errorf("sets: %v", got)
	}
	if got := s.Databases(); len(got) != 2 || got[0] != enrichment.ASN || got[1] != enrichment.Geo {
		t.Errorf("databases: %v", got)
	}
}

func TestConcurrentStringEvaluations(t *testing.T) {
	store, env := exampleDatabases(t)
	s := mustCompileString(t, env, `geo.country.code + "/" + request.host`)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				r := &request.Request{Host: "api.example.com", Headers: map[string]string{}}
				r.ClientIP = sampleRequest("192.0.2.7").ClientIP
				if got, err := s.Eval(NewInput(r, nil, store.Snapshot(), nil)); got != "IT/api.example.com" || err != nil {
					t.Errorf("%q, %v", got, err)
					return
				}
			}
		})
	}
	wg.Wait()
}

// An upstream header whose value can hold the value of authorization or
// cookie is known when it is compiled: it names one of them, in any
// spelling, or reads the map of the headers otherwise than by a literal
// name. The query is not a secret here.
func TestStringExpressionSecrets(t *testing.T) {
	env := newEnv(t, Options{SecretHeaders: []string{"X-Api-Key"}})
	for text, want := range map[string]bool{
		`request.headers["proxy-authorization"]`:                     true,
		`request.headers["x-api-key"]`:                               true,
		`"X-API-KEY" in request.headers ? "y" : "n"`:                 true,
		`request.headers["x-api-key-id"]`:                            false,
		`request.headers["authorization"]`:                           true,
		`request.headers["Authorization"]`:                           true,
		`request.headers.cookie`:                                     true,
		`"cookie" in request.headers ? "y" : "n"`:                    true,
		`has(request.headers.authorization) ? "y" : "n"`:             true,
		`request.headers[request.headers["x-source"]]`:               true,
		`request.headers.exists(h, h == "x-tag") ? "y" : "n"`:        true,
		`string(size(request.headers))`:                              true,
		`[request][0].host`:                                          true,
		`request.headers["user-agent"]`:                              false,
		`request.headers.accept + request.host`:                      false,
		`"x-tag" in request.headers ? request.headers["x-tag"] : ""`: false,
		`has(request.headers) ? "y" : "n"`:                           false,
		`request.query`:                                              false,
		`["authorization"].exists(h, h == "cookie") ? "y" : "n"`:     false,
		`string(client.ip)`:                                          false,
	} {
		if got := mustCompileString(t, env, text).ReadsSecrets(); got != want {
			t.Errorf("%s: ReadsSecrets() = %v, want %v", text, got, want)
		}
	}
	// A decision field cannot read them at all.
	field, err := env.CompileDecisionField(`request.headers["user-agent"]`)
	if err != nil || field.ReadsSecrets() {
		t.Errorf("decision field: %v, %v", err, field)
	}
}
