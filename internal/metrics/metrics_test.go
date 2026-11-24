package metrics

import (
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/gtriggiano/tessera/internal/engine"
	"github.com/gtriggiano/tessera/internal/enrichment"
	"github.com/gtriggiano/tessera/internal/expr"
	"github.com/gtriggiano/tessera/internal/sets"
	"github.com/gtriggiano/tessera/internal/useragent"
)

var testBuild = BuildInfo{Version: "1.2.3", Revision: "abc123", GoVersion: "go1.test"}

const testHash = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"

// testPolicy has a rule of each kind: allow, deny, and audit, and two
// upstream headers.
var testPolicy = engine.Policy{DefaultAction: engine.Deny, Rules: []engine.Rule{
	{Name: "blocked", Action: engine.Deny},
	{Name: "watch", Action: engine.Deny, Audit: true},
	{Name: "office", Action: engine.Allow},
}, UpstreamHeaders: []engine.NamedExpression{
	{Name: "X-Client-Country", Expression: "geo.country.code"},
	{Name: "x-client-asn", Expression: "string(asn.number)"},
}}

// testDatabases configures the geo database only.
var testDatabases = []enrichment.Kind{enrichment.Geo}

// testSets has a set of each type, read from a file or of values.
var testSets = []sets.Declaration{
	{Name: "blocked", Type: sets.CIDR, Source: sets.SourceFile, OnUnavailable: sets.Fail},
	{Name: "networks", Type: sets.ASN, Source: sets.SourceValues, OnUnavailable: sets.Fail},
	{Name: "partners", Type: sets.String, Source: sets.SourceFile, OnUnavailable: sets.NoMatch},
}

func gather(t *testing.T, m *Metrics) map[string]*dto.MetricFamily {
	t.Helper()
	families, err := m.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*dto.MetricFamily{}
	for _, f := range families {
		byName[f.GetName()] = f
	}
	return byName
}

func labels(m *dto.Metric) map[string]string {
	out := map[string]string{}
	for _, l := range m.GetLabel() {
		out[l.GetName()] = l.GetValue()
	}
	return out
}

func TestInfoGauges(t *testing.T) {
	families := gather(t, New(testBuild, testHash, testPolicy, testSets, testDatabases, useragent.New(100)))
	for _, tc := range []struct {
		name   string
		labels map[string]string
	}{
		{"tessera_build_info", map[string]string{"version": "1.2.3", "revision": "abc123", "go_version": "go1.test"}},
		{"tessera_config_info", map[string]string{"hash": testHash}},
	} {
		f, ok := families[tc.name]
		if !ok || f.GetType() != dto.MetricType_GAUGE || len(f.GetMetric()) != 1 {
			t.Fatalf("%s: %v", tc.name, f)
		}
		m := f.GetMetric()[0]
		if m.GetGauge().GetValue() != 1 {
			t.Errorf("%s = %v, want 1", tc.name, m.GetGauge().GetValue())
		}
		got := labels(m)
		if len(got) != len(tc.labels) {
			t.Errorf("%s: labels %v, want %v", tc.name, got, tc.labels)
		}
		for k, v := range tc.labels {
			if got[k] != v {
				t.Errorf("%s: labels %v, want %v", tc.name, got, tc.labels)
			}
		}
	}
	// The Go collector; the process families depend on the platform.
	for _, name := range []string{"go_goroutines", "go_info"} {
		if _, ok := families[name]; !ok {
			t.Errorf("%s is not exported", name)
		}
	}
}

func TestConnectionRefused(t *testing.T) {
	m := New(testBuild, testHash, testPolicy, testSets, testDatabases, useragent.New(100))
	value := func() float64 {
		f := gather(t, m)["tessera_grpc_connections_refused_total"]
		if f.GetType() != dto.MetricType_COUNTER || len(f.GetMetric()) != 1 || len(f.GetMetric()[0].GetLabel()) != 0 {
			t.Fatalf("tessera_grpc_connections_refused_total: %v", f)
		}
		return f.GetMetric()[0].GetCounter().GetValue()
	}
	if got := value(); got != 0 {
		t.Fatalf("at startup: %v", got)
	}
	m.ConnectionRefused()
	m.ConnectionRefused()
	if got := value(); got != 2 {
		t.Fatalf("after two refusals: %v", got)
	}
}

