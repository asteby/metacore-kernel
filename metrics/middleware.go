// Package metrics exposes a Prometheus-compatible registry with standard
// metacore counters/histograms. Both Fiber and net/http middlewares are
// provided and can coexist in the same binary.
package metrics

import (
	"fmt"
	"net/http"
	"time"

	"github.com/gofiber/fiber/v2"
)

// statusRecorder wraps http.ResponseWriter to capture the written status code.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// HTTPMiddleware returns a net/http middleware that increments request counters
// and observes latency histograms in the provided Registry.
func HTTPMiddleware(reg *Registry) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)

			duration := time.Since(start).Seconds()
			status := fmt.Sprintf("%d", rec.status)
			method := r.Method
			path := r.URL.Path

			reg.HTTPRequestsTotal.WithLabelValues(method, path, status).Inc()
			reg.HTTPRequestDuration.WithLabelValues(method, path).Observe(duration)
		})
	}
}

// FiberMiddleware returns a Fiber middleware that records HTTP request counts
// and latency into the provided Registry.
func FiberMiddleware(reg *Registry) fiber.Handler {
	return func(c *fiber.Ctx) error {
		start := time.Now()

		err := c.Next()

		duration := time.Since(start).Seconds()
		status := fmt.Sprintf("%d", c.Response().StatusCode())
		method := c.Method()
		path := c.Path()

		reg.HTTPRequestsTotal.WithLabelValues(method, path, status).Inc()
		reg.HTTPRequestDuration.WithLabelValues(method, path).Observe(duration)

		return err
	}
}
