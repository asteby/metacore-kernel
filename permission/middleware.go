package permission

import (
	"errors"

	"github.com/asteby/metacore-kernel/modelbase"
	"github.com/gofiber/fiber/v2"
)

// UserLookup resolves the authenticated user from a Fiber context. Apps
// typically implement it by reading claims that auth.Middleware stashed in
// c.Locals and loading the user row — or by materialising a lightweight
// AuthUser straight from those claims without a DB roundtrip.
//
// Returning nil means "no authenticated user"; the middleware maps that to
// 401.
type UserLookup func(*fiber.Ctx) modelbase.AuthUser

// GateConfig tunes the Fiber gate. Zero values use sensible defaults; most
// callers just pass nil and rely on the standard JSON error shape.
type GateConfig struct {
	// Mode controls how multiple caps are combined. Zero value = ModeAll
	// (every listed capability is required), which is the safest default.
	Mode CheckMode

	// OnDenied lets apps customise the 403 body. Defaults to the standard
	// {"success": false, "message": "Permission denied"} shape.
	OnDenied func(c *fiber.Ctx, err error) error

	// OnUnauthenticated lets apps customise the 401 body (fires when the
	// UserLookup returns nil). Defaults to {"success": false,
	// "message": "Unauthorized"}.
	OnUnauthenticated func(c *fiber.Ctx, err error) error
}

// CheckMode enumerates the ways a gate combines multiple capabilities.
type CheckMode int

const (
	// ModeAll requires the user to hold every listed capability (default).
	ModeAll CheckMode = iota
	// ModeAny requires at least one.
	ModeAny
)

// Gate builds a Fiber middleware that:
//
//  1. resolves the AuthUser via userLookup,
//  2. runs Check / CheckAll / CheckAny against service,
//  3. on success calls c.Next(), otherwise short-circuits with 401 or 403.
//
// Must be chained AFTER auth.Middleware so the user id/role locals are
// already populated.
func Gate(service *Service, userLookup UserLookup, caps ...Capability) fiber.Handler {
	return gateWith(service, userLookup, GateConfig{}, caps...)
}

// GateWith is the configurable form of Gate (lets apps flip Any/All or inject
// custom error responders).
func GateWith(service *Service, userLookup UserLookup, cfg GateConfig, caps ...Capability) fiber.Handler {
	return gateWith(service, userLookup, cfg, caps...)
}

// Gate is an ergonomic method form of the top-level Gate function so apps
// can write svc.Gate(lookup, cap) without repeating the service variable.
func (s *Service) Gate(userLookup UserLookup, caps ...Capability) fiber.Handler {
	return gateWith(s, userLookup, GateConfig{}, caps...)
}

// GateWith is the method form of the top-level GateWith.
func (s *Service) GateWith(userLookup UserLookup, cfg GateConfig, caps ...Capability) fiber.Handler {
	return gateWith(s, userLookup, cfg, caps...)
}

func gateWith(service *Service, userLookup UserLookup, cfg GateConfig, caps ...Capability) fiber.Handler {
	if service == nil {
		panic("permission: nil *Service passed to Gate")
	}
	if userLookup == nil {
		panic("permission: nil UserLookup passed to Gate")
	}

	onDenied := cfg.OnDenied
	if onDenied == nil {
		onDenied = defaultOnDenied
	}
	onUnauth := cfg.OnUnauthenticated
	if onUnauth == nil {
		onUnauth = defaultOnUnauthenticated
	}

	return func(c *fiber.Ctx) error {
		user := userLookup(c)
		if user == nil {
			return onUnauth(c, ErrNoUser)
		}

		var err error
		switch {
		case len(caps) == 0:
			// No caps requested — the gate only asserts authentication.
			err = nil
		case len(caps) == 1:
			err = service.Check(c.UserContext(), user, caps[0])
		case cfg.Mode == ModeAny:
			err = service.CheckAny(c.UserContext(), user, caps...)
		default:
			err = service.CheckAll(c.UserContext(), user, caps...)
		}

		if err != nil {
			if errors.Is(err, ErrNoUser) {
				return onUnauth(c, err)
			}
			return onDenied(c, err)
		}
		return c.Next()
	}
}

func defaultOnDenied(c *fiber.Ctx, err error) error {
	return c.Status(fiber.StatusForbidden).JSON(fiber.Map{
		"success": false,
		"message": "Permission denied",
	})
}

func defaultOnUnauthenticated(c *fiber.Ctx, err error) error {
	return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
		"success": false,
		"message": "Unauthorized",
	})
}