func TestHandlerServesTheTextFormat(t *testing.T) {
	rec := httptest.NewRecorder()
	New(testBuild, testHash, testPolicy, testSets, testDatabases, useragent.New(100)).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("status %d, content type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	for _, line := range []string{
		`tessera_build_info{go_version="go1.test",revision="abc123",version="1.2.3"} 1`,
		`tessera_config_info{hash="` + testHash + `"} 1`,
	} {
		if !strings.Contains(body, "\n"+line+"\n") {
			t.Errorf("no line %s in:\n%s", line, body)
		}
	}
}

// series returns the tessera_ series of a scrape, without their values.
func series(t *testing.T, m *Metrics) []string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	var out []string
	for line := range strings.Lines(rec.Body.String()) {
		if strings.HasPrefix(line, "tessera_") {
			out = append(out, line[:strings.LastIndexByte(line, ' ')])
		}
	}
	return out
}

// Every series that the policy can produce exists from the start, and no
// decision adds one.
func TestDecisionSeries(t *testing.T) {
	for _, audit := range []bool{false, true} {
		p := testPolicy
		p.Audit = audit
		m := New(testBuild, testHash, p, testSets, testDatabases, useragent.New(100))
		before := series(t, m)
		enforced := "true"
		if audit {
			enforced = "false"
		}
		for _, line := range []string{
			`tessera_decisions_total{action="deny",enforced="` + enforced + `",reason="rule",rule="blocked"}`,
			`tessera_decisions_total{action="deny",enforced="` + enforced + `",reason="error",rule="blocked"}`,
			`tessera_decisions_total{action="allow",enforced="true",reason="rule",rule="office"}`,
			`tessera_decisions_total{action="deny",enforced="` + enforced + `",reason="default",rule="default"}`,
			`tessera_audit_matches_total{rule="watch"}`,
			`tessera_rule_evaluation_errors_total{cause="client_ip_missing",rule="office"}`,
			`tessera_rule_evaluation_errors_total{cause="panic",rule="watch"}`,
			`tessera_rule_evaluation_errors_total{cause="set_unavailable",rule="office"}`,
			`tessera_rule_evaluation_errors_total{cause="set_unavailable",rule="watch"}`,
			`tessera_rule_evaluation_errors_total{cause="database_error",rule="blocked"}`,
			`tessera_rule_evaluation_errors_total{cause="database_error",rule="watch"}`,
			`tessera_decision_duration_seconds_bucket{action="allow",le="2.5e-05"}`,
			`tessera_decision_duration_seconds_count{action="deny"}`,
			`tessera_decision_logs_dropped_total`,
			`tessera_policy_audit_mode`,
		} {
			if !slices.Contains(before, line) {
				t.Errorf("audit %t: no series %s at startup", audit, line)
			}
		}
		if slices.ContainsFunc(before, func(s string) bool {
			return strings.Contains(s, `rule="watch"`) && strings.HasPrefix(s, "tessera_decisions_total")
		}) {
			t.Errorf("a rule in audit mode has a series of tessera_decisions_total")
		}
		for _, d := range []engine.Decision{
			{Action: engine.Deny, Enforced: enforced == "true", Rule: "blocked", Reason: engine.ReasonError, Cause: expr.CauseClientIPMissing, Errors: []engine.RuleError{{Rule: "blocked", Cause: expr.CauseClientIPMissing}}},
			{Action: engine.Allow, Enforced: true, Rule: "office", Reason: engine.ReasonRule, AuditRules: []engine.AuditMatch{{Rule: "watch", Action: engine.Deny}}, Errors: []engine.RuleError{{Rule: "watch", Cause: expr.CauseExpression}}},
			{Action: engine.Deny, Enforced: enforced == "true", Rule: "default", Reason: engine.ReasonDefault, IndeterminateRules: []string{"office"}, Errors: []engine.RuleError{{Rule: "office", Cause: expr.CausePanic}}},
			{Action: engine.Deny, Enforced: enforced == "true", Rule: "blocked", Reason: engine.ReasonError, Cause: expr.CauseDatabaseError, Errors: []engine.RuleError{{Rule: "blocked", Cause: expr.CauseDatabaseError}}},
		} {
			m.Decided(d)
		}
		m.DecisionLogDropped()
		if after := series(t, m); !slices.Equal(before, after) {
			t.Errorf("audit %t: decisions changed the series:\n%s\nwant:\n%s", audit, strings.Join(after, "\n"), strings.Join(before, "\n"))
		}
		f := gather(t, m)
		value := func(name string, labels map[string]string) float64 {
			for _, metric := range f[name].GetMetric() {
				if got := metricLabels(metric); maps.Equal(got, labels) {
					switch {
					case metric.GetCounter() != nil:
						return metric.GetCounter().GetValue()
					case metric.GetGauge() != nil:
						return metric.GetGauge().GetValue()
					case metric.GetHistogram() != nil:
						return float64(metric.GetHistogram().GetSampleCount())
					}
				}
			}
			t.Fatalf("%s %v: no series", name, labels)
			return 0
		}
		for _, tc := range []struct {
			name   string
			labels map[string]string
			want   float64
		}{
			{"tessera_decisions_total", map[string]string{"action": "deny", "enforced": enforced, "rule": "blocked", "reason": "error"}, 2},
			{"tessera_decisions_total", map[string]string{"action": "allow", "enforced": "true", "rule": "office", "reason": "rule"}, 1},
			{"tessera_decisions_total", map[string]string{"action": "deny", "enforced": enforced, "rule": "default", "reason": "default"}, 1},
			{"tessera_decision_duration_seconds", map[string]string{"action": "deny"}, 3},
			{"tessera_audit_matches_total", map[string]string{"rule": "watch"}, 1},
			{"tessera_rule_evaluation_errors_total", map[string]string{"rule": "blocked", "cause": "client_ip_missing"}, 1},
			{"tessera_rule_evaluation_errors_total", map[string]string{"rule": "watch", "cause": "expression"}, 1},
			{"tessera_rule_evaluation_errors_total", map[string]string{"rule": "office", "cause": "panic"}, 1},
			{"tessera_rule_evaluation_errors_total", map[string]string{"rule": "blocked", "cause": "database_error"}, 1},
			{"tessera_decision_logs_dropped_total", map[string]string{}, 1},
			{"tessera_policy_audit_mode", map[string]string{}, map[bool]float64{false: 0, true: 1}[audit]},
		} {
			if got := value(tc.name, tc.labels); got != tc.want {
				t.Errorf("audit %t: %s %v = %v, want %v", audit, tc.name, tc.labels, got, tc.want)
			}
		}
	}
}

