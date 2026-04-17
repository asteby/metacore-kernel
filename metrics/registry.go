// Package metrics exposes a Prometheus-compatible registry with standard
// metacore counters / histograms and Fiber integration.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Registry bundles the Prometheus registry with all pre-registered metrics.
type Registry struct {
	Prometheus *prometheus.Registry

	// HTTP
	HTTPRequestsTotal   *prometheus.CounterVec
	HTTPRequestDuration *prometheus.HistogramVec

	// WebSocket
	WSConnections prometheus.Gauge

	// Webhooks
	WebhookDeliveries *prometheus.CounterVec

	// Push notifications
	PushSends *prometheus.CounterVec
}

// NewRegistry creates and registers all standard metacore metrics in a fresh
// (non-global) Prometheus registry. Go runtime collectors are included.
func NewRegistry() *Registry {
	reg := prometheus.NewRegistry()

	// Go runtime + process metrics.
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	httpTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "Total number of HTTP requests processed, partitioned by method, path, and status.",
	}, []string{"method", "path", "status"})

	httpDuration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Help:    "HTTP request latency in seconds, partitioned by method and path.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "path"})

	wsConns := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "ws_connections",
		Help: "Current number of active WebSocket connections.",
	})

	webhookDeliveries := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "webhook_deliveries_total",
		Help: "Total webhook deliveries attempted, partitioned by status (success|failure).",
	}, []string{"status"})

	pushSends := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "push_sends_total",
		Help: "Total push notifications sent, partitioned by status (success|failure).",
	}, []string{"status"})

	reg.MustRegister(httpTotal, httpDuration, wsConns, webhookDeliveries, pushSends)

	return &Registry{
		Prometheus:          reg,
		HTTPRequestsTotal:   httpTotal,
		HTTPRequestDuration: httpDuration,
		WSConnections:       wsConns,
		WebhookDeliveries:   webhookDeliveries,
		PushSends:           pushSends,
	}
}
