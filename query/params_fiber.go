package query

import (
	"net/url"

	"github.com/gofiber/fiber/v3"
)

// ParseFiber extracts Params from a Fiber request context. It is a thin
// wrapper over ParseFromMap; prefer the map variant from non-Fiber
// transports (Echo, gRPC, CLI).
//
// Per ARCHITECTURE.md Law 3, this file is the ONLY place in the package
// that imports Fiber. builder.go, params.go, filter.go, and errors.go
// MUST remain Fiber-free.
func ParseFiber(c fiber.Ctx) (Params, error) {
	raw := c.Request().URI().QueryString()
	values, err := url.ParseQuery(string(raw))
	if err != nil {
		return Params{}, err
	}
	return ParseFromMap(values)
}
