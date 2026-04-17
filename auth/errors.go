package auth

import "errors"

// Sentinel errors returned by the auth package. Apps can wrap these to map
// to HTTP responses or alter localized messages via the exported message
// constants below.
var (
	ErrInvalidCredentials = errors.New("auth: invalid credentials")
	ErrUserExists         = errors.New("auth: user already exists")
	ErrUserNotFound       = errors.New("auth: user not found")
	ErrInvalidToken       = errors.New("auth: invalid token")
	ErrExpiredToken       = errors.New("auth: token expired")
	ErrMissingToken       = errors.New("auth: missing token")
	ErrUnauthorized       = errors.New("auth: unauthorized")
	ErrUserModelNotSet    = errors.New("auth: user model factory not configured")
)

// Message constants (exported) so apps can override wire text without
// forking. They are intentionally plain English; override via i18n at the
// handler layer if needed.
const (
	MsgInvalidCredentials   = "Invalid credentials"
	MsgUserExists           = "User already exists"
	MsgInvalidRequestFormat = "Invalid request format"
	MsgMissingFields        = "Required fields missing"
	MsgUnauthorized         = "Unauthorized"
	MsgInvalidOrExpired     = "Invalid or expired token"
	MsgLoggedOut            = "Logged out successfully"
	MsgInternalError        = "Internal server error"
)
