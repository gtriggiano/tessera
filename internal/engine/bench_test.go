package engine

import (
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/gtriggiano/tessera/internal/enrichment"
	"github.com/gtriggiano/tessera/internal/mmdbtest"
	"github.com/gtriggiano/tessera/internal/request"
	"github.com/gtriggiano/tessera/internal/sets"
)

// benchRules are ten typical rules: host equality, path prefixes, a method in
// a list, inCidr on a few networks, header presence, a regular expression,
// endsWith and contains, combined with && and ||. A request of the scenario
// "last" is decided by the tenth, after the nine others are evaluated; one of
// the scenario "early" by the second.
var benchRules = []Rule{
	{Name: "blocked-host", When: `request.host == "blocked.example.com"`, Action: Deny},
	{Name: "admin-outside", When: `request.path.startsWith("/admin/") && !client.ip.inCidr("192.0.2.0/24")`, Action: Deny},
	{Name: "blocked-networks", When: `client.ip.inCidr("198.51.100.0/24") || client.ip.inCidr("2001:db8:bad::/48") || client.ip.inCidr("203.0.113.128/25")`, Action: Deny},
	{Name: "methods", When: `!(request.method in ["GET", "HEAD", "POST", "PUT", "DELETE", "OPTIONS"])`, Action: Deny},
	{Name: "scanners", When: `"user-agent" in request.headers && request.headers["user-agent"].matches("(?i)(sqlmap|nikto|masscan)")`, Action: Deny},
	{Name: "status", When: `request.host == "status.example.com" && request.path == "/healthz"`, Action: Allow},
	{Name: "internal-api", When: `"x-api-key" in request.headers && request.host.endsWith(".internal.example.com")`, Action: Allow},
	{Name: "traversal", When: `request.path.contains("..") || request.query.contains("<script")`, Action: Deny},
	{Name: "static", When: `request.method == "GET" && request.path.matches("^/static/[a-z0-9/._-]+$")`, Action: Allow},
	{Name: "api", When: `request.host == "api.example.com" && request.path.startsWith("/v1/") && request.method in ["GET", "POST"] && client.ip.inCidr("192.0.2.0/24")`, Action: Allow},
}

type benchScenario struct {
	name string
	// requests are as many as a power of two.
	requests []*request.Request
	rule     string
	action   Action
}

// benchHeaders returns the headers of the request i of a scenario, from the
// client address client.
func benchHeaders(i int, client string) map[string]string {
	return map[string]string{
		"user-agent":      "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_5) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Safari/605.1.15",
		"accept":          "application/json",
		"accept-encoding": "gzip, br",
		"content-type":    "application/json",
		"x-request-id":    fmt.Sprintf("req-%04d", i),
		"x-forwarded-for": client,
	}
}

// benchScenarios returns 64 different requests for each scenario.
func benchScenarios() []benchScenario {
	last := benchScenario{name: "last", rule: "api", action: Allow}
	early := benchScenario{name: "early", rule: "admin-outside", action: Deny}
	for i := range 64 {
		headers := benchHeaders(i, fmt.Sprintf("192.0.2.%d", i+1))
		last.requests = append(last.requests, &request.Request{
			ID: fmt.Sprintf("req-%04d", i), Scheme: "https", Method: []string{"GET", "POST"}[i%2],
			Host: "api.example.com", Path: fmt.Sprintf("/v1/orders/%d", 1000+i), Query: fmt.Sprintf("page=%d", i%7),
			Headers: headers, ClientIP: netip.AddrFrom4([4]byte{192, 0, 2, byte(i + 1)}),
		})
		early.requests = append(early.requests, &request.Request{
			ID: fmt.Sprintf("req-%04d", i), Scheme: "https", Method: "GET",
			Host: "api.example.com", Path: fmt.Sprintf("/admin/users/%d", i),
			Headers: headers, ClientIP: netip.AddrFrom4([4]byte{203, 0, 113, byte(i + 1)}),
		})
	}
	return []benchScenario{last, early}
}

// benchDatabases are the large databases of the benchmarks, as large as
// GeoLite2 City and GeoLite2 ASN, generated once, in 240.0.0.0/4 and
// 2001:db8::/32 (see mmdbtest.LargeCityDatabase), with the networks of the
// example databases: 192.0.2.0/24 is in Rome, Italy, in AS 64496.
var benchDatabases = sync.OnceValues(func() (*enrichment.Store, []netip.Addr) {
	store, err := enrichment.NewStore(enrichment.Kinds)
	if err != nil {
		panic(err)
	}
	var addrs []netip.Addr
	for _, c := range []struct {
		kind  enrichment.Kind
		large func() ([]byte, []netip.Prefix, error)
	}{
		{enrichment.Geo, mmdbtest.LargeCityDatabase},
		{enrichment.ASN, mmdbtest.LargeASNDatabase},
	} {
		content, sample, err := c.large()
		if err != nil {
			panic(err)
		}
		db, err := enrichment.Parse(c.kind, content)
		if err != nil {
			panic(err)
		}
		if err := store.Replace(string(c.kind), db); err != nil {
			panic(err)
		}
		if c.kind == enrichment.Geo {
			var sample4 []netip.Prefix
			for _, p := range sample {
				if p.Addr().Is4() {
					sample4 = append(sample4, p)
				}
			}
			addrs = mmdbtest.Addresses(sample4, 1<<16, 3)
		}
	}
	return store, addrs
})

