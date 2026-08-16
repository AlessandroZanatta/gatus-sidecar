package controller

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Metrics exposes what an operator needs to alert on: whether the sidecar is
// still rendering, and whether it is quietly dropping endpoints.
type Metrics struct {
	endpoints    prometheus.Gauge
	sources      prometheus.Gauge
	warnings     prometheus.Gauge
	renderErrors prometheus.Counter
	lastRender   prometheus.Gauge
}

// NewMetrics registers the sidecar's metrics with the controller-runtime
// registry, which is served on the manager's metrics endpoint.
func NewMetrics() *Metrics {
	m := &Metrics{
		endpoints: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gatus_sidecar_endpoints",
			Help: "Number of endpoints in the most recently rendered configuration.",
		}),
		sources: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gatus_sidecar_sources",
			Help: "Number of Kubernetes objects currently contributing endpoints.",
		}),
		warnings: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gatus_sidecar_render_warnings",
			Help: "Endpoints dropped or templates skipped during the last render. Non-zero means part of the intended configuration is missing.",
		}),
		renderErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gatus_sidecar_render_errors_total",
			Help: "Renders that failed. The previously written configuration is left in place, so this going up means the file on disk is going stale.",
		}),
		lastRender: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gatus_sidecar_last_successful_render_timestamp_seconds",
			Help: "Unix timestamp of the last successful render.",
		}),
	}

	metrics.Registry.MustRegister(m.endpoints, m.sources, m.warnings, m.renderErrors, m.lastRender)
	return m
}

// recordRender is nil-safe so the render loop can run without metrics in tests.
func (m *Metrics) recordRender(endpoints, warnings int) {
	if m == nil {
		return
	}
	m.endpoints.Set(float64(endpoints))
	m.warnings.Set(float64(warnings))
	m.lastRender.Set(float64(time.Now().Unix()))
}

func (m *Metrics) recordSources(n int) {
	if m == nil {
		return
	}
	m.sources.Set(float64(n))
}

func (m *Metrics) recordError() {
	if m == nil {
		return
	}
	m.renderErrors.Inc()
}
