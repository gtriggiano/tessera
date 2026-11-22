package engine

import (
	"fmt"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/gtriggiano/tessera/internal/request"
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

func benchEngine(b *testing.B) *Engine {
	b.Helper()
	e, err := New(Policy{DefaultAction: Deny, DenyResponse: Response{Status: 403}, Rules: benchRules}, Options{})
	if err != nil {
		b.Fatal(err)
	}
	return e
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
	e := benchEngine(b)
	for _, s := range benchScenarios() {
		checkScenario(b, e, s)
		b.Run(s.name, func(b *testing.B) {
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
	e := benchEngine(b)
	for _, s := range benchScenarios() {
		checkScenario(b, e, s)
		b.Run(s.name, func(b *testing.B) {
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
