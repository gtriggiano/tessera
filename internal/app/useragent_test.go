package app

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"google.golang.org/grpc/codes"

	"github.com/gtriggiano/tessera/internal/useragent"
)

const chromeWindows = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.6778.86 Safari/537.36"

// expectedBot returns the bot that the analysis finds in header, with its
// name, which the tests of internal/useragent fix, and the description of the
// list of crawler-user-agents, both nonempty: a new name or a reworded
// description does not fail this test.
func expectedBot(t *testing.T, header string) useragent.Bot {
	t.Helper()
	b := useragent.New(0).Analyze(header).Bot
	if b.Name == "" || b.Description == "" {
		t.Fatalf("%s: bot %+v, without a name or a description: see Dependabot in CONTRIBUTING.md", header, b)
	}
	return b
}

// logfmtValue returns s as a logfmt event writes it: in double quotes, with
// escapes, when it is empty or holds a space, an equal sign, a double quote
// or a character that is not printable.
func logfmtValue(s string) string {
	if s == "" || strings.ContainsFunc(s, func(r rune) bool {
		return r == '=' || r == '"' || r == utf8.RuneError || unicode.IsSpace(r) || !unicode.IsPrint(r)
	}) {
		return strconv.Quote(s)
	}
	return s
}

// userAgentConfig denies the bots, sends the kind of device to the upstream
// and logs the name and the description of the bot, with a cache of the
// analysis of entries.
func userAgentConfig(entries string) string {
	return `server:
  listen: ` + grpcPlaceholder + `
  drain_delay: 0s
admin:
  listen: ` + adminPlaceholder + `
log:
  decisions: all
  decision_fields:
    bot: ua.bot.name
    bot_description: ua.bot.description
enrichment:
  user_agent:
    cache_entries: ` + entries + `
policy:
  default_action: allow
  rules:
    - name: bots
      action: deny
      when: ua.is_bot
upstream:
  headers:
    x-client-device: ua.device
`
}

// serve analyzes the user-agent header of the requests whose expressions
// read ua, through the cache that every decision shares, whose lookups and
// entries its metrics count from startup. The requests have no client
// address, which ua does not read.
func TestServeUserAgent(t *testing.T) {
	h := launch(t, userAgentConfig("100"), Options{})
	grpcAddress, adminAddress := h.addresses()
	lookups := func(metrics map[string]float64) (hits, misses, entries float64) {
		t.Helper()
		for _, s := range []string{`tessera_enrichment_user_agent_cache_lookups_total{result="hit"}`, `tessera_enrichment_user_agent_cache_lookups_total{result="miss"}`, `tessera_enrichment_user_agent_cache_entries`} {
			if _, ok := metrics[s]; !ok {
				t.Fatalf("no series %s", s)
			}
		}
		return metrics[`tessera_enrichment_user_agent_cache_lookups_total{result="hit"}`], metrics[`tessera_enrichment_user_agent_cache_lookups_total{result="miss"}`], metrics[`tessera_enrichment_user_agent_cache_entries`]
	}
	if hits, misses, entries := lookups(scrape(t, adminAddress)); hits != 0 || misses != 0 || entries != 0 {
		t.Fatalf("at startup: %v hits, %v misses, %v entries", hits, misses, entries)
	}

	conn := dialGRPC(t, grpcAddress)
	for _, tc := range []struct {
		agent string
		code  codes.Code
		set   []string
	}{
		{"curl/8.7.1", codes.PermissionDenied, nil},
		{chromeWindows, codes.OK, []string{"x-client-device=desktop"}},
		{chromeWindows, codes.OK, []string{"x-client-device=desktop"}},
		{"", codes.OK, []string{"x-client-device=unknown"}},
	} {
		headers := map[string]string{}
		if tc.agent != "" {
			headers["user-agent"] = tc.agent
		}
		resp := checkWith(t, conn, headers)
		if got := codes.Code(resp.GetStatus().GetCode()); got != tc.code {
			t.Errorf("%q: %v, want %v", tc.agent, got, tc.code)
		}
		if set, _ := upstreamHeaders(t, resp); !reflect.DeepEqual(set, tc.set) {
			t.Errorf("%q: sets %q, want %q", tc.agent, set, tc.set)
		}
	}
	// The lookups happen in the decisions, before the responses: a request
	// without the header looks nothing up.
	if hits, misses, entries := lookups(scrape(t, adminAddress)); hits != 1 || misses != 2 || entries != 2 {
		t.Errorf("after the requests: %v hits, %v misses, %v entries", hits, misses, entries)
	}

	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	var decisions []string
	for _, event := range h.events() {
		if strings.Contains(event, " msg=decision ") {
			decisions = append(decisions, durationRE.ReplaceAllString(event, "duration_ms=D"))
		}
	}
	curl := expectedBot(t, "curl/8.7.1")
	wantEvents(t, decisions, []string{
		`level=warn msg=decision request_id="" host=api.example.com method=GET path=/ client_ip="" action=deny enforced=true rule=bots reason=rule duration_ms=D bot=` + logfmtValue(curl.Name) + ` bot_description=` + logfmtValue(curl.Description),
		`level=info msg=decision request_id="" host=api.example.com method=GET path=/ client_ip="" action=allow enforced=true rule=default reason=default duration_ms=D bot="" bot_description=""`,
		`level=info msg=decision request_id="" host=api.example.com method=GET path=/ client_ip="" action=allow enforced=true rule=default reason=default duration_ms=D bot="" bot_description=""`,
		`level=info msg=decision request_id="" host=api.example.com method=GET path=/ client_ip="" action=allow enforced=true rule=default reason=default duration_ms=D bot="" bot_description=""`,
	})
}

// Without a cache, serve analyzes every header and has no metrics of the
// cache; an analysis that nothing reads is a config_warning.
func TestServeUserAgentWithoutCache(t *testing.T) {
	h := launch(t, userAgentConfig("0"), Options{})
	grpcAddress, adminAddress := h.addresses()
	resp := checkWith(t, dialGRPC(t, grpcAddress), map[string]string{"user-agent": "curl/8.7.1"})
	if got := codes.Code(resp.GetStatus().GetCode()); got != codes.PermissionDenied {
		t.Errorf("curl: %v", got)
	}
	for series := range scrape(t, adminAddress) {
		if strings.HasPrefix(series, "tessera_enrichment_user_agent_") {
			t.Errorf("series without a cache: %s", series)
		}
	}

	unused := launch(t, baseConfig("deny", "")+"enrichment:\n  user_agent: {}\n", Options{})
	unused.addresses()
	if err := unused.stop(); err != nil {
		t.Fatal(err)
	}
	if events := unused.events(); len(events) < 2 || events[1] != `level=warn msg=config_warning key=enrichment.user_agent warning="the enrichment is not used by any rule, upstream header or decision field"` {
		t.Errorf("events:\n%s", strings.Join(events, "\n"))
	}
}
