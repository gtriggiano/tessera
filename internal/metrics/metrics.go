// Package metrics exports tessera's Prometheus metrics from a private
// registry, which the admin listener serves at /metrics.
//
// Metric names, types and labels are a public interface: the metrics
// reference documents each family, and a test keeps the two equal.
package metrics

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/gtriggiano/tessera/internal/engine"
	"github.com/gtriggiano/tessera/internal/enrichment"
	"github.com/gtriggiano/tessera/internal/expr"
	"github.com/gtriggiano/tessera/internal/sets"
)

// DurationBuckets are the upper bounds, in seconds, of the buckets of
// tessera_decision_duration_seconds.
var DurationBuckets = []float64{0.000025, 0.00005, 0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1}

// RefreshBuckets are the upper bounds, in seconds, of the buckets of
// tessera_set_refresh_duration_seconds and
// tessera_enrichment_database_refresh_duration_seconds.
var RefreshBuckets = []float64{0.0001, 0.001, 0.01, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}

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
	headerErrs  *prometheus.CounterVec
	dropped     prometheus.Counter

	setLabels      map[string]setLabels // of each declared set
	setEntries     *prometheus.GaugeVec
	setAvailable   *prometheus.GaugeVec
	setLastSuccess *prometheus.GaugeVec
	setRefreshes   *prometheus.CounterVec
	setDuration    *prometheus.HistogramVec

	databases     map[enrichment.Kind]bool // the configured ones
	dbBuild       *prometheus.GaugeVec
	dbRefreshes   *prometheus.CounterVec
	dbLastSuccess *prometheus.GaugeVec
	dbDuration    *prometheus.HistogramVec
}