func metricLabels(m *dto.Metric) map[string]string { return labels(m) }

// value returns the value of the series of name with labels: a counter, a
// gauge, or the count of a histogram.
func value(t *testing.T, m *Metrics, name string, labels map[string]string) float64 {
	t.Helper()
	for _, metric := range gather(t, m)[name].GetMetric() {
		if got := metricLabels(metric); maps.Equal(got, labels) {
			switch {
			case metric.GetCounter() != nil:
				return metric.GetCounter().GetValue()
			case metric.GetGauge() != nil:
				return metric.GetGauge().GetValue()
			case metric.GetHistogram() != nil:
				return float64(metric.GetHistogram().GetSampleCount())
			}
		}
	}
	t.Fatalf("%s %v: no series", name, labels)
	return 0
}

// Every declared set has its series of entries and availability from the
// start, labelled with its source; a set that refreshes also has its series
// of refreshes, with every result of tessera_set_refreshes_total, and a set
// of values has none. Recording a check adds no series.
func TestSetSeries(t *testing.T) {
	m := New(testBuild, testHash, testPolicy, testSets, testDatabases, useragent.New(100))
	before := series(t, m)
	for _, d := range testSets {
		labels := `set="` + d.Name + `",source="` + string(d.Source) + `",type="` + string(d.Type) + `"`
		for _, line := range []string{
			`tessera_set_entries{` + labels + `}`,
			`tessera_set_available{` + labels + `}`,
		} {
			if !slices.Contains(before, line) {
				t.Errorf("no series %s at startup", line)
			}
		}
		refreshes := []string{
			`tessera_set_last_refresh_success_timestamp_seconds{` + labels + `}`,
			`tessera_set_refreshes_total{result="updated",` + labels + `}`,
			`tessera_set_refreshes_total{result="unchanged",` + labels + `}`,
			`tessera_set_refreshes_total{result="rejected",` + labels + `}`,
			`tessera_set_refreshes_total{result="error",` + labels + `}`,
			`tessera_set_refresh_duration_seconds_bucket{` + labels + `,le="0.0001"}`,
			`tessera_set_refresh_duration_seconds_count{` + labels + `}`,
		}
		for _, line := range refreshes {
			if got := slices.Contains(before, line); got != d.Source.Refreshes() {
				t.Errorf("series %s at startup: %t, want %t", line, got, d.Source.Refreshes())
			}
		}
	}

	at := time.Date(2026, 11, 22, 10, 15, 0, 500_000_000, time.UTC)
	m.SetRefreshed("blocked", sets.RefreshUpdated, 2*time.Millisecond, at)
	m.SetEntries("blocked", 3)
	m.SetAvailable("blocked", true)
	m.SetRefreshed("blocked", sets.RefreshUnchanged, 50*time.Microsecond, at.Add(10*time.Second))
	// A failed check does not move the time of the last success.
	m.SetRefreshed("blocked", sets.RefreshRejected, time.Millisecond, at.Add(20*time.Second))
	m.SetRefreshed("blocked", sets.RefreshError, time.Millisecond, at.Add(30*time.Second))
	m.SetAvailable("blocked", false)
	// A set of values has no refresh to record.
	m.SetRefreshed("networks", sets.RefreshUpdated, time.Millisecond, at)
	m.SetEntries("networks", 2)
	m.SetAvailable("networks", true)
	if after := series(t, m); !slices.Equal(before, after) {
		t.Errorf("recording changed the series:\n%s\nwant:\n%s", strings.Join(after, "\n"), strings.Join(before, "\n"))
	}
	blocked := map[string]string{"set": "blocked", "type": "cidr", "source": "file"}
	result := func(r string) map[string]string {
		return map[string]string{"set": "blocked", "type": "cidr", "source": "file", "result": r}
	}
	networks := map[string]string{"set": "networks", "type": "asn", "source": "values"}
	for _, tc := range []struct {
		name   string
		labels map[string]string
		want   float64
	}{
		{"tessera_set_entries", blocked, 3},
		{"tessera_set_available", blocked, 0},
		{"tessera_set_last_refresh_success_timestamp_seconds", blocked, float64(at.Add(10*time.Second).UnixNano()) / 1e9},
		{"tessera_set_refreshes_total", result("updated"), 1},
		{"tessera_set_refreshes_total", result("unchanged"), 1},
		{"tessera_set_refreshes_total", result("rejected"), 1},
		{"tessera_set_refreshes_total", result("error"), 1},
		{"tessera_set_refresh_duration_seconds", blocked, 4},
		{"tessera_set_refreshes_total", map[string]string{"set": "partners", "type": "string", "source": "file", "result": "updated"}, 0},
		{"tessera_set_entries", networks, 2},
		{"tessera_set_available", networks, 1},
	} {
		if got := value(t, m, tc.name, tc.labels); got != tc.want {
			t.Errorf("%s %v = %v, want %v", tc.name, tc.labels, got, tc.want)
		}
	}
	// The unchanged check, of 50 µs, is in the first bucket.
	for _, metric := range gather(t, m)["tessera_set_refresh_duration_seconds"].GetMetric() {
		if metricLabels(metric)["set"] == "blocked" {
			if b := metric.GetHistogram().GetBucket()[0]; b.GetUpperBound() != 0.0001 || b.GetCumulativeCount() != 1 {
				t.Errorf("first bucket %v", b)
			}
		}
	}
}

