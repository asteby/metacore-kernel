package query

import "errors"

// Sentinel errors returned by the query package. Apps should branch with
// errors.Is rather than matching on message text.
var (
	// ErrInvalidParam signals that a query parameter (page, per_page) was
	// malformed beyond what ParseFromMap can sanitize silently.
	ErrInvalidParam = errors.New("query: invalid parameter")

	// ErrNoSearchColumns is returned by helper code paths (not the builder
	// itself) when an app requests search but the model declares no
	// SearchColumns in its TableMetadata.
	ErrNoSearchColumns = errors.New("query: no search columns defined")
)

// Defaults and hard limits. The 200-row per-page cap comfortably covers
// dashboards without letting a client exhaust a worker on a single request.
const (
	// DefaultPage is the 1-indexed default page when no page parameter is
	// supplied by the client.
	DefaultPage = 1

	// DefaultPerPage is the default page size.
	DefaultPerPage = 15

	// MaxPerPage caps the page size. Requests above this are clamped
	// silently — the client gets a smaller page, not an error.
	MaxPerPage = 200

	// MaxSearchTermLength caps the accepted search string length to avoid
	// pathological ILIKE patterns.
	MaxSearchTermLength = 100
)
