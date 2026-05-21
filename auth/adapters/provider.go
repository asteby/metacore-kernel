// Package adapters defines the AuthUserProvider contract — the narrow,
// transport-agnostic shape the kernel needs to identify the authenticated
// principal — together with built-in extractors for the most common ways apps
// stash that principal in a request (modelbase.AuthUser, raw UUIDs in Fiber
// locals, JWT claims).
//
// The fat `modelbase.AuthUser` interface (~8 methods incl. setters and
// password-hash accessor) is too heavy for apps that authenticate via JWT and
// never materialise a full User row — e.g. metacore-ops, which puts raw
// uuid.UUID values in Fiber locals. AuthUserProvider exposes only what the
// authorization and tenant-scoping paths actually read:
//
//   - UserID()         — identity
//   - OrganizationID() — tenant scope
//   - Roles()          — role-based authorization input
//
// Apps that already use modelbase.AuthUser keep working unchanged: every
// concrete *modelbase.BaseUser also satisfies AuthUserProvider through the
// trivial wrapper in modelbase_user.go. The two interfaces coexist; the kernel
// internals continue to thread modelbase.AuthUser, and the adapters in this
// package bridge the gap at the HTTP boundary.
package adapters

import (
	"context"

	"github.com/google/uuid"
)

// AuthUserProvider is the narrow contract the kernel needs to identify the
// authenticated principal. It is intentionally read-only — registration and
// password flows continue to use modelbase.AuthUser.
//
// Implementations must be safe for concurrent reads (the kernel may call
// methods from multiple goroutines, e.g. when fanning out hook execution).
type AuthUserProvider interface {
	// UserID returns the authenticated user's id. uuid.Nil signals "no user"
	// and the kernel treats it as 401 in extractor paths.
	UserID() uuid.UUID

	// OrganizationID returns the tenant the user is acting in. Required for
	// the default OrganizationScoper; apps with a different scoping strategy
	// may return uuid.Nil and supply their own dynamic.TenantScoper.
	OrganizationID() uuid.UUID

	// Roles returns the user's role keys. Most apps return a single-element
	// slice (kernel-style RBAC); apps with multi-role users return all of
	// them. The kernel's default permission.Service uses the first role for
	// the SuperRole short-circuit and store lookups — additional roles are
	// merged at the capability layer when the app wires that policy.
	Roles() []string
}

// PermissionChecker is an optional interface a provider can implement when the
// app already knows the user's authoritative permission set (e.g. the JWT
// carries the resolved capability list, or the app has its own permission
// service). When present, dynamic.Service uses it INSTEAD OF calling
// permission.Service, allowing apps to bypass the kernel's permission store
// entirely.
//
// Implementations must be idempotent for the same input — the kernel may
// call HasPermission multiple times per request without caching.
type PermissionChecker interface {
	HasPermission(key string) bool
}

// AuthUserExtractor pulls an AuthUserProvider out of a request context. Apps
// wire this once at startup (typically alongside their auth middleware) and
// the kernel calls it whenever a handler needs to know the caller.
//
// Returning (nil, nil) signals "no authenticated user"; the kernel maps that
// to 401. A non-nil error is propagated unchanged (apps use it to distinguish
// "token expired" from "no token").
type AuthUserExtractor func(ctx context.Context) (AuthUserProvider, error)
