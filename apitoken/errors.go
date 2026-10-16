package apitoken

import "errors"

// Typed errors so transports map to status codes without string matching.
var (
	// ErrNotFound — no token with that id.
	ErrNotFound = errors.New("apitoken: token not found")
	// ErrInvalidToken — the plaintext is malformed or unknown. Deliberately
	// the same error for both so callers cannot probe which tokens exist.
	ErrInvalidToken = errors.New("apitoken: invalid token")
	// ErrRevoked — the token was revoked.
	ErrRevoked = errors.New("apitoken: token revoked")
	// ErrExpired — the token is past its ExpiresAt.
	ErrExpired = errors.New("apitoken: token expired")
	// ErrInvalidKind — unknown kind, or a kind/organization mismatch
	// (platform tokens have none, the others need one).
	ErrInvalidKind = errors.New("apitoken: invalid kind")
	// ErrInvalidScope — malformed scope, or a scope a publishable token may
	// not hold.
	ErrInvalidScope = errors.New("apitoken: invalid scope")
	// ErrInvalidInput — empty/too long name, negative rate limit, expiry in
	// the past.
	ErrInvalidInput = errors.New("apitoken: invalid input")
	// ErrDuplicateHash — a token with the same hash already exists.
	ErrDuplicateHash = errors.New("apitoken: duplicate token hash")
)
