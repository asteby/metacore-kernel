package metrics_test

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/asteby/metacore-kernel/metrics"
)

func TestNewRegistry_NotNil(t *testing.T) {
	reg := metrics.NewRegistry()
	if reg == nil {
		t.Fatal("expected non-nil registry")
	}
	if reg.Prometheus == nil {
		t.Fatal("expected non-nil Prometheus registry")
	}
}

func TestHTTPRequestsTotal(t *testing.T) {
	reg := metrics.NewRegistry()
	reg.HTTPRequestsTotal.WithLabelValues("GET", "/health", "200").Inc()
	reg.HTTPRequestsTotal.WithLabelValues("POST", "/auth/login", "401").Inc()

	count, err := testutil.GatherAndCount(reg.Prometheus)
	if err != nil {
		t.Fatalf("gather error: %v", err)
	}
	if count == 0 {
		t.Fatal("expected at least one metric family")
	}
}

func TestHTTPRequestDuration(t *testing.T) {
	reg := metrics.NewRegistry()
	reg.HTTPRequestDuration.WithLabelValues("GET", "/api/users").Observe(0.042)
}

func TestWSConnections(t *testing.T) {
	reg := metrics.NewRegistry()
	reg.WSConnections.Set(5)
	reg.WSConnections.Inc()
	reg.WSConnections.Dec()
}

func TestWebhookDeliveries(t *testing.T) {
	reg := metrics.NewRegistry()
	reg.WebhookDeliveries.WithLabelValues("success").Add(3)
	reg.WebhookDeliveries.WithLabelValues("failure").Inc()
}

func TestPushSends(t *testing.T) {
	reg := metrics.NewRegistry()
	reg.PushSends.WithLabelValues("success").Add(10)
	reg.PushSends.WithLabelValues("failure").Inc()
}

func TestMetricsOutput_ContainsExpectedNames(t *testing.T) {
	reg := metrics.NewRegistry()
	// Trigger observations so the metrics appear in output.
	reg.HTTPRequestsTotal.WithLabelValues("GET", "/", "200").Inc()
	reg.HTTPRequestDuration.WithLabelValues("GET", "/").Observe(0.001)
	reg.WSConnections.Set(1)
	reg.WebhookDeliveries.WithLabelValues("success").Inc()
	reg.PushSends.WithLabelValues("success").Inc()

	out, err := testutil.GatherAndLint(reg.Prometheus)
	if err != nil {
		t.Fatalf("lint error: %v", err)
	}
	if len(out) > 0 {
		t.Errorf("lint issues: %v", out)
	}

	// Ensure expected metric names are present in gathered output.
	families, err := reg.Prometheus.Gather()
	if err != nil {
		t.Fatalf("gather error: %v", err)
	}
	names := make(map[string]bool)
	for _, f := range families {
		names[f.GetName()] = true
	}
	expected := []string{
		"http_requests_total",
		"http_request_duration_seconds",
		"ws_connections",
		"webhook_deliveries_total",
		"push_sends_total",
	}
	for _, name := range expected {
		found := false
		for n := range names {
			if strings.HasPrefix(n, name) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("metric %q not found in registry output", name)
		}
	}
}
