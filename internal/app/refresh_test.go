package app

import (
	"context"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

// fileSetConfig declares the set blocked, read from path, with the keys of
// the set in keys, and a deny rule that reads it; the default action allows.
func fileSetConfig(path, server, keys string) string {
	return `server:
  listen: ` + grpcPlaceholder + `
` + server + `admin:
  listen: ` + adminPlaceholder + `
client_ip:
  from: x_forwarded_for
  trusted_hops: 0
log:
  decisions: none
sets:
  blocked:
    type: cidr
    file: ` + path + `
` + keys + `policy:
  default_action: allow
  rules:
    - name: blocked
      action: deny
      when: sets.blocked.contains(client.ip)
`
}

// fastSets makes the refresh and the max_staleness of the sets a hundred
// times shorter than configured: 1s becomes 10ms.
func fastSets(d time.Duration) time.Duration { return d / 100 }

// decide returns the status of the decision for a request from the client
// address addr.
func decide(t *testing.T, conn *grpc.ClientConn, addr string) codes.Code {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := authv3.NewAuthorizationClient(conn).Check(ctx, &authv3.CheckRequest{Attributes: &authv3.AttributeContext{
		Request: &authv3.AttributeContext_Request{Http: &authv3.AttributeContext_HttpRequest{
			Method: "GET", Host: "api.example.com", Path: "/", Headers: map[string]string{"x-forwarded-for": addr},
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return codes.Code(resp.GetStatus().GetCode())
}

// wantDecisions checks the decisions for a request from each address.
func wantDecisions(t *testing.T, conn *grpc.ClientConn, want map[string]codes.Code) {
	t.Helper()
	for addr, code := range want {
		if got := decide(t, conn, addr); got != code {
			t.Fatalf("decision for %s: %v, want %v", addr, got, code)
		}
	}
}

// scrape returns the value of each tessera_ series of /metrics.
func scrape(t *testing.T, adminAddress string) map[string]float64 {
	t.Helper()
	code, body := get(t, "http://"+adminAddress+"/metrics")
	if code != 200 {
		t.Fatalf("/metrics: %d", code)
	}
	out := map[string]float64{}
	for line := range strings.Lines(body) {
		if !strings.HasPrefix(line, "tessera_") {
			continue
		}
		i := strings.LastIndexByte(line, ' ')
		v, err := strconv.ParseFloat(strings.TrimSpace(line[i+1:]), 64)
		if err != nil {
			t.Fatalf("/metrics: %q", line)
		}
		out[line[:i]] = v
	}
	return out
}

// waitUntil waits until cond holds, and fails after 10 seconds.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// replaceSetFile writes content to a new file and renames it over path.
func replaceSetFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path+".tmp", []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		t.Fatal(err)
	}
}

// refresherGoroutines counts the goroutines of the refreshers of the sets.
func refresherGoroutines() int {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	n := 0
	for g := range strings.SplitSeq(string(buf), "\n\n") {
		if strings.Contains(g, "internal/source.(*refresher)") {
			n++
		}
	}
	return n
}

// noRefresherLeft waits until every goroutine of the refreshers has
// returned.
func noRefresherLeft(t *testing.T) {
	t.Helper()
	waitUntil(t, "the refreshers to return", func() bool { return refresherGoroutines() == 0 })
}

var durationMS = regexp.MustCompile(` duration_ms=[0-9.e+-]+`)

// setEvents returns the set_ events, without their duration_ms.
func setEvents(h *harness) []string {
	var out []string
	for _, e := range h.events() {
		if strings.Contains(e, " msg=set_") {
			out = append(out, durationMS.ReplaceAllString(e, ""))
		}
	}
	return out
}

const (
	inSet    = "198.51.100.7"
	notInSet = "203.0.113.7"
	blocked  = `set="blocked",source="file",type="cidr"`
)

func refreshes(result string) string {
	return `tessera_set_refreshes_total{result="` + result + `",` + blocked + `}`
}

// The exit criterion of the sets: a modified file changes the decisions
// without a restart; an invalid file, or one over max_entries, changes
// nothing, shows in the metrics and in one set_load_failed event; so does a
// file that disappears.
func TestFileSetRefresh(t *testing.T) {
	path := writeSetFile(t, "198.51.100.0/24\n")
	started := time.Now()
	h := launch(t, fileSetConfig(path, "  drain_delay: 0s\n", "    refresh: 1s\n    max_entries: 2\n"), Options{setTime: fastSets})
	grpcAddress, adminAddress := h.addresses()
	conn := dialGRPC(t, grpcAddress)
	wantDecisions(t, conn, map[string]codes.Code{inSet: codes.PermissionDenied, notInSet: codes.OK})

	// The load at startup is a refresh; the checks of the unchanged file
	// are successful, and the histogram observes them. A check is counted
	// before it is observed: wait for both.
	waitUntil(t, "an unchanged check, observed", func() bool {
		m := scrape(t, adminAddress)
		return m[refreshes("unchanged")] > 0 && m["tessera_set_refresh_duration_seconds_count{"+blocked+"}"] >= 2
	})
	m := scrape(t, adminAddress)
	for series, want := range map[string]float64{
		"tessera_set_entries{" + blocked + "}":   1,
		"tessera_set_available{" + blocked + "}": 1,
		refreshes("updated"):                     1,
		refreshes("rejected"):                    0,
		refreshes("error"):                       0,
	} {
		if m[series] != want {
			t.Errorf("%s = %v, want %v", series, m[series], want)
		}
	}
	if last := m["tessera_set_last_refresh_success_timestamp_seconds{"+blocked+"}"]; last < float64(started.Unix()) || last > float64(time.Now().Unix()+1) {
		t.Errorf("last success at %v, started at %v", last, started.Unix())
	}

	// A modified file changes the decisions.
	replaceSetFile(t, path, "203.0.113.0/24\n203.0.113.8\n")
	waitUntil(t, "the modified file", func() bool { return decide(t, conn, notInSet) == codes.PermissionDenied })
	wantDecisions(t, conn, map[string]codes.Code{inSet: codes.OK})

	// An invalid file, a file over max_entries, and a missing file change
	// nothing.
	for _, tc := range []struct {
		change func()
		result string
	}{
		{func() { replaceSetFile(t, path, "203.0.113.0/24\nnot-an-address\n") }, "rejected"},
		{func() { replaceSetFile(t, path, "203.0.113.0/24\n192.0.2.0/24\n198.51.100.0/24\n") }, "rejected"},
		{func() {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}, "error"},
	} {
		before := scrape(t, adminAddress)[refreshes(tc.result)]
		tc.change()
		waitUntil(t, "three failed checks", func() bool { return scrape(t, adminAddress)[refreshes(tc.result)] >= before+3 })
		wantDecisions(t, conn, map[string]codes.Code{inSet: codes.OK, notInSet: codes.PermissionDenied})
	}
	m = scrape(t, adminAddress)
	for series, want := range map[string]float64{
		"tessera_set_entries{" + blocked + "}":   2,
		"tessera_set_available{" + blocked + "}": 1,
		refreshes("updated"):                     2,
	} {
		if m[series] != want {
			t.Errorf("%s = %v, want %v", series, m[series], want)
		}
	}

	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	if n := refresherGoroutines(); n != 0 {
		t.Errorf("%d goroutines of the refreshers left", n)
	}
	events := h.events()
	for i, e := range events {
		events[i] = durationMS.ReplaceAllString(e, "")
	}
	wantEvents(t, events, []string{
		`level=info msg=starting version=1.2.3 revision=abc123 config_path=` + h.path,
		`level=info msg=set_loaded set=blocked set_type=cidr set_source=file entries=1`,
		`level=info msg=listening listener=grpc address=` + grpcAddress,
		`level=info msg=listening listener=admin address=` + adminAddress,
		`level=info msg=ready`,
		`level=info msg=set_loaded set=blocked set_type=cidr set_source=file entries=2`,
		`level=warn msg=set_load_failed set=blocked set_type=cidr set_source=file error="line 2 of the file: expected an IP address or a network, such as 192.0.2.0/24 or 2001:db8::/32"`,
		`level=warn msg=set_load_failed set=blocked set_type=cidr set_source=file error="line 3 of the file: more entries than max_entries"`,
		`level=warn msg=set_load_failed set=blocked set_type=cidr set_source=file error="the file does not exist"`,
		`level=info msg=shutdown_started reason=signal`,
		`level=info msg=not_ready`,
		`level=info msg=shutdown_complete outcome=drained`,
	})
	// duration_ms has the format of that of decision.
	if !regexp.MustCompile(` msg=set_loaded set=blocked set_type=cidr set_source=file entries=1 duration_ms=[0-9.e+-]+\n`).MatchString(h.logs.String()) {
		t.Errorf("no duration_ms in set_loaded:\n%s", h.logs.String())
	}
}

// A set whose last successful check is older than max_staleness is
// unavailable: contains answers what on_unavailable says, until a check
// succeeds again.
func TestStaleSet(t *testing.T) {
	for _, tc := range []struct {
		onUnavailable string
		code          codes.Code
		series        string // counts the decision for an address not in the set
	}{
		{"fail", codes.PermissionDenied, `tessera_decisions_total{action="deny",enforced="true",reason="error",rule="blocked"}`},
		{"match", codes.PermissionDenied, `tessera_decisions_total{action="deny",enforced="true",reason="rule",rule="blocked"}`},
		{"no_match", codes.OK, `tessera_decisions_total{action="allow",enforced="true",reason="default",rule="default"}`},
	} {
		t.Run(tc.onUnavailable, func(t *testing.T) {
			path := writeSetFile(t, "198.51.100.0/24\n")
			// max_staleness is a hundred checks: 10ms and 1s with fastSets,
			// so that a goroutine held off for less than a second makes no
			// premature staleness.
			h := launch(t, fileSetConfig(path, "  drain_delay: 0s\n", "    refresh: 1s\n    max_staleness: 100s\n    on_unavailable: "+tc.onUnavailable+"\n"), Options{setTime: fastSets})
			grpcAddress, adminAddress := h.addresses()
			conn := dialGRPC(t, grpcAddress)
			wantDecisions(t, conn, map[string]codes.Code{inSet: codes.PermissionDenied, notInSet: codes.OK})

			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			waitUntil(t, "the set to be unavailable", func() bool { return scrape(t, adminAddress)["tessera_set_available{"+blocked+"}"] == 0 })
			before := scrape(t, adminAddress)
			if got := decide(t, conn, notInSet); got != tc.code {
				t.Fatalf("decision on a stale set: %v, want %v", got, tc.code)
			}
			after := scrape(t, adminAddress)
			if after[tc.series] != before[tc.series]+1 {
				t.Errorf("%s = %v, was %v", tc.series, after[tc.series], before[tc.series])
			}
			errors := `tessera_rule_evaluation_errors_total{cause="set_unavailable",rule="blocked"}`
			if want := map[bool]float64{true: 1, false: 0}[tc.onUnavailable == "fail"]; after[errors]-before[errors] != want {
				t.Errorf("%s grew by %v, want %v", errors, after[errors]-before[errors], want)
			}
			if after[refreshes("error")] == 0 {
				t.Error("the failed checks are not counted")
			}

			// The file is back: the next check makes the set available.
			replaceSetFile(t, path, "198.51.100.0/24\n")
			waitUntil(t, "the set to be available", func() bool { return scrape(t, adminAddress)["tessera_set_available{"+blocked+"}"] == 1 })
			wantDecisions(t, conn, map[string]codes.Code{inSet: codes.PermissionDenied, notInSet: codes.OK})
			if err := h.stop(); err != nil {
				t.Fatal(err)
			}
			wantEvents(t, setEvents(h), []string{
				`level=info msg=set_loaded set=blocked set_type=cidr set_source=file entries=1`,
				`level=warn msg=set_load_failed set=blocked set_type=cidr set_source=file error="the file does not exist"`,
				`level=warn msg=set_unavailable set=blocked set_type=cidr set_source=file reason=stale`,
				`level=info msg=set_available set=blocked set_type=cidr set_source=file`,
			})
		})
	}
}

// The refreshers stop on every path of a shutdown, and log nothing after
// shutdown_complete.
func TestRefreshersStop(t *testing.T) {
	config := func(t *testing.T, server string) string {
		return fileSetConfig(writeSetFile(t, "198.51.100.0/24\n"), server, "    refresh: 1s\n    max_staleness: 1h\n")
	}
	lastEvent := func(t *testing.T, h *harness, want string) {
		t.Helper()
		noRefresherLeft(t)
		if events := h.events(); events[len(events)-1] != want {
			t.Fatalf("last event %q, want %q", events[len(events)-1], want)
		}
	}
	t.Run("a second signal", func(t *testing.T) {
		h := launch(t, config(t, "  drain_delay: 1h\n"), Options{setTime: fastSets})
		h.addresses()
		h.signals <- syscall.SIGTERM
		h.waitFor("not_ready")
		if err := h.stopWithin(2 * time.Second); err == nil || err.Error() != "shutdown forced by signal" {
			t.Fatalf("Run: %v", err)
		}
		lastEvent(t, h, `level=warn msg=shutdown_complete outcome=forced forced_by=signal`)
	})
	t.Run("the shutdown timeout", func(t *testing.T) {
		b := newBlocker()
		defer close(b.release)
		h := launch(t, config(t, "  drain_delay: 0s\n  shutdown_timeout: 200ms\n"), Options{setTime: fastSets, decider: b})
		grpcAddress, _ := h.addresses()
		go func() { _, _ = check(dialGRPC(t, grpcAddress)) }()
		<-b.entered
		if err := h.stopWithin(2 * time.Second); err == nil || err.Error() != "shutdown forced by timeout" {
			t.Fatalf("Run: %v", err)
		}
		lastEvent(t, h, `level=warn msg=shutdown_complete outcome=forced forced_by=timeout`)
	})
	t.Run("a listener failure", func(t *testing.T) {
		h := launch(t, config(t, "  drain_delay: 0s\n"), Options{setTime: fastSets})
		h.addresses()
		_ = h.listener(grpcPlaceholder).Close()
		if err := h.result(); err == nil || err.Error() != "the grpc listener failed" {
			t.Fatalf("Run: %v", err)
		}
		if n := refresherGoroutines(); n != 0 {
			t.Errorf("%d goroutines of the refreshers left", n)
		}
		lastEvent(t, h, `level=info msg=shutdown_complete outcome=drained`)
	})
	t.Run("a listener that cannot open", func(t *testing.T) {
		yaml := strings.Replace(config(t, ""), grpcPlaceholder, slowAddress, 1)
		h := launch(t, yaml, Options{setTime: fastSets, listenTimeout: 100 * time.Millisecond})
		if err := h.result(); err == nil || err.Error() != "cannot open the gRPC listener" {
			t.Fatalf("Run: %v", err)
		}
		lastEvent(t, h, `level=error msg=startup_failed listener=grpc address=`+slowAddress+` error="timed out"`)
	})
}

// The set_ events name the set, its type and its source, in this order and
// right after msg, in JSON too, with the values of the labels of the metrics.
func TestSetEventsInJSON(t *testing.T) {
	path := writeSetFile(t, "198.51.100.0/24\n")
	yaml := fileSetConfig(path, "  drain_delay: 0s\n", "    refresh: 1s\n    max_staleness: 100s\n")
	yaml = strings.Replace(yaml, "log:\n  decisions: none\n", "log:\n  decisions: none\n  format: json\n", 1)
	yaml = strings.Replace(yaml, "sets:\n", "sets:\n  office:\n    type: asn\n    values: [64496]\n", 1)
	h := launch(t, yaml, Options{setTime: fastSets})
	logged := func(msg string) func() bool {
		return func() bool { return strings.Contains(h.logs.String(), `"msg":"`+msg+`"`) }
	}
	waitUntil(t, "ready", logged("ready"))
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "set_unavailable", logged("set_unavailable"))
	replaceSetFile(t, path, "198.51.100.0/24\n")
	waitUntil(t, "set_available", logged("set_available"))
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	logs := h.logs.String()
	for _, want := range []string{
		`"level":"info","msg":"set_loaded","set":"office","set_type":"asn","set_source":"values","entries":1,"duration_ms":`,
		`"level":"info","msg":"set_loaded","set":"blocked","set_type":"cidr","set_source":"file","entries":1,"duration_ms":`,
		`"level":"warn","msg":"set_load_failed","set":"blocked","set_type":"cidr","set_source":"file","error":"the file does not exist"}` + "\n",
		`"level":"warn","msg":"set_unavailable","set":"blocked","set_type":"cidr","set_source":"file","reason":"stale"}` + "\n",
		`"level":"info","msg":"set_available","set":"blocked","set_type":"cidr","set_source":"file"}` + "\n",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("no %s in:\n%s", want, logs)
		}
	}
}
