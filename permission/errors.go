package permission

import "errors"

// Sentinel errors. Callers compare with errors.Is; the HTTP layer maps them
// to status codes (ErrNoUser -> 401, ErrPermissionDenied -> 403).
var (
	// ErrPermissionDenied is returned by Check/CheckAny/CheckAll when the user
	// is authenticated but lacks the required capability.
	ErrPermissionDenied = errors.New("permission: denied")

	// ErrUnknownRole is returned by the store when a role is not recognised.
	// Services treat it as "no capabilities granted" rather than a fatal
	// failure so a newly added role does not 500 the API.
	ErrUnknownRole = errors.New("permission: unknown role")

	// ErrNoUser is returned by the middleware when no AuthUser can be resolved
	// from the request context (usually a missing or misconfigured auth
	// middleware upstream).
	ErrNoUser = errors.New("permission: no authenticated user")

	// ErrNilStore is returned by New when Config.Store is nil.
	ErrNilStore = errors.New("permission: nil store")
)
