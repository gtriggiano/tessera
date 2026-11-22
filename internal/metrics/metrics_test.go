package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"
)

var testBuild = BuildInfo{Version: "1.2.3", Revision: "abc123", GoVersion: "go1.test"}

const testHash = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"

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
	families := gather(t, New(testBuild, testHash))
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
	m := New(testBuild, testHash)
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
	New(testBuild, testHash).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
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
