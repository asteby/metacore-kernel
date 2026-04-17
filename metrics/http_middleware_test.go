package metrics_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/asteby/metacore-kernel/metrics"
)

func TestHTTPMiddleware_IncrementsCounter(t *testing.T) {
	reg := metrics.NewRegistry()

	handler := metrics.HTTPMiddleware(reg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/users", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	count, err := testutil.GatherAndCount(reg.Prometheus)
	if err != nil {
		t.Fatalf("gather error: %v", err)
	}
	if count == 0 {
		t.Fatal("expected metrics to be recorded")
	}
}

func TestHTTPMiddleware_CapturesNonOKStatus(t *testing.T) {
	reg := metrics.NewRegistry()

	handler := metrics.HTTPMiddleware(reg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))

	req := httptest.NewRequest(http.MethodPost, "/auth/login", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	families, err := reg.Prometheus.Gather()
	if err != nil {
		t.Fatalf("gather error: %v", err)
	}

	found := false
	for _, f := range families {
		if f.GetName() == "http_requests_total" {
			for _, m := range f.GetMetric() {
				for _, lp := range m.GetLabel() {
					if lp.GetName() == "status" && lp.GetValue() == "401" {
						found = true
					}
				}
			}
		}
	}
	if !found {
		t.Error("expected http_requests_total with status=401 label")
	}
}

func TestHTTPMiddleware_ObservesHistogram(t *testing.T) {
	reg := metrics.NewRegistry()

	handler := metrics.HTTPMiddleware(reg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/slow", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
	}

	families, err := reg.Prometheus.Gather()
	if err != nil {
		t.Fatalf("gather error: %v", err)
	}

	found := false
	for _, f := range families {
		if f.GetName() == "http_request_duration_seconds" {
			for _, m := range f.GetMetric() {
				if m.GetHistogram().GetSampleCount() == 3 {
					found = true
				}
			}
		}
	}
	if !found {
		t.Error("expected http_request_duration_seconds with 3 observations")
	}
}
