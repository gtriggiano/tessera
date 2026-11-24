package engine

import (
	"reflect"
	"strings"
	"testing"

	"github.com/gtriggiano/tessera/internal/useragent"
)

const (
	chromeWindows = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.6778.86 Safari/537.36"
	safariIOS14   = "Mozilla/5.0 (iPhone; CPU iPhone OS 14_8 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/14.1.2 Mobile/15E148 Safari/604.1"
)

func TestUserAgent(t *testing.T) {
	ua := useragent.New(100)
	e := newEngine(t, Policy{DefaultAction: Allow, Rules: []Rule{
		{Name: "blocked", When: `request.path == "/blocked"`, Action: Deny},
		{Name: "bots", When: `ua.is_bot && request.path.startsWith("/checkout")`, Action: Deny},
		{Name: "desktop", When: `ua.device == "desktop" && ua.browser.name == "Chrome"`, Action: Allow},
		{Name: "old-ios", When: `ua.os.name == "iOS" && ua.browser.major < 15`, Action: Deny},
	}, UpstreamHeaders: []NamedExpression{
		{Name: "x-client-device", Expression: `ua.device`},
	}, DecisionFields: []NamedExpression{
		{Name: "bot", Expression: `ua.bot.name`},
	}}, Options{UserAgent: ua})
	decide := func(agent, path string) Decision {
		t.Helper()
		r := req("192.0.2.7")
		r.Path = path
		delete(r.Headers, "user-agent")
		if agent != "" {
			r.Headers["user-agent"] = agent
		}
		return e.Decide(r)
	}
	lookups := func() (uint64, uint64) {
		t.Helper()
		return ua.Lookups()
	}

	// A denial by a rule that does not read ua analyzes nothing; its
	// decision fields, computed for its event, analyze the header then.
	d := decide("curl/8.7.1", "/blocked")
	if hits, misses := lookups(); d.Rule != "blocked" || hits+misses != 0 {
		t.Fatalf("blocked: %+v, lookups %d, %d", summary(d), hits, misses)
	}
	// The name that the analysis gives to curl, which the tests of
	// internal/useragent fix.
	curl := useragent.New(0).Analyze("curl/8.7.1").Bot.Name
	if curl == "" {
		t.Fatal("curl has no bot name")
	}
	if fields := e.DecisionFields(d); !reflect.DeepEqual(fields, []Field{{Name: "bot", Value: curl}}) {
		t.Errorf("fields %+v", fields)
	}
	if hits, misses := lookups(); hits != 0 || misses != 1 {
		t.Fatalf("after the decision fields: lookups %d, %d", hits, misses)
	}

	// Every expression of a decision reads one analysis: the rules, the
	// upstream header and the decision field.
	d = decide(safariIOS14, "/")
	if d.Action != Deny || d.Rule != "old-ios" {
		t.Fatalf("old iOS: %+v", summary(d))
	}
	if fields := e.DecisionFields(d); !reflect.DeepEqual(fields, []Field{{Name: "bot", Value: ""}}) {
		t.Errorf("fields %+v", fields)
	}
	d = decide(chromeWindows, "/")
	if d.Rule != "desktop" || !reflect.DeepEqual(d.Upstream, []UpstreamHeader{{Name: "x-client-device", Value: "desktop"}}) {
		t.Errorf("desktop: %+v", summary(d))
	}
	e.DecisionFields(d)
	if hits, misses := lookups(); hits != 0 || misses != 3 {
		t.Fatalf("lookups %d, %d; want one analysis per decision", hits, misses)
	}
	// The next decision with a header already analyzed finds it in the
	// cache.
	if d = decide("curl/8.7.1", "/checkout/pay"); d.Action != Deny || d.Rule != "bots" {
		t.Errorf("bots: %+v", summary(d))
	}
	if hits, misses := lookups(); hits != 1 || misses != 3 {
		t.Errorf("lookups %d, %d; want a hit", hits, misses)
	}

	// Without the header, ua reads empty values: the request is no bot,
	// and nothing is looked up.
	d = decide("", "/checkout/pay")
	if d.Action != Allow || d.Rule != DefaultRule || !reflect.DeepEqual(d.Upstream, []UpstreamHeader{{Name: "x-client-device", Value: "unknown"}}) {
		t.Errorf("without a user-agent header: %+v", summary(d))
	}
	if hits, misses := lookups(); hits != 1 || misses != 3 {
		t.Errorf("a request without the header was looked up: %d, %d", hits, misses)
	}

	// What Explain reads of ua is what the conditions read.
	r := req("192.0.2.7")
	r.Headers["user-agent"] = chromeWindows
	x := e.Explain(r)
	if u, err := x.Input.UserAgent(); err != nil || u.Browser.Name != "Chrome" || u.Device != useragent.DeviceDesktop {
		t.Errorf("UserAgent: %+v, %v", u, err)
	}
	if got := x.Attributes; !reflect.DeepEqual(got, []string{"request.path", "ua.browser.name", "ua.device", "ua.is_bot"}) {
		t.Errorf("attributes %v", got)
	}
}

// Without the analysis, a policy that reads ua does not compile.
func TestUserAgentNotConfigured(t *testing.T) {
	_, err := New(Policy{DefaultAction: Allow, Rules: []Rule{{Name: "bots", When: `ua.is_bot`, Action: Deny}}}, Options{})
	if err == nil || !strings.Contains(err.Error(), "ua is not available: the configuration has no enrichment.user_agent") {
		t.Errorf("New: %v", err)
	}
}