// Every configured database has its series from the start, with every result
// of tessera_enrichment_database_refreshes_total; a database that is not
// configured has none. Recording a check adds no series.
func TestDatabaseSeries(t *testing.T) {
	m := New(testBuild, testHash, testPolicy, testSets, testDatabases, useragent.New(100))
	before := series(t, m)
	for _, line := range []string{
		`tessera_enrichment_database_build_timestamp_seconds{database="geo"}`,
		`tessera_enrichment_database_last_refresh_success_timestamp_seconds{database="geo"}`,
		`tessera_enrichment_database_refreshes_total{database="geo",result="updated"}`,
		`tessera_enrichment_database_refreshes_total{database="geo",result="unchanged"}`,
		`tessera_enrichment_database_refreshes_total{database="geo",result="rejected"}`,
		`tessera_enrichment_database_refreshes_total{database="geo",result="error"}`,
		`tessera_enrichment_database_refresh_duration_seconds_bucket{database="geo",le="0.0001"}`,
		`tessera_enrichment_database_refresh_duration_seconds_count{database="geo"}`,
	} {
		if !slices.Contains(before, line) {
			t.Errorf("no series %s at startup", line)
		}
	}
	if slices.ContainsFunc(before, func(s string) bool { return strings.Contains(s, `database="asn"`) }) {
		t.Error("a series of a database that is not configured")
	}

	at := time.Date(2026, 11, 22, 10, 15, 0, 500_000_000, time.UTC)
	built := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	m.DatabaseRefreshed(enrichment.Geo, sets.RefreshUpdated, 50*time.Millisecond, at)
	m.DatabaseBuilt(enrichment.Geo, built)
	m.DatabaseRefreshed(enrichment.Geo, sets.RefreshUnchanged, 50*time.Microsecond, at.Add(time.Minute))
	// A failed check does not move the time of the last success.
	m.DatabaseRefreshed(enrichment.Geo, sets.RefreshRejected, time.Millisecond, at.Add(2*time.Minute))
	m.DatabaseRefreshed(enrichment.Geo, sets.RefreshError, time.Millisecond, at.Add(3*time.Minute))
	// A database that is not configured records nothing.
	m.DatabaseRefreshed(enrichment.ASN, sets.RefreshUpdated, time.Millisecond, at)
	m.DatabaseBuilt(enrichment.ASN, built)
	if after := series(t, m); !slices.Equal(before, after) {
		t.Errorf("recording changed the series:\n%s\nwant:\n%s", strings.Join(after, "\n"), strings.Join(before, "\n"))
	}
	geo := map[string]string{"database": "geo"}
	result := func(r string) map[string]string { return map[string]string{"database": "geo", "result": r} }
	for _, tc := range []struct {
		name   string
		labels map[string]string
		want   float64
	}{
		{"tessera_enrichment_database_build_timestamp_seconds", geo, float64(built.Unix())},
		{"tessera_enrichment_database_last_refresh_success_timestamp_seconds", geo, float64(at.Add(time.Minute).UnixNano()) / 1e9},
		{"tessera_enrichment_database_refreshes_total", result("updated"), 1},
		{"tessera_enrichment_database_refreshes_total", result("unchanged"), 1},
		{"tessera_enrichment_database_refreshes_total", result("rejected"), 1},
		{"tessera_enrichment_database_refreshes_total", result("error"), 1},
		{"tessera_enrichment_database_refresh_duration_seconds", geo, 4},
	} {
		if got := value(t, m, tc.name, tc.labels); got != tc.want {
			t.Errorf("%s %v = %v, want %v", tc.name, tc.labels, got, tc.want)
		}
	}
}

