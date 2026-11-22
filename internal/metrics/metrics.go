// Package metrics exports tessera's Prometheus metrics from a private
// registry, which the admin listener serves at /metrics.
//
// Metric names, types and labels are a public interface: the metrics
// reference documents each family, and a test keeps the two equal.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const namespace = "tessera"

// BuildInfo identifies the binary, as tessera version prints it.
type BuildInfo struct {
	Version, Revision, GoVersion string
}

// Metrics owns the registry and every series in it.
type Metrics struct {
	registry *prometheus.Registry
	handler  http.Handler
	refused  prometheus.Counter
}

// New returns the metrics of a process built as build, whose configuration
// has the hash configHash: the hexadecimal SHA-256 of the output of tessera
// config print. It registers the Go and process collectors too.
func New(build BuildInfo, configHash string) *Metrics {
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
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		buildInfo,
		configInfo,
		refused,
	)
	return &Metrics{registry: reg, handler: promhttp.HandlerFor(reg, promhttp.HandlerOpts{}), refused: refused}
}

// ConnectionRefused counts a connection to the gRPC listener refused because
// server.max_connections were open.
func (m *Metrics) ConnectionRefused() { m.refused.Inc() }

// Handler serves the registry in the Prometheus exposition format.
func (m *Metrics) Handler() http.Handler { return m.handler }