// defaultScenario returns the requests of the scenario "default": those of
// the scenario "last", from 65,536 different IPv4 addresses of the networks
// of the large geo database, outside 192.0.2.0/24, so that no rule decides
// and the default action denies, after the ten rules.
func defaultScenario() benchScenario {
	_, addrs := benchDatabases()
	s := benchScenario{name: "default", rule: DefaultRule, action: Deny}
	for i, a := range addrs {
		s.requests = append(s.requests, &request.Request{
			ID: fmt.Sprintf("req-%04d", i), Scheme: "https", Method: []string{"GET", "POST"}[i%2],
			Host: "api.example.com", Path: fmt.Sprintf("/v1/orders/%d", 1000+i%64), Query: fmt.Sprintf("page=%d", i%7),
			Headers: benchHeaders(i, a.String()), ClientIP: a,
		})
	}
	return s
}

// setRule is the rule added before the ten in the cases with a set: a deny
// rule on a cidr set, which the requests of the scenario "last" do not match.
var setRule = Rule{Name: "blocked-set", When: `sets.blocked.contains(client.ip)`, Action: Deny}

// databaseRules are the rules added before the ten in the case
// default-databases: a deny rule on the country, which reads geo, and one on
// the autonomous system, which reads asn. No request of the scenario
// default matches them.
var databaseRules = []Rule{
	{Name: "blocked-countries", When: `geo.country.code in ["KP", "IR", "SY"] && !geo.country.in_eu`, Action: Deny},
	{Name: "hosting", When: `asn.number in [64510, 64511] || asn.organization.endsWith(" Hosting")`, Action: Deny},
}

// benchUpstream is what the policy of the case last-upstream adds to the ten
// rules: three upstream headers and two decision fields, which read geo and
// asn in the large databases.
var benchUpstream = Policy{
	UpstreamHeaders: []NamedExpression{
		{"x-client-asn", `asn.found ? string(asn.number) : ""`},
		{"x-client-city", "geo.city"},
		{"x-client-country", "geo.country.code"},
	},
	DecisionFields: []NamedExpression{
		{"agent", `request.headers["user-agent"]`},
		{"country", "geo.country.code"},
	},
}

// benchCase is a scenario decided by the ten rules: after setRule on a set
// of networks networks when networks is not 0; after databaseRules with the
// large databases when databases; with benchUpstream when upstream, whose
// decision fields each decision computes too, as for a decision event.
type benchCase struct {
	name                string
	scenario            benchScenario
	networks            int
	databases, upstream bool
}

func benchCases() []benchCase {
	scenarios := benchScenarios()
	last, early := scenarios[0], scenarios[1]
	byDefault := defaultScenario()
	return []benchCase{
		{name: "last", scenario: last},
		{name: "early", scenario: early},
		{name: "last-set-100000", scenario: last, networks: 100_000},
		{name: "last-set-1000000", scenario: last, networks: 1_000_000},
		{name: "default", scenario: byDefault},
		{name: "default-databases", scenario: byDefault, databases: true},
		{name: "last-upstream", scenario: last, upstream: true},
	}
}

func benchEngine(b *testing.B, c benchCase) *Engine {
	b.Helper()
	p, opts := Policy{DefaultAction: Deny, DenyResponse: Response{Status: 403}, Rules: benchRules}, Options{}
	if c.networks > 0 {
		p.Rules, opts.Sets = append([]Rule{setRule}, benchRules...), benchSet(b, c.networks)
	}
	if c.databases || c.upstream {
		opts.Databases, _ = benchDatabases()
	}
	if c.databases {
		p.Rules = append(slices.Clone(databaseRules), benchRules...)
	}
	if c.upstream {
		p.UpstreamHeaders, p.DecisionFields = benchUpstream.UpstreamHeaders, benchUpstream.DecisionFields
	}
	e, err := New(p, opts)
	if err != nil {
		b.Fatal(err)
	}
	return e
}

// decide decides r with e, and computes the decision fields when the case
// has them, as the decision event does.
func (c benchCase) decide(e *Engine, r *request.Request) Decision {
	d := e.Decide(r)
	if c.upstream {
		e.DecisionFields(d)
	}
	return d
}

