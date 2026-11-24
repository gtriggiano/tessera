package engine

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"maps"
	"math"
	"math/rand/v2"
	"net/netip"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gtriggiano/tessera/internal/enrichment"
	"github.com/gtriggiano/tessera/internal/mmdbtest"
	"github.com/gtriggiano/tessera/internal/request"
	"github.com/gtriggiano/tessera/internal/sets"
	"github.com/gtriggiano/tessera/internal/useragent"
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

// userAgentRule is the rule added before the ten in the cases with the
// analysis of the user-agent header: a deny rule on the bots and on
// Internet Explorer, which reads ua, and which the browsers of the requests
// do not match.
var userAgentRule = Rule{Name: "bots", When: `ua.is_bot || ua.browser.name == "Internet Explorer"`, Action: Deny}

// benchAgents returns n distinct user-agent headers of browsers, the same
// ones at every run: Chrome, Edge and Firefox on computers and phones, and
// Safari on the iPhone, with versions drawn at random.
func benchAgents(n int) []string {
	formats := []string{
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%d.0.%d.%d Safari/537.36",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%d.0.%d.%d Safari/537.36",
		"Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%d.0.%d.%d Mobile Safari/537.36",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36 Edg/%d.0.%d.%d",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:%[1]d.0) Gecko/20100101 Firefox/%[1]d.%[2]d.%[3]d",
		"Mozilla/5.0 (iPhone; CPU iPhone OS 17_6 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/%d.%d.%d Mobile/15E148 Safari/604.1",
	}
	r := rand.New(rand.NewPCG(2026, 41))
	seen := make(map[string]struct{}, n)
	agents := make([]string, 0, n)
	for len(agents) < n {
		f := formats[r.IntN(len(formats))]
		major := 100 + r.IntN(41)
		if strings.Contains(f, "Version/") {
			major = 13 + r.IntN(6)
		}
		a := fmt.Sprintf(f, major, r.IntN(10000), r.IntN(1000))
		if _, ok := seen[a]; !ok {
			seen[a] = struct{}{}
			agents = append(agents, a)
		}
	}
	return agents
}

// withAgents returns the scenario s with n requests, a power of two: those
// of s in turn, each with the user-agent header agents[i%len(agents)].
func withAgents(s benchScenario, n int, agents []string) benchScenario {
	out := s
	out.requests = make([]*request.Request, n)
	for i := range out.requests {
		r := *s.requests[i%len(s.requests)]
		r.Headers = maps.Clone(r.Headers)
		r.Headers["user-agent"] = agents[i%len(agents)]
		out.requests[i] = &r
	}
	return out
}

// areaRule is the rule added before the ten in the case default-area: a deny
// rule on an area set and on the method DELETE, which the requests do not
// use, so that every decision reads geo and looks its location up in the
// areas, and no decision is taken by the rule.
var areaRule = Rule{Name: "closed-areas", When: `sets.closed.contains(geo.location) && request.method == "DELETE"`, Action: Deny}

// benchAreas returns a store with the area set closed: n geofences, the same
// ones at every run, around places of the world, from 55 degrees south to 70
// degrees north, as a store locator would export them: circles of 0.5 to 5
// km of radius, of 32 to 64 vertices, written with six decimals.
func benchAreas(b *testing.B, n int) *sets.Store {
	b.Helper()
	r := rand.New(rand.NewPCG(2026, 51))
	doc := []byte(`{"type":"FeatureCollection","features":[`)
	for i := range n {
		lat, lon := -55+r.Float64()*125, -179+r.Float64()*358
		km, vertices := 0.5+r.Float64()*4.5, 32+r.IntN(33)
		dLat, dLon := km/111.32, km/(111.32*math.Cos(lat*math.Pi/180))
		if i > 0 {
			doc = append(doc, ',')
		}
		doc = fmt.Appendf(doc, `{"type":"Feature","properties":{"name":"store-%d"},"geometry":{"type":"Polygon","coordinates":[[`, i+1)
		for v := 0; v <= vertices; v++ {
			angle := 2 * math.Pi * float64(v%vertices) / float64(vertices)
			if v > 0 {
				doc = append(doc, ',')
			}
			doc = fmt.Appendf(doc, "[%.6f,%.6f]", lon+dLon*math.Cos(angle), lat+dLat*math.Sin(angle))
		}
		doc = append(doc, "]]}}"...)
	}
	doc = append(doc, "]}"...)
	set, err := sets.Read(sets.Area, bytes.NewReader(doc), n)
	if err != nil {
		b.Fatal(err)
	}
	store, err := sets.NewStore([]sets.Declaration{{Name: "closed", Type: sets.Area, OnUnavailable: sets.Fail}})
	if err != nil {
		b.Fatal(err)
	}
	if err := store.Replace("closed", set); err != nil {
		b.Fatal(err)
	}
	return store
}

// benchCase is a scenario decided by the ten rules: after setRule on a set
// of networks networks when networks is not 0; after databaseRules with the
// large databases when databases; after userAgentRule, with the analysis of
// userAgent, when it is set; after areaRule on areas geofences, with the
// large databases, when areas is not 0; with benchUpstream when upstream,
// whose decision fields each decision computes too, as for a decision event.
type benchCase struct {
	name                string
	scenario            benchScenario
	networks, areas     int
	databases, upstream bool
	userAgent           *useragent.Analyzer
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
		// 64 headers, always in the cache of 10,000 entries, the default.
		{name: "last-ua-warm", scenario: withAgents(last, 64, benchAgents(64)), userAgent: useragent.New(10_000)},
		// 65,536 headers in turn: none is in the cache when it comes back.
		{name: "last-ua-cold", scenario: withAgents(last, 1<<16, benchAgents(1<<16)), userAgent: useragent.New(10_000)},
		{name: "default-area", scenario: byDefault, areas: 10_000},
	}
}

