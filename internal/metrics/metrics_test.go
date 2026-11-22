package metrics

import (
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"

	"github.com/gtriggiano/tessera/internal/engine"
	"github.com/gtriggiano/tessera/internal/expr"
)

var testBuild = BuildInfo{Version: "1.2.3", Revision: "abc123", GoVersion: "go1.test"}

const testHash = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"

// testPolicy has a rule of each kind: allow, deny, and audit.
var testPolicy = engine.Policy{DefaultAction: engine.Deny, Rules: []engine.Rule{
	{Name: "blocked", Action: engine.Deny},
	{Name: "watch", Action: engine.Deny, Audit: true},
	{Name: "office", Action: engine.Allow},
}}

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
	families := gather(t, New(testBuild, testHash, testPolicy))
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
	m := New(testBuild, testHash, testPolicy)
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
	New(testBuild, testHash, testPolicy).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
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
		m := New(testBuild, testHash, p)
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
			{"tessera_decisions_total", map[string]string{"action": "deny", "enforced": enforced, "rule": "blocked", "reason": "error"}, 1},
			{"tessera_decisions_total", map[string]string{"action": "allow", "enforced": "true", "rule": "office", "reason": "rule"}, 1},
			{"tessera_decisions_total", map[string]string{"action": "deny", "enforced": enforced, "rule": "default", "reason": "default"}, 1},
			{"tessera_decision_duration_seconds", map[string]string{"action": "deny"}, 2},
			{"tessera_audit_matches_total", map[string]string{"rule": "watch"}, 1},
			{"tessera_rule_evaluation_errors_total", map[string]string{"rule": "blocked", "cause": "client_ip_missing"}, 1},
			{"tessera_rule_evaluation_errors_total", map[string]string{"rule": "watch", "cause": "expression"}, 1},
			{"tessera_rule_evaluation_errors_total", map[string]string{"rule": "office", "cause": "panic"}, 1},
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
