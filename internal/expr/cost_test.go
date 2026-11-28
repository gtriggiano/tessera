package expr

import (
	"errors"
	"strings"
	"testing"

	"cel.dev/cel-go/common/cost"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/interpreter"

	"github.com/gtriggiano/tessera/internal/enrichment"
	"github.com/gtriggiano/tessera/internal/mmdbtest"
	"github.com/gtriggiano/tessera/internal/request"
	"github.com/gtriggiano/tessera/internal/sets"
)

// The cost of a condition decides whether it passes CostLimit, so it is part
// of the expression language: an update of cel-go that changes the cost of
// one of these expressions, or the revision of the cost model, fails here,
// also when the evaluation fails or a failure is absorbed. Adopt the new
// costs deliberately, as a change of the expression language.
func TestCosts(t *testing.T) {
	store, dbs, _ := testAreas(t, sets.Fail, loadDatabase(t, enrichment.Geo, mmdbtest.CityDatabase(6)))
	down, downDBs, _ := testAreas(t, sets.Fail, loadDatabase(t, enrichment.Geo, mmdbtest.CityDatabase(6)))
	for _, name := range []string{"office", "stores"} {
		if err := down.SetAvailable(name, false); err != nil {
			t.Fatal(err)
		}
	}
	env := newEnv(t, Options{Sets: store.Declarations(), Databases: enrichment.Kinds, UserAgent: true})
	typical := func() *request.Request { return sampleRequest("192.0.2.7") }
	noClient := func() *request.Request { return sampleRequest("") }
	noLocation := func() *request.Request { return sampleRequest("203.0.113.7") }
	chrome := func() *request.Request { return withUserAgent(chromeWindows, true) }
	checkout := func() *request.Request {
		r := withUserAgent(safariIPhone, true)
		r.Path = "/checkout/pay"
		return r
	}
	for _, tc := range []struct {
		text        string
		isString    bool
		req         func() *request.Request
		unavailable bool // office and stores are unavailable
		want        uint64
		cause       Cause
	}{
		{`request.path.startsWith("/admin")`, false, typical, false, 3, ""},
		{`request.method in ["GET", "HEAD"]`, false, typical, false, 2, ""},
		{`request.host == "api.example.com" && "authorization" in request.headers`, false, typical, false, 7, ""},
		{`size(request.headers) == 3 && size(request.path) == 13`, false, typical, false, 8, ""},
		{`client.ip.inCidr("198.51.100.0/24") || client.ip.inCidr("2001:db8:bad::/48")`, false, typical, false, 4, ""},
		{`client.ip.inCidr("192.0.2.0/24") ? request.method == "GET" : false`, false, typical, false, 5, ""},
		{`client.ip.family() == 4`, false, typical, false, 4, ""},
		{`request.path.contains("orders")`, false, typical, false, 3, ""},
		{`request.path.matches("^/v[0-9]+/orders/[0-9]+$")`, false, typical, false, 11, ""},
		{`"user-agent" in request.headers && request.headers["user-agent"].matches("(?i)(sqlmap|nikto)")`, false, typical, false, 11, ""},
		{`"user-agent" in request.headers && request.headers["user-agent"].matches("(?i)(sqlmap|nikto)")`, false, chrome, false, 59, ""},
		{`sets.office.contains(client.ip)`, false, typical, false, 5, ""},
		{`sets.hosts.contains(request.host) && !sets.office.contains(client.ip)`, false, typical, false, 11, ""},
		{`sets.asns.contains(asn.number)`, false, typical, false, 5, ""},
		{`sets.stores.contains(geo.location)`, false, typical, false, 5, ""},
		{`sets.stores.contains(geo.location) ? request.method == "GET" : false`, false, typical, false, 8, ""},
		{`geo.country.code == "IT" && geo.subdivisions.exists(s, s.code == "62")`, false, typical, false, 17, ""},
		{`has(geo.location) && geo.location.lat > 41.0 && geo.location.lat < 43.0 && geo.location.lon > 11.5 && geo.location.lon < 13.5`, false, typical, false, 18, ""},
		{`request.path.startsWith("/checkout") && ua.browser.name == "Safari" && ua.browser.major < 15`, false, checkout, false, 11, ""},
		{`asn.found ? string(asn.number) : ""`, true, typical, false, 5, ""},
		{`geo.country.code`, true, typical, false, 3, ""},
		// The largest request that Envoy lets through.
		{`request.headers.exists(k, k.startsWith("x-debug-"))`, false, largeRequest, false, 603, ""},
		{`request.headers["x-big"].contains("needle")`, false, largeRequest, false, 3567, ""},
		// Without a client address.
		{`sets.office.contains(client.ip)`, false, noClient, false, 4, CauseClientIPMissing},
		{`sets.stores.contains(geo.location)`, false, noClient, false, 3, CauseClientIPMissing},
		{`sets.office.contains(client.ip) || request.method == "GET"`, false, noClient, false, 7, ""},
		{`sets.stores.contains(geo.location) || request.method == "GET"`, false, noClient, false, 6, ""},
		{`request.headers.exists(k, client.ip.inCidr("192.0.2.0/24") ? k == "accept" : false)`, false, noClient, false, 21, CauseClientIPMissing},
		// 24 includes one unit for each == on an error operand, a charge that
		// cel-go marks for removal: a later cel-go that drops it fails here.
		{`request.headers.exists(k, (client.ip.inCidr("192.0.2.0/24") ? k : "") == "x")`, false, noClient, false, 24, CauseClientIPMissing},
		// A record without a location: contains on an area set is false.
		{`sets.stores.contains(geo.location)`, false, noLocation, false, 5, ""},
		{`!sets.stores.contains(geo.location)`, false, noLocation, false, 6, ""},
		// Unavailable sets, whose on_unavailable is fail.
		{`sets.office.contains(client.ip)`, false, typical, true, 5, CauseSetUnavailable},
		{`sets.stores.contains(geo.location)`, false, typical, true, 5, CauseSetUnavailable},
		{`sets.office.contains(client.ip) || request.method == "GET"`, false, typical, true, 8, ""},
	} {
		var p program
		if tc.isString {
			p = mustCompileString(t, env, tc.text).program
		} else {
			p = mustCompile(t, env, tc.text).program
		}
		in := NewInput(tc.req(), store.Snapshot(), dbs.Snapshot(), nil)
		if tc.unavailable {
			in = NewInput(tc.req(), down.Snapshot(), downDBs.Snapshot(), nil)
		}
		_, details, err := p.prg.Eval(in)
		var got Cause
		if err != nil {
			got = p.classify(err, in).Cause
		}
		if got != tc.cause || details.ActualCost() == nil {
			t.Errorf("%s: %v, cause %q, no cost; want cause %q", tc.text, err, got, tc.cause)
			continue
		}
		if got := *details.ActualCost(); got != tc.want {
			t.Errorf("%s costs %d, want %d", tc.text, got, tc.want)
		}
	}
}

