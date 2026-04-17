package auth

import (
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
)

// Fiber context local keys. Exported so downstream middlewares (e.g. an
// app-specific branch resolver) can read/write the same values.
const (
	LocalClaims         = "auth_claims"
	LocalUserID         = "user_id"
	LocalOrganizationID = "organization_id"
	LocalEmail          = "user_email"
	LocalRole           = "user_role"
)

// MiddlewareConfig configures the Fiber JWT middleware.
type MiddlewareConfig struct {
	// Secret is the HMAC secret used to validate tokens. Required.
	Secret []byte
	// Skipper optionally skips the middleware for a request (public routes,
	// health checks, etc.). If it returns true the middleware calls Next()
	// without inspecting Authorization.
	Skipper func(*fiber.Ctx) bool
	// Unauthorized lets apps customize the 401 response. When nil the
	// middleware responds with {"success": false, "message": "..."}.
	Unauthorized func(c *fiber.Ctx, err error) error
}

// Middleware returns a Fiber handler that validates a bearer (or ?token=)
// JWT and, on success, stashes the claims plus convenience fields in
// c.Locals. It intentionally does NOT touch X-Branch-ID — that is
// app-specific and belongs in a chained middleware.
func Middleware(config MiddlewareConfig) fiber.Handler {
	unauthorized := config.Unauthorized
	if unauthorized == nil {
		unauthorized = defaultUnauthorized
	}

	return func(c *fiber.Ctx) error {
		if config.Skipper != nil && config.Skipper(c) {
			return c.Next()
		}

		token := extractToken(c)
		if token == "" {
			return unauthorized(c, ErrMissingToken)
		}

		claims, err := ValidateToken(token, config.Secret)
		if err != nil {
			return unauthorized(c, err)
		}

		c.Locals(LocalClaims, claims)
		c.Locals(LocalUserID, claims.UserID)
		c.Locals(LocalOrganizationID, claims.OrganizationID)
		c.Locals(LocalEmail, claims.Email)
		c.Locals(LocalRole, claims.Role)

		return c.Next()
	}
}

// extractToken pulls a token from `Authorization: Bearer <token>` or the
// `?token=` query parameter (useful for browser file downloads / websockets).
func extractToken(c *fiber.Ctx) string {
	if h := c.Get(fiber.HeaderAuthorization); h != "" {
		parts := strings.SplitN(h, " ", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
			return strings.TrimSpace(parts[1])
		}
		// Some clients send the raw token; accept that too rather than 401.
		return strings.TrimSpace(h)
	}
	return c.Query("token")
}

func defaultUnauthorized(c *fiber.Ctx, err error) error {
	msg := MsgUnauthorized
	if err == ErrMissingToken || err == ErrInvalidToken || err == ErrExpiredToken {
		msg = MsgInvalidOrExpired
	}
	return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
		"success": false,
		"message": msg,
	})
}

// GetClaims returns the claims attached by Middleware, or nil if none.
func GetClaims(c *fiber.Ctx) *Claims {
	v := c.Locals(LocalClaims)
	if v == nil {
		return nil
	}
	claims, _ := v.(*Claims)
	return claims
}

// GetUserID returns the authenticated user id or uuid.Nil if not set.
func GetUserID(c *fiber.Ctx) uuid.UUID {
	v := c.Locals(LocalUserID)
	if id, ok := v.(uuid.UUID); ok {
		return id
	}
	return uuid.Nil
}

// GetOrganizationID returns the authenticated organization id or uuid.Nil.
func GetOrganizationID(c *fiber.Ctx) uuid.UUID {
	v := c.Locals(LocalOrganizationID)
	if id, ok := v.(uuid.UUID); ok {
		return id
	}
	return uuid.Nil
}

// GetRole returns the authenticated role string (may be empty).
func GetRole(c *fiber.Ctx) string {
	if v, ok := c.Locals(LocalRole).(string); ok {
		return v
	}
	return ""
}

// GetEmail returns the authenticated email (may be empty).
func GetEmail(c *fiber.Ctx) string {
	if v, ok := c.Locals(LocalEmail).(string); ok {
		return v
	}
	return ""
}
