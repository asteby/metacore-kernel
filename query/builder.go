package query

import (
	"fmt"
	"strings"

	"github.com/asteby/metacore-kernel/modelbase"
	"gorm.io/gorm"
)

// Builder translates Params into GORM clauses, gated by a
// TableMetadata for column whitelisting.
//
// Builder is stateless w.r.t. the request — safe to create one per model
// at boot and share across goroutines. All validation state (allowed
// columns, searchable columns) is derived from the meta passed to New.
type Builder struct {
	meta *modelbase.TableMetadata

	// allowed is the set of column keys that filters and sort may target.
	// Populated from meta.Columns at construction so Apply is O(1) per
	// filter key instead of O(columns).
	allowed map[string]struct{}

	// searchable is the ordered list of columns that Search fans out to.
	// Sourced from meta.SearchColumns, intersected with allowed so the
	// builder never emits SQL for a column the frontend cannot see.
	searchable []string
}

// New constructs a Builder bound to a TableMetadata. A nil meta is
// tolerated (yields a Builder that permits nothing — handy for tests of
// the error paths) but production callers should always pass a real one.
func New(meta *modelbase.TableMetadata) *Builder {
	b := &Builder{meta: meta, allowed: map[string]struct{}{}}
	if meta == nil {
		return b
	}

	for _, col := range meta.Columns {
		if col.Key == "" || !isSafeIdent(col.Key) {
			continue
		}
		b.allowed[col.Key] = struct{}{}
	}
	for _, s := range meta.SearchColumns {
		if _, ok := b.allowed[s]; !ok {
			continue
		}
		if !isSafeIdent(s) {
			continue
		}
		b.searchable = append(b.searchable, s)
	}
	return b
}

// Apply layers sort + filters + search onto db. Pagination is applied
// separately via Paginate so Count can run on the un-paginated query.
//
// Apply never returns an error: every validation failure (unknown column,
// unsafe identifier) drops the offending clause silently — a garbage sort
// param produces a default-sorted list, not a 400.
func (b *Builder) Apply(db *gorm.DB, params Params) *gorm.DB {
	db = b.applyFilters(db, params)
	db = b.applySearch(db, params)
	db = b.applySort(db, params)
	return db
}

// Paginate applies LIMIT/OFFSET from params. Call this AFTER Count, on
// the same base query returned by Apply. Pagination is idempotent: calling
// Paginate twice yields the same limit/offset (GORM overwrites prior
// LIMIT/OFFSET clauses).
func (b *Builder) Paginate(db *gorm.DB, params Params) *gorm.DB {
	page, perPage := b.normalizePage(params)
	offset := (page - 1) * perPage
	return db.Offset(offset).Limit(perPage)
}

// Count runs COUNT(*) against the query, stripping any LIMIT/OFFSET.
// Use this BEFORE Paginate so the total reflects the filtered set, not
// the current page. The query is cloned via gorm.Session so the caller's
// original *gorm.DB remains usable for Find.
func (b *Builder) Count(db *gorm.DB, params Params) (int64, error) {
	var total int64
	// Use a session clone so setting Limit(-1).Offset(-1) does not
	// mutate the caller's query pipeline.
	session := db.Session(&gorm.Session{})
	if err := session.Limit(-1).Offset(-1).Count(&total).Error; err != nil {
		return 0, err
	}
	return total, nil
}

// PageMeta builds the pagination envelope returned alongside the data
// slice. LastPage is 1 when total is 0 so that frontends can safely
// render "Page 1 of 1" even on empty results.
func (b *Builder) PageMeta(total int64, params Params) PageMeta {
	page, perPage := b.normalizePage(params)
	last := int64(1)
	if perPage > 0 {
		last = (total + int64(perPage) - 1) / int64(perPage)
		if last < 1 {
			last = 1
		}
	}
	return PageMeta{
		Total:    total,
		Page:     page,
		PerPage:  perPage,
		LastPage: int(last),
	}
}

// PageMeta is the JSON envelope the frontend consumes for pagination.
// The tags are load-bearing — changing them is a MAJOR bump.
type PageMeta struct {
	Total    int64 `json:"total"`
	Page     int   `json:"page"`
	PerPage  int   `json:"per_page"`
	LastPage int   `json:"last_page"`
}

// normalizePage returns the effective page and per-page for params,
// applying defaults and clamps. Kept in one place so Paginate and
// PageMeta always agree.
func (b *Builder) normalizePage(params Params) (int, int) {
	page := params.Page
	if page < 1 {
		page = DefaultPage
	}
	perPage := params.PerPage
	if perPage < 1 {
		perPage = DefaultPerPage
	}
	if perPage > MaxPerPage {
		perPage = MaxPerPage
	}
	return page, perPage
}

