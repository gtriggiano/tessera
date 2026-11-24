//go:build e2e

package e2e

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gtriggiano/tessera/internal/useragent"
)

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

// fromAgents sends a GET request through Envoy to path, under /reflect/,
// with one User-Agent line for each of agents, none when there are none, and
// values that a client would forge for the upstream headers of the policy.
// It writes the request itself, since an HTTP client of Go sends one
// User-Agent line at most. It returns the status of the response and the
// values of the upstream headers x-client-device and x-client-bot that
// reached the upstream: "" for a header that did not.
func fromAgents(t *testing.T, front, path string, agents ...string) (status int, device, bot string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", front, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "GET /reflect/%s HTTP/1.1\r\nHost: www.example.com\r\nConnection: close\r\n", path)
	b.WriteString("X-Client-Device: desktop\r\nX-Client-Bot: Googlebot\r\n")
	for _, agent := range agents {
		fmt.Fprintf(&b, "User-Agent: %s\r\n", agent)
	}
	b.WriteString("\r\n")
	if _, err := io.WriteString(conn, b.String()); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("X-Upstream-Client-Device"), resp.Header.Get("X-Upstream-Client-Bot")
}

// metric returns the value of series, a metric name with its labels as
// /metrics writes them, on the admin endpoint admin.
func metric(t *testing.T, admin, series string) float64 {
	t.Helper()
	resp, err := http.Get("http://" + admin + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if value, ok := strings.CutPrefix(sc.Text(), series+" "); ok {
			n, err := strconv.ParseFloat(value, 64)
			if err != nil {
				t.Fatalf("%s: %v", sc.Text(), err)
			}
			return n
		}
	}
	t.Fatalf("no series %s in /metrics", series)
	return 0
}

// TestEnvoyUserAgent decides requests through Envoy with a rule on the bots,
// recognized in the user-agent header that Envoy passes to tessera: a bot is
// denied on the checkout pages, and the allowed requests reach the upstream
// with tessera's values of the kind of device and of the name of the bot,
// never the client's, and without a header whose value is empty. The
// decision event has the description of the bot, which the binary of the
// image reads from the list of crawler-user-agents. A repeated header
// reaches the analysis as Envoy joins it, with a comma.
func TestEnvoyUserAgent(t *testing.T) {
	e := setUp(t)
	config, err := filepath.Abs(filepath.Join("testdata", "useragent.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	tessera, front := e.start(t, "useragent", config)
	admin := hostAddress(t, tessera, "9090/tcp")

	const (
		chrome    = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.6778.86 Safari/537.36"
		iPhone    = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_6 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.6 Mobile/15E148 Safari/604.1"
		googlebot = "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)"
		curl      = "curl/8.7.1"
	)
	// The names and the descriptions of the bots, as the analysis of this
	// test gives them: the binary of the image must give the same.
	gb, cu := expectedBot(t, googlebot), expectedBot(t, curl)
	type want struct {
		status      int
		device, bot string // the values that reached the upstream
		rule        string
		// The decision fields agent, browser, device, bot and
		// bot_description.
		fields [5]string
	}
	for _, c := range []struct {
		name, path string
		agents     []string
		want       want
	}{
		// A browser: its device reaches the upstream, and the forged name
		// of a bot does not.
		{"desktop browser", "checkout/1", []string{chrome}, want{http.StatusOK, "desktop", "", "default", [5]string{chrome, "Chrome", "desktop", "", ""}}},
		{"mobile browser", "checkout/2", []string{iPhone}, want{http.StatusOK, "mobile", "", "default", [5]string{iPhone, "Safari", "mobile", "", ""}}},
		// A bot is denied on the checkout pages, and let through elsewhere
		// with its name.
		{"crawler on checkout", "checkout/3", []string{googlebot}, want{http.StatusForbidden, "", "", "checkout-bots", [5]string{googlebot, "", "bot", gb.Name, gb.Description}}},
		{"crawler elsewhere", "catalog/4", []string{googlebot}, want{http.StatusOK, "bot", gb.Name, "default", [5]string{googlebot, "", "bot", gb.Name, gb.Description}}},
		{"script on checkout", "checkout/5", []string{curl}, want{http.StatusForbidden, "", "", "checkout-bots", [5]string{curl, "", "bot", cu.Name, cu.Description}}},
		{"script elsewhere", "catalog/6", []string{curl}, want{http.StatusOK, "bot", cu.Name, "default", [5]string{curl, "", "bot", cu.Name, cu.Description}}},
		// Without the header: the device is unknown, and not a bot.
		{"no header", "checkout/7", nil, want{http.StatusOK, "unknown", "", "default", [5]string{"", "", "unknown", "", ""}}},
		// Two User-Agent lines: Envoy joins them with a comma, and the
		// analysis reads the joined value, which starts with curl.
		{"repeated header", "checkout/8", []string{curl, chrome}, want{http.StatusForbidden, "", "", "checkout-bots", [5]string{curl + "," + chrome, "", "bot", cu.Name, cu.Description}}},
	} {
		status, device, bot := fromAgents(t, front, c.path, c.agents...)
		if status != c.want.status || device != c.want.device || bot != c.want.bot {
			t.Errorf("%s: status %d, upstream x-client-device %q and x-client-bot %q; want %d, %q and %q", c.name, status, device, bot, c.want.status, c.want.device, c.want.bot)
		}
		path := "/reflect/" + c.path
		event := decisionEvents(t, tessera, []string{path})[path]
		got := [5]any{event["agent"], event["browser"], event["device"], event["bot"], event["bot_description"]}
		if event["rule"] != c.want.rule || got != [5]any{c.want.fields[0], c.want.fields[1], c.want.fields[2], c.want.fields[3], c.want.fields[4]} {
			t.Errorf("%s: decision event %v, want rule %s and fields agent, browser, device, bot and bot_description %q", c.name, event, c.want.rule, c.want.fields)
		}
	}

	// The second request of each bot found its analysis in the cache.
	if hits := metric(t, admin, `tessera_enrichment_user_agent_cache_lookups_total{result="hit"}`); hits < 2 {
		t.Errorf("%v lookups found their header in the cache, want 2 at least", hits)
	}
	if entries := metric(t, admin, "tessera_enrichment_user_agent_cache_entries"); entries < 5 || entries > 10000 {
		t.Errorf("%v entries in the cache, want 5 to 10000", entries)
	}
	// No analysis failed.
	if failed := events(t, tessera, "evaluation_error"); len(failed) != 0 {
		t.Errorf("evaluation_error events: %v", failed)
	}
	if recovered := events(t, tessera, "panic_recovered"); len(recovered) != 0 {
		t.Errorf("panic_recovered events: %v", recovered)
	}
}