func benchEngine(b *testing.B, c benchCase) *Engine {
	b.Helper()
	p, opts := Policy{DefaultAction: Deny, DenyResponse: Response{Status: 403}, Rules: benchRules}, Options{}
	if c.networks > 0 {
		p.Rules, opts.Sets = append([]Rule{setRule}, benchRules...), benchSet(b, c.networks)
	}
	if c.databases || c.upstream || c.areas > 0 {
		opts.Databases, _ = benchDatabases()
	}
	if c.databases {
		p.Rules = append(slices.Clone(databaseRules), benchRules...)
	}
	if c.userAgent != nil {
		p.Rules, opts.UserAgent = append([]Rule{userAgentRule}, benchRules...), c.userAgent
	}
	if c.areas > 0 {
		p.Rules, opts.Sets = append([]Rule{areaRule}, benchRules...), benchAreas(b, c.areas)
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

// lookups returns the lookups in the cache of the analysis of the case so
// far, that found their header and that did not.
func (c benchCase) lookups() (hits, misses uint64) { return c.userAgent.Lookups() }

// reportHits reports, for a case with the analysis of the user-agent header,
// the share of the lookups in its cache since hits and misses that found
// their header, as ua-hit-%.
func (c benchCase) reportHits(b *testing.B, hits, misses uint64) {
	b.Helper()
	if c.userAgent == nil {
		return
	}
	h, m := c.lookups()
	if total := h - hits + m - misses; total > 0 {
		b.ReportMetric(100*float64(h-hits)/float64(total), "ua-hit-%")
	}
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
			hits, misses := c.lookups()
			b.ReportAllocs()
			i := 0
			for b.Loop() {
				c.decide(e, s.requests[i&mask])
				i++
			}
			c.reportHits(b, hits, misses)
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
			hits, misses := c.lookups()
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
			reportPercentiles(b, times)
			c.reportHits(b, hits, misses)
		})
	}
}

// reportPercentiles reports the 50th, 99th and 99.9th percentiles of times,
// in nanoseconds.
func reportPercentiles(b *testing.B, times []time.Duration) {
	b.Helper()
	if len(times) == 0 {
		return
	}
	slices.Sort(times)
	at := func(q float64) float64 { return float64(times[int(q*float64(len(times)-1))].Nanoseconds()) }
	b.ReportMetric(at(0.50), "p50-ns")
	b.ReportMetric(at(0.99), "p99-ns")
	b.ReportMetric(at(0.999), "p99.9-ns")
}

// parallelCases are the cases that BenchmarkDecisionLatencyParallel
// decides.
var parallelCases = []string{"last", "last-ua-warm", "last-ua-cold", "default-area"}

// BenchmarkDecisionLatencyParallel decides the requests of some cases from
// many goroutines at once, as tessera decides the calls that Envoy sends at
// once, with the same engine, and reports the percentiles of the time of each
// decision, in nanoseconds, with the time of the clock itself and the waits
// of a goroutine for a processor in each. The goroutines are GOMAXPROCS,
// then 16 times as many. They take the requests of the scenario in turns of
// 256, in order, from where the previous run of the case stopped, so that a
// request comes back only after all the others, as with one goroutine.
// ns/op is the time of all the goroutines divided by the number of
// decisions.
func BenchmarkDecisionLatencyParallel(b *testing.B) {
	for _, c := range benchCases() {
		if !slices.Contains(parallelCases, c.name) {
			continue
		}
		e, s := benchEngine(b, c), c.scenario
		checkScenario(b, e, c)
		mask := len(s.requests) - 1
		var next atomic.Int64
		for _, p := range []int{1, 16} {
			goroutines := p * runtime.GOMAXPROCS(0)
			b.Run(fmt.Sprintf("%s/goroutines=%d", c.name, goroutines), func(b *testing.B) {
				hits, misses := c.lookups()
				// Each goroutine keeps the times of its last decisions,
				// about a million in all.
				keep := max(1, (1<<20)/goroutines)
				var (
					mu    sync.Mutex
					times []time.Duration
				)
				b.SetParallelism(p)
				b.ResetTimer()
				b.RunParallel(func(pb *testing.PB) {
					own := make([]time.Duration, 0, keep)
					defer func() {
						mu.Lock()
						times = append(times, own...)
						mu.Unlock()
					}()
					for n := 0; ; {
						first := int(next.Add(256) - 256)
						for i := first; i < first+256; i++ {
							if !pb.Next() {
								return
							}
							start := time.Now()
							c.decide(e, s.requests[i&mask])
							elapsed := time.Since(start)
							if len(own) < keep {
								own = append(own, elapsed)
							} else {
								own[n%keep] = elapsed
							}
							n++
						}
					}
				})
				b.StopTimer()
				reportPercentiles(b, times)
				c.reportHits(b, hits, misses)
			})
		}
	}
}
