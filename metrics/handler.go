package metrics

import (
	"github.com/gofiber/fiber/v2"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/valyala/fasthttp/fasthttpadaptor"
)

// Handler returns a Fiber handler that exposes the registry metrics at the
// standard /metrics path in Prometheus text exposition format.
func Handler(reg *Registry) fiber.Handler {
	h := promhttp.HandlerFor(reg.Prometheus, promhttp.HandlerOpts{})
	adapted := fasthttpadaptor.NewFastHTTPHandler(h)
	return func(c *fiber.Ctx) error {
		adapted(c.Context())
		return nil
	}
}
