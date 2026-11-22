package expr

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/gtriggiano/tessera/internal/request"
	"github.com/gtriggiano/tessera/internal/sets"
)

// testSets declares office (cidr), asns (asn) and hosts (string), each with
// on_unavailable answer, and loads them.
func testSets(t testing.TB, answer sets.OnUnavailable) (*sets.Store, Options) {
	t.Helper()
	decls := []sets.Declaration{
		{Name: "office", Type: sets.CIDR, OnUnavailable: answer},
		{Name: "asns", Type: sets.ASN, OnUnavailable: answer},
		{Name: "hosts", Type: sets.String, OnUnavailable: answer},
	}
	store, err := sets.NewStore(decls)
	if err != nil {
		t.Fatal(err)
	}
	for name, values := range map[string][]string{
		"office": {"192.0.2.0/24", "2001:db8::/32"},
		"asns":   {"64496", "AS64511"},
		"hosts":  {"api.example.com", "Admin.example.com"},
	} {
		typ := map[string]sets.Type{"office": sets.CIDR, "asns": sets.ASN, "hosts": sets.String}[name]
		s, err := sets.FromValues(typ, values, 10)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Replace(name, s); err != nil {
			t.Fatal(err)
		}
	}
	return store, Options{Sets: decls}
}

func evalWith(t *testing.T, env *Env, text string, r *request.Request, store *sets.Store) (bool, error) {
	t.Helper()
	return mustCompile(t, env, text).Eval(NewInput(r, store.Snapshot()))
}

func TestSetContains(t *testing.T) {
	store, opts := testSets(t, sets.Fail)
	env := newEnv(t, opts)
	for text, want := range map[string]bool{
		`sets.office.contains(client.ip)`:                                        true,
		`!sets.office.contains(client.ip)`:                                       false,
		`sets.asns.contains(64496)`:                                              true,
		`sets.asns.contains(64511)`:                                              true,
		`sets.asns.contains(64500)`:                                              false,
		`sets.asns.contains(0)`:                                                  false,
		`sets.asns.contains(-64496)`:                                             false,
		`sets.asns.contains(4294967296 + 64496)`:                                 false,
		`sets.hosts.contains(request.host)`:                                      true,
		`sets.hosts.contains("admin.example.com")`:                               false, // exact
		`sets.hosts.contains("Admin.example.com")`:                               true,
		`sets.hosts.contains(request.host) && "api.example.com".contains("api")`: true,
		`request.host.contains("example") && sets.hosts.contains("x")`:           false,
		`sets.office.contains(client.ip) || sets.hosts.contains("x")`:            true,
		`request.headers.exists(k, sets.hosts.contains(k))`:                      false,
	} {
		got, err := evalWith(t, env, text, sampleRequest("192.0.2.10"), store)
		if err != nil || got != want {
			t.Errorf("%s = %v, %v; want %v", text, got, err, want)
		}
	}
	// client.ip is unmapped: an IPv4-mapped address is in an IPv4 network.
	if got, err := evalWith(t, env, `sets.office.contains(client.ip)`, sampleRequest("::ffff:192.0.2.10"), store); !got || err != nil {
		t.Errorf("a mapped client address: %v, %v", got, err)
	}
	if got, err := evalWith(t, env, `sets.office.contains(client.ip)`, sampleRequest("2001:db8::7"), store); !got || err != nil {
		t.Errorf("an IPv6 client address: %v, %v", got, err)
	}
	if got, err := evalWith(t, env, `sets.office.contains(client.ip)`, sampleRequest("198.51.100.1"), store); got || err != nil {
		t.Errorf("an address outside the set: %v, %v", got, err)
	}
	// Without a client address, the condition is indeterminate for that
	// reason, not for the set.
	if _, err := evalWith(t, env, `sets.office.contains(client.ip)`, sampleRequest(""), store); cause(err) != CauseClientIPMissing {
		t.Errorf("without a client address: %v", err)
	}
}