// applySort emits an ORDER BY clause if SortBy is whitelisted. When SortBy
// is empty or unknown, applySort is a no-op so the caller's default ordering
// (typically DESC on created_at via a scope) survives untouched.
func (b *Builder) applySort(db *gorm.DB, params Params) *gorm.DB {
	col := strings.TrimSpace(params.SortBy)
	if col == "" {
		return db
	}
	if _, ok := b.allowed[col]; !ok {
		return db
	}
	if !isSafeIdent(col) {
		return db
	}
	order := strings.ToLower(params.Order)
	if order != "asc" && order != "desc" {
		order = "desc"
	}
	// Safe: col passed isSafeIdent, order is a fixed string literal.
	return db.Order(col + " " + order)
}

// applyFilters fans params.Filters out to typed GORM Where clauses. Each
// key is whitelisted and validated as a safe SQL identifier; unknown or
// unsafe keys are dropped silently. Each op is dispatched to a helper
// that uses placeholder binding for the value — we never interpolate
// user-supplied strings into the SQL fragment.
func (b *Builder) applyFilters(db *gorm.DB, params Params) *gorm.DB {
	for col, f := range params.Filters {
		if _, ok := b.allowed[col]; !ok {
			continue
		}
		if !isSafeIdent(col) {
			continue
		}
		db = applyOneFilter(db, col, f)
	}
	return db
}

// applyOneFilter emits a single WHERE clause for one (column, filter)
// pair. It is a free function (not a method) so future call sites — e.g.
// a debug tool or a dry-run planner — can reuse it without a Builder.
func applyOneFilter(db *gorm.DB, col string, f Filter) *gorm.DB {
	switch f.Op {
	case OpEq:
		v, ok := f.Value.(string)
		if !ok || v == "" {
			return db
		}
		return db.Where(fmt.Sprintf("%s = ?", col), v)
	case OpIlike:
		v, ok := f.Value.(string)
		if !ok || v == "" {
			return db
		}
		return db.Where(fmt.Sprintf("%s ILIKE ?", col), "%"+escapeLike(v)+"%")
	case OpIn:
		vals, ok := f.Value.([]string)
		if !ok || len(vals) == 0 {
			return db
		}
		return db.Where(fmt.Sprintf("%s IN ?", col), vals)
	case OpGte:
		v, ok := f.Value.(string)
		if !ok || v == "" {
			return db
		}
		return db.Where(fmt.Sprintf("%s >= ?", col), v)
	case OpLte:
		v, ok := f.Value.(string)
		if !ok || v == "" {
			return db
		}
		return db.Where(fmt.Sprintf("%s <= ?", col), v)
	case OpRange:
		rng, ok := f.Value.([2]string)
		if !ok {
			return db
		}
		switch {
		case rng[0] != "" && rng[1] != "":
			return db.Where(fmt.Sprintf("%s BETWEEN ? AND ?", col), rng[0], rng[1])
		case rng[0] != "":
			return db.Where(fmt.Sprintf("%s >= ?", col), rng[0])
		case rng[1] != "":
			return db.Where(fmt.Sprintf("%s <= ?", col), rng[1])
		}
	}
	return db
}

// applySearch emits one ILIKE clause OR'd across every searchable column
// when params.Search is non-empty. If the model declares no searchable
// columns the clause is skipped entirely (search is a no-op rather than
// an error — matches the audited source behaviour).
func (b *Builder) applySearch(db *gorm.DB, params Params) *gorm.DB {
	term := strings.TrimSpace(params.Search)
	if term == "" || len(b.searchable) == 0 {
		return db
	}
	if len(term) > MaxSearchTermLength {
		term = term[:MaxSearchTermLength]
	}

	pattern := "%" + escapeLike(term) + "%"

	conds := make([]string, 0, len(b.searchable))
	args := make([]interface{}, 0, len(b.searchable))
	for _, col := range b.searchable {
		// Defence-in-depth: col already checked at New time, re-check
		// in case meta was mutated between New and Apply (tests do this).
		if !isSafeIdent(col) {
			continue
		}
		conds = append(conds, fmt.Sprintf("%s ILIKE ?", col))
		args = append(args, pattern)
	}
	if len(conds) == 0 {
		return db
	}
	return db.Where(strings.Join(conds, " OR "), args...)
}

// escapeLike escapes LIKE/ILIKE wildcard characters so a user-supplied
// search term is matched literally. GORM binds the value as a parameter
// (no SQL injection risk), but without escaping the user could still
// exploit wildcard semantics to force a full scan or leak structure.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}