// New returns the metrics of a process built as build, whose configuration
// has the hash configHash, the hexadecimal SHA-256 of the output of tessera
// config print, the policy p, the sets declared and the databases of the
// kinds databases. It registers the Go and process collectors too.
//
// Every series that p, its upstream headers included, declared and
// databases can produce exists from the start, at zero: label values come
// from the configuration and fixed sets only, so the number of series
// depends on the configuration alone.
func New(build BuildInfo, configHash string, p engine.Policy, declared []sets.Declaration, databases []enrichment.Kind) *Metrics {
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
		headerErrs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "upstream_header_errors_total",
			Help:      "Upstream headers removed because their expression failed or gave a value that cannot be sent, by header and cause.",
		}, []string{"header", "cause"}),
		dropped: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "decision_logs_dropped_total",
			Help:      "Decision events dropped because their queue was full, the output closed or a write failed.",
		}),
		setLabels: map[string]setLabels{},
		setEntries: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "set_entries",
			Help:      "Entries of the copy of the set in use, by set, type and source.",
		}, []string{"set", "type", "source"}),
		setAvailable: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "set_available",
			Help:      "1 when the set is available, 0 when it is not, by set, type and source.",
		}, []string{"set", "type", "source"}),
		setLastSuccess: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "set_last_refresh_success_timestamp_seconds",
			Help:      "The Unix time of the last successful check of the source of a set that refreshes, unchanged or updated, by set, type and source.",
		}, []string{"set", "type", "source"}),
		setRefreshes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "set_refreshes_total",
			Help:      "Checks of the source of a set that refreshes, the load at startup included, by set, type, source and result.",
		}, []string{"set", "type", "source", "result"}),
		setDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "set_refresh_duration_seconds",
			Help:      "The time of a check of the source of a set that refreshes, the load at startup included, by set, type and source.",
			Buckets:   RefreshBuckets,
		}, []string{"set", "type", "source"}),
		databases: map[enrichment.Kind]bool{},
		dbBuild: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "enrichment_database_build_timestamp_seconds",
			Help:      "The build time of the database in use, from its metadata, in seconds since the Unix epoch, by database.",
		}, []string{"database"}),
		dbRefreshes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "enrichment_database_refreshes_total",
			Help:      "Checks of the file of a database, the load at startup included, by database and result.",
		}, []string{"database", "result"}),
		dbLastSuccess: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "enrichment_database_last_refresh_success_timestamp_seconds",
			Help:      "The Unix time of the last successful check of the file of a database, unchanged or updated, by database.",
		}, []string{"database"}),
		dbDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "enrichment_database_refresh_duration_seconds",
			Help:      "The time of a check of the file of a database, the load at startup included, by database.",
			Buckets:   RefreshBuckets,
		}, []string{"database"}),
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
		for _, c := range expr.Causes {
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
	for _, h := range p.UpstreamHeaders {
		for _, c := range expr.Causes {
			m.headerErrs.WithLabelValues(strings.ToLower(h.Name), string(c))
		}
	}
	for _, a := range []engine.Action{engine.Allow, engine.Deny} {
		m.duration.WithLabelValues(string(a))
	}
	for _, d := range declared {
		l := setLabels{typ: string(d.Type), source: string(d.Source), refreshes: d.Source.Refreshes()}
		m.setLabels[d.Name] = l
		m.setEntries.WithLabelValues(d.Name, l.typ, l.source)
		m.setAvailable.WithLabelValues(d.Name, l.typ, l.source)
		if !l.refreshes {
			continue
		}
		m.setLastSuccess.WithLabelValues(d.Name, l.typ, l.source)
		for _, r := range sets.RefreshResults {
			m.setRefreshes.WithLabelValues(d.Name, l.typ, l.source, string(r))
		}
		m.setDuration.WithLabelValues(d.Name, l.typ, l.source)
	}
	for _, k := range databases {
		m.databases[k] = true
		m.dbBuild.WithLabelValues(string(k))
		m.dbLastSuccess.WithLabelValues(string(k))
		m.dbDuration.WithLabelValues(string(k))
		for _, r := range sets.RefreshResults {
			m.dbRefreshes.WithLabelValues(string(k), string(r))
		}
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
		m.headerErrs,
		m.dropped,
		m.setEntries,
		m.setAvailable,
		m.setLastSuccess,
		m.setRefreshes,
		m.setDuration,
		m.dbBuild,
		m.dbRefreshes,
		m.dbLastSuccess,
		m.dbDuration,
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
	for _, h := range d.Upstream {
		if h.Cause != "" {
			m.headerErrs.WithLabelValues(h.Name, string(h.Cause)).Inc()
		}
	}
}

// DecisionLogDropped counts a decision event dropped.
func (m *Metrics) DecisionLogDropped() { m.dropped.Inc() }

// ConnectionRefused counts a connection to the gRPC listener refused because
// server.max_connections were open.
func (m *Metrics) ConnectionRefused() { m.refused.Inc() }

// setLabels are the labels of the series of a set, and whether it has the
// series of the refreshes.
type setLabels struct {
	typ, source string
	refreshes   bool
}

// SetRefreshed records a check of the source of the set name, or its load
// at startup: its result, how long it took, and when it ended, which is the
// time of the last successful check when the result is updated or
// unchanged. It records nothing for a set that does not refresh, such as a
// set of values.
func (m *Metrics) SetRefreshed(name string, r sets.RefreshResult, took time.Duration, at time.Time) {
	l := m.setLabels[name]
	if !l.refreshes {
		return
	}
	m.setRefreshes.WithLabelValues(name, l.typ, l.source, string(r)).Inc()
	m.setDuration.WithLabelValues(name, l.typ, l.source).Observe(took.Seconds())
	if r == sets.RefreshUpdated || r == sets.RefreshUnchanged {
		m.setLastSuccess.WithLabelValues(name, l.typ, l.source).Set(float64(at.UnixNano()) / 1e9)
	}
}

// SetEntries records the entries of the copy of the set name in use.
func (m *Metrics) SetEntries(name string, entries int) {
	l := m.setLabels[name]
	m.setEntries.WithLabelValues(name, l.typ, l.source).Set(float64(entries))
}

// SetAvailable records whether the set name is available.
func (m *Metrics) SetAvailable(name string, available bool) {
	v := 0.0
	if available {
		v = 1
	}
	l := m.setLabels[name]
	m.setAvailable.WithLabelValues(name, l.typ, l.source).Set(v)
}

// DatabaseRefreshed records a check of the file of the database of kind k,
// or its load at startup: its result, how long it took, and when it ended,
// which is the time of the last successful check when the result is updated
// or unchanged. It records nothing for a database that is not configured.
func (m *Metrics) DatabaseRefreshed(k enrichment.Kind, r sets.RefreshResult, took time.Duration, at time.Time) {
	if !m.databases[k] {
		return
	}
	m.dbRefreshes.WithLabelValues(string(k), string(r)).Inc()
	m.dbDuration.WithLabelValues(string(k)).Observe(took.Seconds())
	if r == sets.RefreshUpdated || r == sets.RefreshUnchanged {
		m.dbLastSuccess.WithLabelValues(string(k)).Set(float64(at.UnixNano()) / 1e9)
	}
}

// DatabaseBuilt records the build time of the copy in use of the database
// of kind k.
func (m *Metrics) DatabaseBuilt(k enrichment.Kind, built time.Time) {
	if !m.databases[k] {
		return
	}
	m.dbBuild.WithLabelValues(string(k)).Set(float64(built.Unix()))
}

// Handler serves the registry in the Prometheus exposition format.
func (m *Metrics) Handler() http.Handler { return m.handler }