// tessera counts costs with revision 1 of CEL's cost model, not with the
// latest revision of the cel-go in use.
func TestCostModelRevision(t *testing.T) {
	c := mustCompile(t, newEnv(t, Options{}), `request.method == "GET"`)
	frame, err := interpreter.NewExecutionFrame(NewInput(sampleRequest(""), nil, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer frame.Close()
	if _, _, err := c.prg.Eval(frame); err != nil {
		t.Fatal(err)
	}
	if got := frame.CostTracker().ModelVersion(); got != 1 {
		t.Errorf("revision %d of the cost model, want 1", got)
	}
}

// A condition that costs CostLimit evaluates; one that costs a unit more is
// indeterminate, also when that unit is the one that tessera charges for
// contains on an area set.
func TestCostLimitBoundary(t *testing.T) {
	store, dbs, env := testAreas(t, sets.Fail, loadDatabase(t, enrichment.Geo, mmdbtest.CityDatabase(6)))
	// Reading the header costs 3, and contains costs ⌈0.06·n⌉ on n
	// characters.
	const header = `request.headers["x-big"].contains("needle")`
	for _, tc := range []struct {
		text  string
		n     int
		limit bool
	}{
		{header, 166616, false}, // 3 + 9997
		{header, 166617, true},  // 3 + 9998
		// Then reading sets.stores and geo.location costs 4, and contains
		// on the area 1.
		{header + ` || sets.stores.contains(geo.location)`, 166533, false}, // 3 + 9992 + 4 + 1
		{header + ` || sets.stores.contains(geo.location)`, 166550, true},  // 3 + 9993 + 4 + 1
	} {
		c := mustCompile(t, env, tc.text)
		r := sampleRequest("192.0.2.7")
		r.Headers["x-big"] = strings.Repeat("a", tc.n)
		_, err := c.Eval(NewInput(r, store.Snapshot(), dbs.Snapshot(), nil))
		if tc.limit {
			if !errors.Is(err, ErrCostLimit) || cause(err) != CauseExpression {
				t.Errorf("%s, %d characters: %v; want the cost limit", tc.text, tc.n, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s, %d characters: %v", tc.text, tc.n, err)
		}
		if _, details, _ := c.prg.Eval(NewInput(r, store.Snapshot(), dbs.Snapshot(), nil)); details.ActualCost() == nil {
			t.Errorf("%s, %d characters: no cost", tc.text, tc.n)
		} else if got := *details.ActualCost(); got != CostLimit {
			t.Errorf("%s, %d characters: cost %d, want %d", tc.text, tc.n, got, CostLimit)
		}
	}
}

// The panics that stop an evaluation at the cost limit go through tessera's
// guards: a guard that recovered them would turn the limit into a panic.
func TestCostStopsAreNotRecovered(t *testing.T) {
	reported := 0
	lib := &library{onPanic: func(*Panic) { reported++ }}
	for _, stop := range []any{
		interpreter.EvalCancelledError{Cause: interpreter.CostLimitExceeded, Message: "stop"},
		cost.LimitExceededError{Message: "stop"},
	} {
		func() {
			defer func() {
				if r := recover(); r != stop {
					t.Errorf("%T: recovered %v, want it to go on", stop, r)
				}
			}()
			var out ref.Val
			func() {
				defer lib.recoverPanic(&out, nil)
				panic(stop)
			}()
		}()
	}
	if reported != 0 {
		t.Errorf("%d panics reported, want none", reported)
	}
}
