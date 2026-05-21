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

	// ErrUnknownAggregate is returned by ParseFromMap when the client
	// supplied an aggregation function that is not in the supported set
	// (sum, count, avg, min, max). Unlike unknown filter columns, an
	// unknown aggregate is loud — clients that ask for it have explicit
	// intent and should learn early that the function is unsupported.
	ErrUnknownAggregate = errors.New("query: unknown aggregate function")

	// ErrInvalidAggregate is returned when the aggregate token is
	// syntactically malformed (e.g. missing parens, empty field name).
	ErrInvalidAggregate = errors.New("query: invalid aggregate expression")

	// ErrUnknownRelation is returned at Apply time when params reference a
	// relation that is not declared on the builder's bound RelationDef
	// set. Used by relation filters and preloads. Apply itself does not
	// surface this error — it drops the offending clause silently to
	// match the audited filter behaviour — but Validate/Plan helpers and
	// future call sites can branch on it.
	ErrUnknownRelation = errors.New("query: unknown relation")
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
