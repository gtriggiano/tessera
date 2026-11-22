// Package metrics exports tessera's Prometheus metrics from a private
// registry, which the admin listener serves at /metrics.
//
// Metric names, types and labels are a public interface: the metrics
// reference documents each family, and a test keeps the two equal.
package metrics

import (
	"net/http"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/gtriggiano/tessera/internal/engine"
	"github.com/gtriggiano/tessera/internal/expr"
)

// DurationBuckets are the upper bounds, in seconds, of the buckets of
// tessera_decision_duration_seconds.
var DurationBuckets = []float64{0.000025, 0.00005, 0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1}

// causes are the values of the label cause.
var causes = []expr.Cause{expr.CauseClientIPMissing, expr.CauseExpression, expr.CausePanic}

const namespace = "tessera"

// BuildInfo identifies the binary, as tessera version prints it.
type BuildInfo struct {
	Version, Revision, GoVersion string
}

// Metrics owns the registry and every series in it.
type Metrics struct {
	registry    *prometheus.Registry
	handler     http.Handler
	refused     prometheus.Counter
	decisions   *prometheus.CounterVec
	duration    *prometheus.HistogramVec
	auditTotal  *prometheus.CounterVec
	errorsTotal *prometheus.CounterVec
	dropped     prometheus.Counter
}

// New returns the metrics of a process built as build, whose configuration
// has the hash configHash, the hexadecimal SHA-256 of the output of tessera
// config print, and the policy p. It registers the Go and process collectors
// too.
//
// Every series that p can produce exists from the start, at zero: label
// values come from p and fixed sets only, so the number of series depends on
// the configuration alone.
func New(build BuildInfo, configHash string, p engine.Policy) *Metrics {
	reg := prometheus.NewRegistry()
	buildInfo := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace:   namespace,
		Name:        "build_info",
		Help:        "Always 1, labelled with the build of the binary.",
		ConstLabels: prometheus.Labels{"version": build.Version, "revision": build.Revision, "go_version": build.GoVersion},
	})
	buildInfo.Set(1)
	configInfo := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace:   namespace,
		Name:        "config_info",
		Help:        "Always 1, labelled with the SHA-256 of the output of tessera config print for the configuration in use.",
		ConstLabels: prometheus.Labels{"hash": configHash},
	})
	configInfo.Set(1)
	refused := prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "grpc_connections_refused_total",
		Help:      "Connections to the gRPC listener closed as soon as accepted, because server.max_connections were open.",
	})
	m := &Metrics{
		registry: reg,
		refused:  refused,
		decisions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "decisions_total",
			Help:      "Decisions, by action, whether the action was enforced, the rule that decided (or default) and the reason.",
		}, []string{"action", "enforced", "rule", "reason"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "decision_duration_seconds",
			Help:      "The time of a decision, by action.",
			Buckets:   DurationBuckets,
		}, []string{"action"}),
		auditTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "audit_matches_total",
			Help:      "Requests for which the condition of a rule in audit mode was true, by rule.",
		}, []string{"rule"}),
		errorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "rule_evaluation_errors_total",
			Help:      "Conditions of rules that could not be evaluated, by rule and cause.",
		}, []string{"rule", "cause"}),
		dropped: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "decision_logs_dropped_total",
			Help:      "Decision events dropped because their queue was full, the output closed or a write failed.",
		}),
	}
	auditMode := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "policy_audit_mode",
		Help:      "1 when policy.mode is audit, 0 otherwise.",
	})
	if p.Audit {
		auditMode.Set(1)
	}
	enforced := func(a engine.Action) string { return strconv.FormatBool(!(p.Audit && a == engine.Deny)) }
	for _, r := range p.Rules {
		for _, c := range causes {
			m.errorsTotal.WithLabelValues(r.Name, string(c))
		}
		if r.Audit {
			m.auditTotal.WithLabelValues(r.Name)
			continue
		}
		m.decisions.WithLabelValues(string(r.Action), enforced(r.Action), r.Name, string(engine.ReasonRule))
		if r.Action == engine.Deny {
			m.decisions.WithLabelValues(string(engine.Deny), enforced(engine.Deny), r.Name, string(engine.ReasonError))
		}
	}
	m.decisions.WithLabelValues(string(p.DefaultAction), enforced(p.DefaultAction), engine.DefaultRule, string(engine.ReasonDefault))
	for _, a := range []engine.Action{engine.Allow, engine.Deny} {
		m.duration.WithLabelValues(string(a))
	}
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		buildInfo,
		configInfo,
		refused,
		auditMode,
		m.decisions,
		m.duration,
		m.auditTotal,
		m.errorsTotal,
		m.dropped,
	)
	m.handler = promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
	return m
}

// Decided records the decision d.
func (m *Metrics) Decided(d engine.Decision) {
	m.decisions.WithLabelValues(string(d.Action), strconv.FormatBool(d.Enforced), d.Rule, string(d.Reason)).Inc()
	m.duration.WithLabelValues(string(d.Action)).Observe(d.Duration.Seconds())
	for _, a := range d.AuditRules {
		m.auditTotal.WithLabelValues(a.Rule).Inc()
	}
	for _, e := range d.Errors {
		m.errorsTotal.WithLabelValues(e.Rule, string(e.Cause)).Inc()
	}
}

// DecisionLogDropped counts a decision event dropped.
func (m *Metrics) DecisionLogDropped() { m.dropped.Inc() }

// ConnectionRefused counts a connection to the gRPC listener refused because
// server.max_connections were open.
func (m *Metrics) ConnectionRefused() { m.refused.Inc() }

// Handler serves the registry in the Prometheus exposition format.
func (m *Metrics) Handler() http.Handler { return m.handler }