// Every upstream header has a series for each cause from the start; a
// header removed because its expression failed or gave an invalid value
// counts, and one removed by an empty value does not.
func TestUpstreamHeaderSeries(t *testing.T) {
	m := New(testBuild, testHash, testPolicy, testSets, testDatabases, useragent.New(100))
	before := series(t, m)
	var headerSeries []string
	for _, line := range before {
		if strings.HasPrefix(line, "tessera_upstream_header_errors_total") {
			headerSeries = append(headerSeries, line)
		}
	}
	var want []string
	for _, header := range []string{"x-client-asn", "x-client-country"} {
		for _, cause := range expr.Causes {
			want = append(want, `tessera_upstream_header_errors_total{cause="`+string(cause)+`",header="`+header+`"}`)
		}
	}
	slices.Sort(headerSeries)
	slices.Sort(want)
	if !slices.Equal(headerSeries, want) {
		t.Errorf("series at startup:\n%s\nwant:\n%s", strings.Join(headerSeries, "\n"), strings.Join(want, "\n"))
	}
	for _, d := range []engine.Decision{
		{Action: engine.Allow, Enforced: true, Rule: "office", Reason: engine.ReasonRule, Upstream: []engine.UpstreamHeader{
			{Name: "x-client-asn", Value: "64496"},
			{Name: "x-client-country", Cause: expr.CauseDatabaseError},
		}},
		{Action: engine.Allow, Enforced: true, Rule: "office", Reason: engine.ReasonRule, Upstream: []engine.UpstreamHeader{
			{Name: "x-client-asn", Cause: expr.CauseExpression},
			{Name: "x-client-country", Cause: expr.CauseDatabaseError},
		}},
		{Action: engine.Allow, Enforced: true, Rule: "office", Reason: engine.ReasonRule, Upstream: []engine.UpstreamHeader{
			{Name: "x-client-asn"},
			{Name: "x-client-country", Cause: expr.CauseClientIPMissing},
		}},
	} {
		m.Decided(d)
	}
	if after := series(t, m); !slices.Equal(before, after) {
		t.Errorf("decisions changed the series")
	}
	for _, tc := range []struct {
		header, cause string
		want          float64
	}{
		{"x-client-country", "database_error", 2},
		{"x-client-country", "client_ip_missing", 1},
		{"x-client-asn", "expression", 1},
		{"x-client-asn", "client_ip_missing", 0},
		{"x-client-asn", "panic", 0},
	} {
		if got := value(t, m, "tessera_upstream_header_errors_total", map[string]string{"header": tc.header, "cause": tc.cause}); got != tc.want {
			t.Errorf("%s %s = %v, want %v", tc.header, tc.cause, got, tc.want)
		}
	}
	// A policy without upstream headers has no series.
	plain := testPolicy
	plain.UpstreamHeaders = nil
	if slices.ContainsFunc(series(t, New(testBuild, testHash, plain, testSets, testDatabases, useragent.New(100))), func(s string) bool {
		return strings.HasPrefix(s, "tessera_upstream_header_errors_total")
	}) {
		t.Error("series without upstream headers")
	}
}