// benchSet returns a store with the cidr set blocked of n distinct networks,
// always the same ones: the addresses of 192.0.2.0/24 from 192.0.2.65, which
// the requests do not use, so that the lookup of their client addresses goes
// down to the last level of the table, and a blocklist of both families
// generated in ranges that are not real address space, as in the benchmarks
// of internal/sets: 60% IPv4 addresses, 24.9% IPv4 /24 networks, 0.1% IPv4
// networks from /16 to /23, all in 240.0.0.0/4 (reserved for future use),
// 12% IPv6 /64 networks and 3% IPv6 /48 networks, in 2001:db8::/32.
func benchSet(b *testing.B, n int) *sets.Store {
	b.Helper()
	values := make([]string, 0, n)
	seen := make(map[string]struct{}, n)
	add := func(p netip.Prefix) {
		v := p.String()
		if p.IsSingleIP() {
			v = p.Addr().String()
		}
		if _, ok := seen[v]; !ok {
			seen[v] = struct{}{}
			values = append(values, v)
		}
	}
	for i := 65; i < 256; i++ {
		add(netip.PrefixFrom(netip.AddrFrom4([4]byte{192, 0, 2, byte(i)}), 32))
	}
	r := rand.New(rand.NewPCG(2026, 11))
	v4 := func(bits int) netip.Prefix {
		var a [4]byte
		binary.BigEndian.PutUint32(a[:], 0xf0000000|r.Uint32N(1<<28))
		return netip.PrefixFrom(netip.AddrFrom4(a), bits).Masked()
	}
	v6 := func(bits int) netip.Prefix {
		a := [16]byte{0x20, 0x01, 0x0d, 0xb8}
		binary.BigEndian.PutUint32(a[4:], r.Uint32())
		binary.BigEndian.PutUint64(a[8:], r.Uint64())
		return netip.PrefixFrom(netip.AddrFrom16(a), bits).Masked()
	}
	for len(values) < n {
		switch x := r.IntN(1000); {
		case x < 600:
			add(v4(32))
		case x < 849:
			add(v4(24))
		case x < 850:
			add(v4(16 + r.IntN(8)))
		case x < 970:
			add(v6(64))
		default:
			add(v6(48))
		}
	}
	set, err := sets.FromValues(sets.CIDR, values, n)
	if err != nil {
		b.Fatal(err)
	}
	store, err := sets.NewStore([]sets.Declaration{{Name: "blocked", Type: sets.CIDR, OnUnavailable: sets.Fail}})
	if err != nil {
		b.Fatal(err)
	}
	if err := store.Replace("blocked", set); err != nil {
		b.Fatal(err)
	}
	return store
}

func checkScenario(b *testing.B, e *Engine, c benchCase) {
	b.Helper()
	s := c.scenario
	for i, r := range s.requests {
		if d := c.decide(e, r); d.Action != s.action || d.Rule != s.rule || len(d.Errors) != 0 {
			b.Fatalf("%s request %d: %+v, want %s by %s", s.name, i, d, s.action, s.rule)
		}
	}
	if c.upstream {
		d := e.Decide(s.requests[0])
		fields := e.DecisionFields(d)
		if len(d.Upstream) != 3 || d.Upstream[0].Value != "64496" || d.Upstream[2].Value != "IT" || len(fields) != 2 || fields[1].Value != "IT" {
			b.Fatalf("upstream headers %+v, decision fields %+v", d.Upstream, fields)
		}
	}
}

// BenchmarkDecision times one decision over the ten rules, from the
// normalized request to the decision, the cost limit included.
func BenchmarkDecision(b *testing.B) {
	for _, c := range benchCases() {
		e, s := benchEngine(b, c), c.scenario
		checkScenario(b, e, c)
		mask := len(s.requests) - 1
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			i := 0
			for b.Loop() {
				c.decide(e, s.requests[i&mask])
				i++
			}
		})
	}
}

// BenchmarkDecisionLatency times every decision apart, and reports the 50th,
// 99th and 99.9th percentiles of their times, in nanoseconds, with the time
// of the clock itself in each.
func BenchmarkDecisionLatency(b *testing.B) {
	for _, c := range benchCases() {
		e, s := benchEngine(b, c), c.scenario
		checkScenario(b, e, c)
		mask := len(s.requests) - 1
		b.Run(c.name, func(b *testing.B) {
			// The times of the last million decisions at most. b.Loop runs
			// this function once, so b.N is not known here.
			times := make([]time.Duration, 0, 1<<20)
			i := 0
			for b.Loop() {
				start := time.Now()
				c.decide(e, s.requests[i&mask])
				elapsed := time.Since(start)
				if len(times) < cap(times) {
					times = append(times, elapsed)
				} else {
					times[i%cap(times)] = elapsed
				}
				i++
			}
			if len(times) == 0 {
				return
			}
			slices.Sort(times)
			at := func(q float64) float64 { return float64(times[int(q*float64(len(times)-1))].Nanoseconds()) }
			b.ReportMetric(at(0.50), "p50-ns")
			b.ReportMetric(at(0.99), "p99-ns")
			b.ReportMetric(at(0.999), "p99.9-ns")
		})
	}
}