func TestSetUnavailable(t *testing.T) {
	r := sampleRequest("192.0.2.10")
	for _, tc := range []struct {
		answer sets.OnUnavailable
		want   bool
		cause  Cause
	}{
		{sets.Fail, false, CauseSetUnavailable},
		{sets.Match, true, ""},
		{sets.NoMatch, false, ""},
	} {
		store, opts := testSets(t, tc.answer)
		env := newEnv(t, opts)
		for _, name := range []string{"office", "asns", "hosts"} {
			if err := store.SetAvailable(name, false); err != nil {
				t.Fatal(err)
			}
		}
		for _, text := range []string{
			`sets.office.contains(client.ip)`,
			`sets.asns.contains(64496)`,
			`sets.asns.contains(0)`, // whatever the argument
			`sets.hosts.contains("nowhere.example.com")`,
		} {
			got, err := evalWith(t, env, text, r, store)
			if got != tc.want || cause(err) != tc.cause {
				t.Errorf("on_unavailable %s: %s = %v, %v; want %v, cause %q", tc.answer, text, got, err, tc.want, tc.cause)
			}
		}
		// A set never loaded is unavailable too: an Input without a
		// snapshot has none loaded.
		got, err := mustCompile(t, env, `sets.hosts.contains("api.example.com")`).Eval(NewInput(r, nil))
		if got != tc.want || cause(err) != tc.cause {
			t.Errorf("on_unavailable %s, never loaded: %v, %v", tc.answer, got, err)
		}
	}

	// && and || absorb the error when the other operand decides.
	store, opts := testSets(t, sets.Fail)
	env := newEnv(t, opts)
	if err := store.SetAvailable("hosts", false); err != nil {
		t.Fatal(err)
	}
	for text, want := range map[string]struct {
		value bool
		cause Cause
	}{
		`sets.hosts.contains(request.host) || true`:                  {true, ""},
		`true || sets.hosts.contains(request.host)`:                  {true, ""},
		`sets.hosts.contains(request.host) && false`:                 {false, ""},
		`sets.hosts.contains(request.host) && sets.asns.contains(1)`: {false, ""},
		`sets.hosts.contains(request.host) || false`:                 {false, CauseSetUnavailable},
		`!sets.hosts.contains(request.host)`:                         {false, CauseSetUnavailable},
		`sets.office.contains(client.ip)`:                            {true, ""}, // another set stays available
	} {
		got, err := evalWith(t, env, text, r, store)
		if got != want.value || cause(err) != want.cause {
			t.Errorf("%s = %v, %v; want %v, cause %q", text, got, err, want.value, want.cause)
		}
	}
	_, err := evalWith(t, env, `sets.hosts.contains(request.host)`, r, store)
	if !errors.Is(err, ErrSetUnavailable) || err.Error() != "indeterminate condition: set_unavailable" {
		t.Errorf("error %v", err)
	}
}

func TestSetCompileErrors(t *testing.T) {
	_, opts := testSets(t, sets.Fail)
	env := newEnv(t, opts)
	for _, tc := range []struct {
		expr         string
		line, column int
		prefix       string
	}{
		{`sets.office.contains("192.0.2.10")`, 1, 21, "found no matching overload for 'contains' applied to 'tessera.CIDRSet.(string)'"},
		{`sets.office.contains(64496)`, 1, 21, "found no matching overload for 'contains' applied to 'tessera.CIDRSet.(int)'"},
		{`sets.asns.contains("AS64496")`, 1, 19, "found no matching overload for 'contains' applied to 'tessera.ASNSet.(string)'"},
		{`sets.asns.contains(client.ip)`, 1, 19, "found no matching overload for 'contains' applied to 'tessera.ASNSet.(tessera.IP)'"},
		{`sets.hosts.contains(client.ip)`, 1, 20, "found no matching overload for 'contains' applied to 'tessera.StringSet.(tessera.IP)'"},
		{`sets.hosts.contains(request.host, "x")`, 1, 20, "found no matching overload for 'contains'"},
		{`sets.partners.contains(request.host)`, 1, 5, "undefined field 'partners'"},
		{`request.host == "x" && sets.nope.contains(client.ip)`, 1, 28, "undefined field 'nope'"},
		{`sets.office.startsWith("x")`, 1, 23, "found no matching overload for 'startsWith'"},
		{`sets == sets`, 1, 1, "a set can be used only as the target of contains"},
		{`sets.office == sets.office`, 1, 1, "a set can be used only as the target of contains"},
		{`has(sets.office)`, 1, 5, "a set can be used only as the target of contains"},
		{`[sets.hosts].size() == 1`, 1, 2, "a set can be used only as the target of contains"},
		{`request.host == "x" || type(sets.asns) == type(sets.asns)`, 1, 29, "a set can be used only as the target of contains"},
		{"request.host == \"x\" &&\n  sets.hosts.contains(client.ip)", 2, 22, "found no matching overload for 'contains'"},
	} {
		is := compileIssue(t, env, tc.expr)
		if is.Line != tc.line || is.Column != tc.column || !strings.HasPrefix(is.Message, tc.prefix) {
			t.Errorf("%q: line %d, column %d: %q; want line %d, column %d: %q…", tc.expr, is.Line, is.Column, is.Message, tc.line, tc.column, tc.prefix)
		}
	}
	// Without declared sets, sets has no field.
	bare := newEnv(t, Options{})
	if is := compileIssue(t, bare, `sets.office.contains(client.ip)`); !strings.HasPrefix(is.Message, "undefined field 'office'") {
		t.Errorf("no set declared: %q", is.Message)
	}
	// A message never repeats a literal of the expression.
	for _, text := range []string{
		`sets.office.contains("s3cr3t")`,
		`sets.asns.contains("s3cr3t") || sets.hosts.contains(1)`,
		`sets.hosts.contains(request.host) == "s3cr3t"`,
	} {
		if _, err := env.Compile(text); err == nil || strings.Contains(err.Error(), "s3cr3t") {
			t.Errorf("%s: %v", text, err)
		}
	}
	// An iteration variable named sets is not the variable sets.
	if _, err := env.Compile(`[1, 2].exists(sets, sets == 2)`); err != nil {
		t.Errorf("an iteration variable named sets: %v", err)
	}
}

