package adapters

import (
	"context"
	"fmt"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
)

// FiberLocalsKey is the context key the locals-based adapters use to stash the
// active *fiber.Ctx. Apps that build a kernel.Service from a non-Fiber path
// would normally never see this — the kernel's HTTP layer puts the ctx in
// place during Handler dispatch.
//
// Exported so apps with custom middleware can inject the same value if they
// need to drive the extractor from a non-Fiber goroutine (background jobs
// kicked off from a request, etc.).
type fiberCtxKeyT struct{}

// FiberCtxKey is the context key used to look up the active *fiber.Ctx in
// extractors that walk Fiber locals.
var FiberCtxKey = fiberCtxKeyT{}

// WithFiberCtx returns a child context carrying c so locals-based extractors
// can pull values from c.Locals without depending on Fiber's own context
// being threaded all the way down. The kernel HTTP layer calls this once per
// request; app code rarely needs it directly.
func WithFiberCtx(parent context.Context, c fiber.Ctx) context.Context {
	if c == nil {
		return parent
	}
	return context.WithValue(parent, FiberCtxKey, c)
}

// FiberCtxFrom returns the *fiber.Ctx previously stashed by WithFiberCtx, or
// nil if none. Adapters use it to avoid panicking when called from a
// non-Fiber context.
func FiberCtxFrom(ctx context.Context) fiber.Ctx {
	if ctx == nil {
		return nil
	}
	v := ctx.Value(FiberCtxKey)
	if v == nil {
		return nil
	}
	c, _ := v.(fiber.Ctx)
	return c
}

// UUIDLocalsProvider is the concrete AuthUserProvider materialised by
// FiberLocalsExtractor. Exported so tests and adapters in other packages can
// construct it directly.
type UUIDLocalsProvider struct {
	ID    uuid.UUID
	OrgID uuid.UUID
	// RoleKeys is optional — apps that do not store a role in locals leave it
	// empty, and the permission.Service treats them as a non-super principal.
	RoleKeys []string
}

// UserID implements AuthUserProvider.
func (p UUIDLocalsProvider) UserID() uuid.UUID { return p.ID }

// OrganizationID implements AuthUserProvider.
func (p UUIDLocalsProvider) OrganizationID() uuid.UUID { return p.OrgID }

// Roles implements AuthUserProvider.
func (p UUIDLocalsProvider) Roles() []string { return p.RoleKeys }

// FiberLocalsConfig customises FiberLocalsExtractor lookups. Zero values fall
// back to the kernel auth middleware defaults (LocalUserID, LocalOrganizationID,
// LocalRole) so apps that use auth.Middleware do not need to set anything.
type FiberLocalsConfig struct {
	// UserIDKey is the locals key holding the user id. Defaults to
	// auth.LocalUserID ("user_id") — the value written by auth.Middleware.
	UserIDKey string

	// OrgIDKey is the locals key holding the organization id. Defaults to
	// auth.LocalOrganizationID ("organization_id").
	OrgIDKey string

	// RoleKey is the locals key holding the role string. Defaults to
	// auth.LocalRole ("user_role"). Optional — when the value is missing or
	// empty the extractor returns a provider with an empty Roles slice and
	// the permission gate uses store-only authorization.
	RoleKey string
}

// Locals key defaults — kept in sync with auth/middleware.go via the
// const-import below. Duplicated here so adapters does not need to import the
// auth package (which would close a `auth -> adapters -> auth` cycle the day
// auth wants to use AuthUserProvider).
const (
	defaultUserIDKey = "user_id"
	defaultOrgIDKey  = "organization_id"
	defaultRoleKey   = "user_role"
)

// FiberLocalsExtractor returns an AuthUserExtractor that reads raw uuid.UUID
// values out of Fiber locals — the layout written by auth.Middleware and used
// by apps like metacore-ops that never materialise a full User struct.
//
// Pass FiberLocalsConfig{} to use the kernel defaults
// ("user_id" / "organization_id" / "user_role"). Empty values inside the
// config are also treated as "use the default" so apps can override only the
// keys they actually moved.
//
// Errors:
//
//   - When no *fiber.Ctx is attached to ctx (i.e. the kernel was driven
//     outside an HTTP request), the extractor returns
//     (nil, nil) — callers map that to 401.
//   - When the user id value exists but is not a uuid.UUID the extractor
//     returns an error so misconfigurations surface as 500 rather than 401.
func FiberLocalsExtractor(cfg FiberLocalsConfig) AuthUserExtractor {
	userKey := cfg.UserIDKey
	if userKey == "" {
		userKey = defaultUserIDKey
	}
	orgKey := cfg.OrgIDKey
	if orgKey == "" {
		orgKey = defaultOrgIDKey
	}
	roleKey := cfg.RoleKey
	if roleKey == "" {
		roleKey = defaultRoleKey
	}

	return func(ctx context.Context) (AuthUserProvider, error) {
		c := FiberCtxFrom(ctx)
		if c == nil {
			return nil, nil
		}

		idVal := c.Locals(userKey)
		if idVal == nil {
			return nil, nil
		}
		id, ok := idVal.(uuid.UUID)
		if !ok {
			return nil, fmt.Errorf("adapters: locals[%q] is %T, want uuid.UUID", userKey, idVal)
		}
		if id == uuid.Nil {
			return nil, nil
		}

		var orgID uuid.UUID
		if v := c.Locals(orgKey); v != nil {
			if parsed, ok := v.(uuid.UUID); ok {
				orgID = parsed
			} else {
				return nil, fmt.Errorf("adapters: locals[%q] is %T, want uuid.UUID", orgKey, v)
			}
		}

		var roles []string
		if v := c.Locals(roleKey); v != nil {
			switch r := v.(type) {
			case string:
				if r != "" {
					roles = []string{r}
				}
			case []string:
				roles = r
			}
		}

		return UUIDLocalsProvider{ID: id, OrgID: orgID, RoleKeys: roles}, nil
	}
}
