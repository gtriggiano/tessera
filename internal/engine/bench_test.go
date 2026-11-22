package engine

import (
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"net/netip"
	"slices"
	"testing"
	"time"

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
	name     string
	requests []*request.Request
	rule     string
	action   Action
}

// benchScenarios returns 64 different requests for each scenario.
func benchScenarios() []benchScenario {
	last := benchScenario{name: "last", rule: "api", action: Allow}
	early := benchScenario{name: "early", rule: "admin-outside", action: Deny}
	for i := range 64 {
		headers := map[string]string{
			"user-agent":      "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_5) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Safari/605.1.15",
			"accept":          "application/json",
			"accept-encoding": "gzip, br",
			"content-type":    "application/json",
			"x-request-id":    fmt.Sprintf("req-%04d", i),
			"x-forwarded-for": fmt.Sprintf("192.0.2.%d", i+1),
		}
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

// setRule is the rule added before the ten in the cases with a set: a deny
// rule on a cidr set, which the requests of the scenario "last" do not match.
var setRule = Rule{Name: "blocked-set", When: `sets.blocked.contains(client.ip)`, Action: Deny}

// benchCase is a scenario decided by the ten rules, after setRule on a set of
// networks networks when networks is not 0.
type benchCase struct {
	name     string
	scenario benchScenario
	networks int
}

func benchCases() []benchCase {
	scenarios := benchScenarios()
	last, early := scenarios[0], scenarios[1]
	return []benchCase{
		{"last", last, 0},
		{"early", early, 0},
		{"last-set-100000", last, 100_000},
		{"last-set-1000000", last, 1_000_000},
	}
}

func benchEngine(b *testing.B, networks int) *Engine {
	b.Helper()
	rules, opts := benchRules, Options{}
	if networks > 0 {
		rules, opts.Sets = append([]Rule{setRule}, benchRules...), benchSet(b, networks)
	}
	e, err := New(Policy{DefaultAction: Deny, DenyResponse: Response{Status: 403}, Rules: rules}, opts)
	if err != nil {
		b.Fatal(err)
	}
	return e
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

func checkScenario(b *testing.B, e *Engine, s benchScenario) {
	b.Helper()
	for i, r := range s.requests {
		if d := e.Decide(r); d.Action != s.action || d.Rule != s.rule || len(d.Errors) != 0 {
			b.Fatalf("%s request %d: %+v, want %s by %s", s.name, i, d, s.action, s.rule)
		}
	}
}

// BenchmarkDecision times one decision over the ten rules, from the
// normalized request to the decision, the cost limit included.
func BenchmarkDecision(b *testing.B) {
	for _, c := range benchCases() {
		e, s := benchEngine(b, c.networks), c.scenario
		checkScenario(b, e, s)
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			i := 0
			for b.Loop() {
				e.Decide(s.requests[i&63])
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
		e, s := benchEngine(b, c.networks), c.scenario
		checkScenario(b, e, s)
		b.Run(c.name, func(b *testing.B) {
			// The times of the last million decisions at most. b.Loop runs
			// this function once, so b.N is not known here.
			times := make([]time.Duration, 0, 1<<20)
			i := 0
			for b.Loop() {
				start := time.Now()
				e.Decide(s.requests[i&63])
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