func TestSetsRead(t *testing.T) {
	_, opts := testSets(t, sets.Fail)
	env := newEnv(t, opts)
	for text, want := range map[string]string{
		`sets.office.contains(client.ip)`: `office`,
		`sets.hosts.contains(request.host) || sets.asns.contains(64496) && sets.hosts.contains("x")`: `asns hosts`,
		`request.headers.exists(k, sets.hosts.contains(k))`:                                          `hosts`,
		`[1].exists(sets, sets == 1) && request.host == "x"`:                                         ``,
		`client.ip.inCidr("192.0.2.0/24")`:                                                           ``,
	} {
		if got := strings.Join(mustCompile(t, env, text).Sets(), " "); got != want {
			t.Errorf("%s: sets %q, want %q", text, got, want)
		}
		if got := mustCompile(t, env, text).Attributes(); strings.Contains(strings.Join(got, " "), "sets") {
			t.Errorf("%s: attributes %q name a set", text, got)
		}
	}
}

// One decision reads one snapshot of the sets: a copy replaced while it is
// in progress changes nothing for it.
func TestSetSnapshotPerDecision(t *testing.T) {
	store, opts := testSets(t, sets.Fail)
	env := newEnv(t, opts)
	c := mustCompile(t, env, `sets.hosts.contains(request.host)`)
	r := sampleRequest("192.0.2.10")
	in := NewInput(r, store.Snapshot())
	if ok, err := c.Eval(in); !ok || err != nil {
		t.Fatalf("before the replacement: %v, %v", ok, err)
	}
	other, _ := sets.FromValues(sets.String, []string{"www.example.com"}, 10)
	if err := store.Replace("hosts", other); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAvailable("hosts", false); err != nil {
		t.Fatal(err)
	}
	if ok, err := c.Eval(in); !ok || err != nil {
		t.Errorf("same decision, after the replacement: %v, %v; want the copy it started with", ok, err)
	}
	if _, err := c.Eval(NewInput(r, store.Snapshot())); cause(err) != CauseSetUnavailable {
		t.Errorf("next decision: %v; want the set unavailable", err)
	}
}

// Decisions run while copies are replaced and marked unavailable: each sees
// one consistent snapshot. Run with -race.
func TestSetsConcurrentReplacement(t *testing.T) {
	store, opts := testSets(t, sets.Fail)
	env := newEnv(t, opts)
	// Both conditions read hosts: in one snapshot they agree.
	first := mustCompile(t, env, `sets.hosts.contains("api.example.com")`)
	second := mustCompile(t, env, `sets.hosts.contains("api.example.com") || false`)
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
			copies := []*sets.Set{with, without}
			_ = store.Replace("hosts", copies[i%2])
			if i%3 == 0 {
				_ = store.SetAvailable("hosts", false)
			}
		}
	})
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			for range 2000 {
				in := NewInput(sampleRequest("192.0.2.10"), store.Snapshot())
				a, errA := first.Eval(in)
				b, errB := second.Eval(in)
				if a != b || cause(errA) != cause(errB) {
					t.Errorf("one decision saw two states: %v %v, %v %v", a, errA, b, errB)
					return
				}
			}
		})
	}
	workers.Wait()
	close(stop)
	writer.Wait()
}

// contains costs one unit whatever the size of the set and of its argument.
func TestSetContainsCost(t *testing.T) {
	store, opts := testSets(t, sets.Fail)
	env := newEnv(t, opts)
	c := mustCompile(t, env, `sets.hosts.contains(request.host)`)
	cost := func(host string) uint64 {
		r := sampleRequest("192.0.2.10")
		r.Host = host
		_, details, err := c.prg.Eval(NewInput(r, store.Snapshot()))
		if err != nil || details == nil || details.ActualCost() == nil {
			t.Fatalf("no cost: %v", err)
		}
		return *details.ActualCost()
	}
	short, long := cost("a.example.com"), cost(strings.Repeat("a", 60<<10)+".example.com")
	if short != long || short > 10 {
		t.Errorf("cost %d for a short host, %d for a long one; want the same small cost", short, long)
	}
}