// The cache of the user-agent analysis has its series from the start, and a
// scrape reads its lookups and its entries; an analysis without a cache, and
// a configuration without the analysis, have none.
func TestUserAgentCacheSeries(t *testing.T) {
	ua := useragent.New(100)
	m := New(testBuild, testHash, testPolicy, testSets, testDatabases, ua)
	want := []string{
		`tessera_enrichment_user_agent_cache_entries`,
		`tessera_enrichment_user_agent_cache_lookups_total{result="hit"}`,
		`tessera_enrichment_user_agent_cache_lookups_total{result="miss"}`,
	}
	got := func(m *Metrics) []string {
		var out []string
		for _, line := range series(t, m) {
			if strings.HasPrefix(line, "tessera_enrichment_user_agent_") {
				out = append(out, line)
			}
		}
		slices.Sort(out)
		return out
	}
	if s := got(m); !slices.Equal(s, want) {
		t.Fatalf("series at startup:\n%s\nwant:\n%s", strings.Join(s, "\n"), strings.Join(want, "\n"))
	}
	for _, h := range []string{"curl/8.7.1", "curl/8.7.1", "curl/8.7.1", "sqlmap/1.8", ""} {
		ua.Analyze(h)
	}
	for _, tc := range []struct {
		name   string
		labels map[string]string
		want   float64
	}{
		{"tessera_enrichment_user_agent_cache_lookups_total", map[string]string{"result": "hit"}, 2},
		{"tessera_enrichment_user_agent_cache_lookups_total", map[string]string{"result": "miss"}, 2},
		{"tessera_enrichment_user_agent_cache_entries", map[string]string{}, 2},
	} {
		if v := value(t, m, tc.name, tc.labels); v != tc.want {
			t.Errorf("%s %v = %v, want %v", tc.name, tc.labels, v, tc.want)
		}
	}
	for _, none := range []*useragent.Analyzer{useragent.New(0), nil} {
		if s := got(New(testBuild, testHash, testPolicy, testSets, testDatabases, none)); len(s) != 0 {
			t.Errorf("series without a cache: %v", s)
		}
	}
}
