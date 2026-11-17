package navcounts

import (
	"time"

	"github.com/gofiber/fiber/v3"
	"gorm.io/gorm"
)

// Caller is what the host resolves from the request: the organization and a
// permission predicate that applies the dynamic role×module×action matrix.
type Caller struct {
	OrgID   string
	Allowed func(permission string) bool
}

// Handler serves GET /api/nav/counts with the kernel envelope
// {success,data,meta}. resolve must authenticate and return the Caller;
// returning an error yields 401.
func Handler(r *Registry, db *gorm.DB, resolve func(c fiber.Ctx) (Caller, error)) fiber.Handler {
	return func(c fiber.Ctx) error {
		caller, err := resolve(c)
		if err != nil {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"success": false, "error": "unauthorized"})
		}
		vals := r.Compute(c.Context(), db, caller.OrgID, caller.Allowed, time.Now())
		return c.JSON(fiber.Map{"success": true, "data": vals, "meta": fiber.Map{"total": len(vals)}})
	}
}
