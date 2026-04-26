// Package permission is the kernel's authorization service: given an
// authenticated user (modelbase.AuthUser) and a capability (e.g.
// "users.create"), decide whether the action is allowed.
//
// It is deliberately lean. The two stable contracts are:
//
//   - Capability — the string "resource.action" that apps declare when they
//     register their models. The kernel never hardcodes the list.
//   - PermissionStore — the backing store. InMemoryStore ships for tests and
//     simple apps; GormStore ships for the common RBAC case; apps can plug in
//     their own (Redis, branch-scoped, per-addon, ...) without forking.
//
// The service is framework-agnostic (pure Go, context.Context, sentinel
// errors). A thin Fiber middleware lives in middleware.go; everything else
// can be driven from a CLI, gRPC, or a test binary without Fiber.
//
// # Quick start
//
//	store := permission.NewInMemoryStore(
//	    map[permission.Role][]permission.Capability{
//	        permission.RoleAdmin: {permission.Cap("users", "create")},
//	    },
//	    nil,
//	)
//
//	svc := permission.New(permission.Config{Store: store})
//
//	if err := svc.Check(ctx, user, permission.Cap("users", "create")); err != nil {
//	    // ErrPermissionDenied -> 403, ErrNoUser -> 401
//	    return err
//	}
//
// # Fiber gating
//
//	app.Delete("/api/admin/dynamic/:model/:id",
//	    authMiddleware,
//	    svc.Gate(userFromCtx, permission.Cap("users", "delete")),
//	    deleteHandler)
//
// # Design rules (ARCHITECTURE.md)
//
// service.go MUST NOT import Fiber — only middleware.go does. Apps that want
// custom storage (e.g. branch-scoped permissions) implement PermissionStore
// themselves. The kernel stays app-agnostic.
package permission
