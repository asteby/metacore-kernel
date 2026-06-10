package audit

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// QueryParams drives a List over the audit trail. The zero value lists the
// whole (org-scoped) trail newest-first with the default page size. OrganizationID
// is REQUIRED — the trail is always tenant-scoped; an empty org yields an error
// from List so a caller can never accidentally leak cross-tenant rows.
//
// All filters are AND-combined and optional:
//   - Model         exact model name (e.g. "products")
//   - AddonKey      exact producing addon ("kernel" for core)
//   - Action        created|updated|deleted
//   - ActorID       the actor uuid
//   - CorrelationID all events in one logical request
//   - RecordID      history of a single record (combine with Model)
//   - From / To     OccurredAt window; To is treated inclusive-of-end-day
//   - Q             free-text ILIKE across model/record_id/addon_key
//
// Pagination: Page is 1-based (default 1); PerPage defaults to 50 and is clamped
// to [1, MaxPerPage].
type QueryParams struct {
	OrganizationID uuid.UUID

	Model         string
	AddonKey      string
	Action        string
	ActorID       *uuid.UUID
	CorrelationID *uuid.UUID
	RecordID      string
	From          *time.Time
	To            *time.Time
	Q             string

	Page    int
	PerPage int

	// TableName overrides the table read from (default DefaultTableName). Set it
	// to match the table a host wired the Recorder to.
	TableName string
}

const (
	defaultPerPage = 50
	// MaxPerPage caps PerPage to protect the DB from unbounded scans.
	MaxPerPage = 200
)

// Query is the read service over the audit trail. It is a thin, stateless
// wrapper — construct one with the host's *gorm.DB (or pass the db per-call via
// List's signature). It returns rows; a host mounts its own HTTP handler /
// response envelope on top.
type Query struct {
	db    *gorm.DB
	table string
}

// NewQuery builds a Query bound to db. Pass opts to override the table name.
func NewQuery(db *gorm.DB, opts ...Option) *Query {
	o := defaultOptions()
	for _, opt := range opts {
		opt(o)
	}
	return &Query{db: db, table: o.tableName}
}

// List returns one page of audit entries plus the unfiltered-by-pagination
// total count, newest-first, scoped to params.OrganizationID. It is the single
// query primitive a host's "GET /activity" handler calls.
//
// The db argument lets a caller pass a request-scoped session; pass nil to use
// the Query's bound db.
func (qs *Query) List(ctx context.Context, db *gorm.DB, params QueryParams) (entries []Entry, total int64, err error) {
	if db == nil {
		db = qs.db
	}
	if db == nil {
		return nil, 0, fmt.Errorf("audit: List requires a *gorm.DB")
	}
	if params.OrganizationID == uuid.Nil {
		return nil, 0, fmt.Errorf("audit: List requires OrganizationID")
	}

	table := params.TableName
	if table == "" {
		table = qs.table
	}
	if table == "" {
		table = DefaultTableName
	}

	page := params.Page
	if page < 1 {
		page = 1
	}
	perPage := params.PerPage
	if perPage < 1 {
		perPage = defaultPerPage
	}
	if perPage > MaxPerPage {
		perPage = MaxPerPage
	}

	q := db.WithContext(ctx).Table(table).
		Where("organization_id = ?", params.OrganizationID)

	if m := strings.TrimSpace(params.Model); m != "" {
		q = q.Where("model = ?", m)
	}
	if ak := strings.TrimSpace(params.AddonKey); ak != "" {
		q = q.Where("addon_key = ?", ak)
	}
	if a := strings.TrimSpace(params.Action); a != "" {
		q = q.Where("action = ?", a)
	}
	if params.ActorID != nil && *params.ActorID != uuid.Nil {
		q = q.Where("actor_id = ?", *params.ActorID)
	}
	if params.CorrelationID != nil && *params.CorrelationID != uuid.Nil {
		q = q.Where("correlation_id = ?", *params.CorrelationID)
	}
	if rid := strings.TrimSpace(params.RecordID); rid != "" {
		q = q.Where("record_id = ?", rid)
	}
	if params.From != nil {
		q = q.Where("occurred_at >= ?", *params.From)
	}
	if params.To != nil {
		// Inclusive of the end day: extend to the final instant before the next.
		q = q.Where("occurred_at <= ?", params.To.Add(24*time.Hour-time.Nanosecond))
	}
	if qstr := strings.TrimSpace(params.Q); qstr != "" {
		like := "%" + qstr + "%"
		q = q.Where(
			"model ILIKE ? OR record_id ILIKE ? OR addon_key ILIKE ?",
			like, like, like,
		)
	}

	if err = q.Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("audit: count: %w", err)
	}

	offset := (page - 1) * perPage
	if err = q.Order("occurred_at DESC").
		Limit(perPage).
		Offset(offset).
		Find(&entries).Error; err != nil {
		return nil, 0, fmt.Errorf("audit: fetch: %w", err)
	}

	return entries, total, nil
}
