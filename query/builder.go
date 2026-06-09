package query

import (
	"fmt"
	"strings"
	"time"

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

	// relations is the optional set of model-to-model edges the builder
	// may JOIN against (for relation filters) or Preload (for ?with=).
	// Populated via WithRelations — kept optional so callers that never
	// touch the new features do not have to plumb modelbase.HasRelations
	// through their boot path.
	relations map[string]modelbase.RelationDef

	// tableName, when non-empty, is the SQL table name of the bound
	// model. Used to disambiguate columns in GROUP BY when relation
	// joins are present. Populated by WithTableName; callers that do
	// not set it fall back to bare column names.
	tableName string

	// dialect, when non-nil, is the per-value filter decoder a host has
	// opted into via WithFilterDialect. It is used ONLY by ParseValues
	// (the builder-driven parser); Apply never touches it. nil means the
	// default dialect (parseFilterValue) — i.e. exactly today's
	// behaviour. This is the extensibility lever: a host wires the ops
	// dialect (or its own) without forking the kernel.
	dialect ParseFilterValue

	// opsParse, when true, routes ParseValues through ParseOpsFromValues
	// so the ops JSONB / native-array / relation promotions apply, not
	// just the per-value dialect. Set by WithOpsDialect.
	opsParse bool

	// searchClause, when non-nil, builds the per-column SQL fragment + bind
	// value for the free-text search OR-clause. Set via WithSearchClause. nil
	// means the default `<col> ILIKE ?` with `%term%` — i.e. exactly today's
	// behaviour. Hosts wire a dialect-specific clause (e.g. Postgres
	// `unaccent(<col>) ILIKE unaccent(?)`) without forking the kernel.
	searchClause SearchClause
}

// SearchClause builds the SQL fragment and bind value for ONE column of the
// free-text search OR-clause. Mirrors the dynamic.SearchMatchClause shape so a
// host can pass the same function to both Options/Search and List. Returning
// ("", nil) skips the column.
type SearchClause func(col, term string) (fragment string, value any)

// Option configures a Builder at construction. Options are applied in
// order after the meta-derived state is built. The variadic form keeps
// New(meta) backward-compatible: callers that pass no options compile and
// behave exactly as before.
type Option func(*Builder)

// WithFilterDialect selects the per-value filter dialect the builder's
// ParseValues method uses. Passing ParseOpsFilterValue opts a host into
// the full ops wire-syntax (operators + RANGE + date-range). Passing nil,
// or omitting the option entirely, leaves the default dialect in place —
// preserving today's behaviour for every existing consumer.
//
//	qb := query.New(meta, query.WithFilterDialect(query.ParseOpsFilterValue))
func WithFilterDialect(d ParseFilterValue) Option {
	return func(b *Builder) { b.dialect = d }
}

// WithOpsDialect is the ergonomic shorthand that opts a host fully into the
// ops wire-syntax: it sets the per-value dialect to ParseOpsFilterValue AND
// routes Builder.ParseValues through ParseOpsFromValues (so JSONB-path,
// relation, and native-array promotions apply). Omitting it leaves the
// builder on the default parser — today's behaviour, byte-for-byte.
//
//	qb := query.New(meta, query.WithOpsDialect())
//	params, _ := qb.ParseValues(req.URL.Query())
func WithOpsDialect() Option {
	return func(b *Builder) {
		b.dialect = ParseOpsFilterValue
		b.opsParse = true
	}
}

// WithSearchClause sets the per-column match clause for the free-text `search`
// param. Omitting it (or passing nil) keeps the default `<col> ILIKE ?` —
// today's behaviour. A Postgres host with the unaccent extension typically wires:
//
//	query.New(meta, query.WithSearchClause(func(col, q string) (string, any) {
//	    return fmt.Sprintf("unaccent(%s) ILIKE unaccent(?)", col), "%" + q + "%"
//	}))
func WithSearchClause(c SearchClause) Option {
	return func(b *Builder) { b.searchClause = c }
}

// New constructs a Builder bound to a TableMetadata. A nil meta is
// tolerated (yields a Builder that permits nothing — handy for tests of
// the error paths) but production callers should always pass a real one.
func New(meta *modelbase.TableMetadata, opts ...Option) *Builder {
	b := &Builder{
		meta:      meta,
		allowed:   map[string]struct{}{},
		relations: map[string]modelbase.RelationDef{},
	}
	if meta != nil {
		buildAllowed(b, meta)
	}
	for _, opt := range opts {
		if opt != nil {
			opt(b)
		}
	}
	return b
}

// buildAllowed populates the whitelist + searchable set from meta. Split
// out of New so the variadic options can be applied afterwards without
// duplicating the derivation.
func buildAllowed(b *Builder, meta *modelbase.TableMetadata) {
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
}

// WithRelations binds the relation set the builder may JOIN against
// (relation filters) or Preload (?with=). Names that fail the SQL-
// identifier check are dropped so a malformed RelationDef cannot leak
// into raw SQL. Returns the same builder so callers can chain:
//
//	qb := query.New(meta).WithRelations(model.DefineRelations()).WithTableName("orders")
//
// Pass nil or an empty slice to disable relation features entirely.
// Idempotent — calling WithRelations twice replaces the prior set
// rather than appending, so a host can rebuild the binding after a
// hot-reload without restarting.
func (b *Builder) WithRelations(rels []modelbase.RelationDef) *Builder {
	out := make(map[string]modelbase.RelationDef, len(rels))
	for _, r := range rels {
		if r.Name == "" || !isSafeIdent(r.Name) {
			continue
		}
		// ForeignKey is required for every kind we render. Through
		// (target table) is required for joins; addons without it
		// still register as "known" relations so Apply can drop them
		// cleanly with a typed error rather than silently producing
		// broken SQL.
		out[r.Name] = r
	}
	b.relations = out
	return b
}

// WithTableName declares the SQL table name of the bound model. The
// builder uses it to qualify columns in GROUP BY and aggregation
// SELECTs once a relation JOIN is in play — otherwise PostgreSQL would
// reject ambiguous unqualified column references. Callers that do not
// use relation features can omit it without consequence.
func (b *Builder) WithTableName(name string) *Builder {
	if isSafeIdent(name) {
		b.tableName = name
	}
	return b
}

// Apply layers sort + filters + search onto db. Pagination is applied
// separately via Paginate so Count can run on the un-paginated query.
//
// Apply never returns an error: every validation failure (unknown column,
// unsafe identifier) drops the offending clause silently — a garbage sort
// param produces a default-sorted list, not a 400.
//
// Order of operations is significant: Preloads and relation joins are
// applied BEFORE filters/search/sort so any column qualifier the
// downstream clauses emit (e.g. "orders.status = ?" once a JOIN is
// present) lines up with the join graph.
func (b *Builder) Apply(db *gorm.DB, params Params) *gorm.DB {
	db = b.applyPreloads(db, params)
	db = b.applyRelationFilters(db, params)
	db = b.applyAggregations(db, params)
	db = b.applyGroupBy(db, params)
	db = b.applyFilters(db, params)
	db = b.applySearch(db, params)
	db = b.applySort(db, params)
	return db
}

// ApplyForCount applies only the WHERE-shaping clauses (relation filters,
// column filters, search) used to compute a list's total. It deliberately
// omits sort, pagination and preloads: a bare COUNT(*) must NOT carry an
// ORDER BY, because Postgres rejects ordering an aggregate by a non-grouped
// column ("column must appear in the GROUP BY clause", SQLSTATE 42803) — which
// otherwise 500s the whole list whenever a sort is active. Callers that count
// a filtered list should use this instead of Apply.
func (b *Builder) ApplyForCount(db *gorm.DB, params Params) *gorm.DB {
	db = b.applyRelationFilters(db, params)
	db = b.applyFilters(db, params)
	db = b.applySearch(db, params)
	return db
}

// ApplyForAggregate applies the WHERE-shaping clauses (relation filters,
// column filters, search) plus the SELECT-list aggregation expressions and
// GROUP BY used to compute a list's footer totals. Like ApplyForCount it
// deliberately omits sort, pagination and preloads: a footer total is a single
// aggregate over the filtered set, so an ORDER BY would both be meaningless and
// — over a non-grouped aggregate — 42803 in Postgres. Callers that want the SUM
// of a column over the SAME filtered set the list shows should use this so the
// footer matches the body row-for-row (minus pagination).
func (b *Builder) ApplyForAggregate(db *gorm.DB, params Params) *gorm.DB {
	db = b.applyRelationFilters(db, params)
	db = b.applyAggregations(db, params)
	db = b.applyGroupBy(db, params)
	db = b.applyFilters(db, params)
	db = b.applySearch(db, params)
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
// The tags are load-bearing — changing or removing them is a MAJOR bump.
// ADDING fields (with their own json tag) is backward-compatible: existing
// JSON consumers ignore unknown keys.
//
// CurrentPage, From and To were added for the ops envelope; they are
// `omitempty` so the default PageMeta (built by PageMeta()) serialises
// EXACTLY as before — the new keys never appear unless OpsMeta populates
// them. This keeps every existing consumer byte-for-byte identical.
type PageMeta struct {
	Total    int64 `json:"total"`
	Page     int   `json:"page"`
	PerPage  int   `json:"per_page"`
	LastPage int   `json:"last_page"`

	// CurrentPage mirrors Page under the snake_case key the ops frontend
	// reads. Omitted unless set by OpsMeta.
	CurrentPage int `json:"current_page,omitempty"`
	// From is the 1-indexed position of the first row on this page
	// (offset+1), or 0 when the page is empty. Omitted unless set by
	// OpsMeta.
	From int `json:"from,omitempty"`
	// To is the 1-indexed position of the last row on this page
	// (offset+len(rows)). Omitted unless set by OpsMeta.
	To int `json:"to,omitempty"`
}

// OpsMeta builds the full ops-compatible pagination envelope. It is a
// superset of PageMeta: the four base fields plus current_page, from and
// to. rowCount is the number of rows actually returned for the current
// page (len(results)) and is required to compute `to` precisely on a short
// final page. Pass 0 when the row count is unknown — `from`/`to` then
// reflect the requested window.
//
// This method is ADDITIVE; PageMeta() is unchanged and remains the default
// envelope for every existing consumer.
func (b *Builder) OpsMeta(total int64, rowCount int, params Params) PageMeta {
	m := b.PageMeta(total, params)
	page, perPage := b.normalizePage(params)
	offset := (page - 1) * perPage
	m.CurrentPage = page
	if total == 0 || rowCount == 0 && offset >= int(total) {
		// Empty page: from/to stay 0.
		return m
	}
	m.From = offset + 1
	if rowCount > 0 {
		m.To = offset + rowCount
	} else {
		// Unknown row count → assume a full page, clamped to total.
		to := offset + perPage
		if int64(to) > total {
			to = int(total)
		}
		m.To = to
	}
	return m
}

// ParseValues parses request parameters honouring the builder's
// configured filter dialect. When no dialect was set (WithFilterDialect
// omitted) it delegates to ParseFromMap — the exact default behaviour.
// When the ops dialect is configured it delegates to ParseOpsFromValues so
// the full ops wire-syntax (operators, RANGE, date-range, JSONB) is
// honoured. Hosts call this instead of the free parser functions when they
// want a single builder-driven entry point.
func (b *Builder) ParseValues(values map[string][]string) (Params, error) {
	if b.opsParse {
		return ParseOpsFromValues(values)
	}
	if b.dialect != nil {
		// A custom per-value dialect without the full ops promotions:
		// parse with ParseFromMap's structure but decode flat filter
		// values through the supplied dialect.
		return parseWithDialect(values, b.dialect)
	}
	return ParseFromMap(values)
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
	case OpNeq:
		v, ok := f.Value.(string)
		if !ok || v == "" {
			return db
		}
		return db.Where(fmt.Sprintf("%s <> ?", col), v)
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
	// --- Extended ops dialect operators (additive). ---
	case OpNotIn:
		vals, ok := f.Value.([]string)
		if !ok || len(vals) == 0 {
			return db
		}
		return db.Where(fmt.Sprintf("%s NOT IN ?", col), vals)
	case OpLike:
		v, ok := f.Value.(string)
		if !ok || v == "" {
			return db
		}
		// Case-sensitive LIKE with explicit escape, matching ops.
		return db.Where(fmt.Sprintf("%s LIKE ? ESCAPE '\\'", col), "%"+escapeLike(v)+"%")
	case OpUnaccentIlike:
		v, ok := f.Value.(string)
		if !ok || v == "" {
			return db
		}
		// Accent- and case-insensitive, requires the unaccent extension.
		return db.Where(
			fmt.Sprintf("unaccent(%s) ILIKE unaccent(?) ESCAPE '\\'", col),
			"%"+escapeLike(v)+"%",
		)
	case OpGt:
		n, ok := f.Value.(float64)
		if !ok {
			return db
		}
		return db.Where(fmt.Sprintf("%s > ?", col), n)
	case OpLt:
		n, ok := f.Value.(float64)
		if !ok {
			return db
		}
		return db.Where(fmt.Sprintf("%s < ?", col), n)
	case OpNumGte:
		n, ok := f.Value.(float64)
		if !ok {
			return db
		}
		return db.Where(fmt.Sprintf("%s >= ?", col), n)
	case OpNumLte:
		n, ok := f.Value.(float64)
		if !ok {
			return db
		}
		return db.Where(fmt.Sprintf("%s <= ?", col), n)
	case OpNumRange:
		rng, ok := f.Value.([2]*float64)
		if !ok {
			return db
		}
		if rng[0] != nil {
			db = db.Where(fmt.Sprintf("%s >= ?", col), *rng[0])
		}
		if rng[1] != nil {
			db = db.Where(fmt.Sprintf("%s <= ?", col), *rng[1])
		}
		return db
	case OpDateRange:
		bounds, ok := f.Value.([2]time.Time)
		if !ok {
			return db
		}
		return db.Where(fmt.Sprintf("%s >= ? AND %s <= ?", col, col), bounds[0], bounds[1])
	case OpNull:
		return db.Where(fmt.Sprintf("%s IS NULL", col))
	case OpNotNull:
		return db.Where(fmt.Sprintf("%s IS NOT NULL", col))
	case OpJSONBEq:
		jf, ok := f.Value.(JSONBFilter)
		if !ok || jf.Key == "" {
			return db
		}
		// col is the JSONB column name (whitelisted); key + value bound.
		return db.Where(fmt.Sprintf("%s->>? = ?", col), jf.Key, jf.Val)
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
		// Host-supplied clause (e.g. unaccent ILIKE) wins; default is ILIKE.
		if b.searchClause != nil {
			frag, val := b.searchClause(col, term)
			if frag == "" {
				continue
			}
			conds = append(conds, frag)
			args = append(args, val)
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

// applyPreloads issues db.Preload for every relation name in
// params.Preloads that the builder knows about. Unknown names are
// dropped silently — matches Filters' "garbage in → safe degrade"
// policy. The preloaded association name is the GORM struct field
// name (e.g. "Customer") in conventional kernel models; since
// RelationDef.Name uses the same convention by contract, we pass it
// through verbatim.
func (b *Builder) applyPreloads(db *gorm.DB, params Params) *gorm.DB {
	if len(params.Preloads) == 0 || len(b.relations) == 0 {
		return db
	}
	for _, name := range params.Preloads {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if !isSafeIdent(name) {
			continue
		}
		if _, ok := b.relations[name]; !ok {
			continue
		}
		db = db.Preload(name)
	}
	return db
}

// applyRelationFilters emits one INNER JOIN + one WHERE per declared
// relation filter. Only belongs_to and one_to_many edges are joinable
// today; many_to_many would require crossing the pivot table and is
// deferred until a real call site needs it.
//
// Unknown relations and relations missing ForeignKey/Through are
// dropped silently. The join clause is built from RelationDef
// metadata, never from client input — every identifier crossing into
// raw SQL is taken from the declared RelationDef, which the host
// controls.
func (b *Builder) applyRelationFilters(db *gorm.DB, params Params) *gorm.DB {
	if len(params.RelationFilters) == 0 || len(b.relations) == 0 {
		return db
	}
	joined := map[string]struct{}{}
	for _, rf := range params.RelationFilters {
		rel, ok := b.relations[rf.Relation]
		if !ok {
			continue
		}
		if rel.Through == "" || rel.ForeignKey == "" {
			continue
		}
		if !isSafeIdent(rel.Through) || !isSafeIdent(rel.ForeignKey) {
			continue
		}
		if !isSafeIdent(rf.Field) {
			continue
		}
		// References defaults to "id" — matches GORM convention.
		refs := rel.References
		if refs == "" {
			refs = "id"
		}
		if !isSafeIdent(refs) {
			continue
		}
		// Skip many_to_many for now: needs pivot traversal that the
		// kernel does not have a stable API for. Document the gap.
		if strings.EqualFold(rel.Kind, "many_to_many") {
			continue
		}

		ownerTable := b.tableName
		if ownerTable == "" {
			// Fall back to the model the *gorm.DB is already bound to —
			// GORM resolves stmt.Table lazily; we emit a best-effort
			// join keyed off the FK metadata.
			ownerTable = ""
		}

		// Build the JOIN clause once per relation, even if multiple
		// filters target the same relation.
		if _, alreadyJoined := joined[rf.Relation]; !alreadyJoined {
			var joinSQL string
			switch strings.ToLower(rel.Kind) {
			case "belongs_to":
				// owner.fk = target.refs
				if ownerTable != "" {
					joinSQL = fmt.Sprintf(
						"JOIN %s ON %s.%s = %s.%s",
						rel.Through, ownerTable, rel.ForeignKey, rel.Through, refs,
					)
				} else {
					joinSQL = fmt.Sprintf(
						"JOIN %s ON %s = %s.%s",
						rel.Through, rel.ForeignKey, rel.Through, refs,
					)
				}
			case "one_to_many", "has_many", "":
				// target.fk = owner.refs (default kind = has_many for
				// legacy manifest authors that omit the field).
				if ownerTable != "" {
					joinSQL = fmt.Sprintf(
						"JOIN %s ON %s.%s = %s.%s",
						rel.Through, rel.Through, rel.ForeignKey, ownerTable, refs,
					)
				} else {
					joinSQL = fmt.Sprintf(
						"JOIN %s ON %s.%s = %s",
						rel.Through, rel.Through, rel.ForeignKey, refs,
					)
				}
			default:
				continue
			}
			db = db.Joins(joinSQL)
			joined[rf.Relation] = struct{}{}
		}

		// Now apply the WHERE clause against the joined table. We
		// always qualify with rel.Through to avoid column-name
		// collisions with the owner table.
		qualified := rel.Through + "." + rf.Field
		db = applyOneFilter(db, qualified, Filter{Op: rf.Op, Value: rf.Value})
	}
	return db
}

// applyAggregations emits SELECT-list aggregation expressions. When
// aggregations are present the builder OVERRIDES the default SELECT
// (which would otherwise be `SELECT *`) — the SELECT list becomes
// the GroupBy columns plus the aggregation aliases. Callers that want
// raw rows alongside aggregates must run two separate queries; this
// matches the wire protocol every host frontend already expects.
func (b *Builder) applyAggregations(db *gorm.DB, params Params) *gorm.DB {
	if len(params.Aggregations) == 0 {
		return db
	}
	parts := make([]string, 0, len(params.Aggregations)+len(params.GroupBy))

	// GroupBy columns first, in declaration order, so the SELECT list
	// mirrors the GROUP BY clause — required by every dialect.
	for _, col := range params.GroupBy {
		col = strings.TrimSpace(col)
		if col == "" {
			continue
		}
		if _, ok := b.allowed[col]; !ok {
			continue
		}
		if !isSafeIdent(col) {
			continue
		}
		parts = append(parts, b.qualifyOwner(col))
	}

	// Aggregation expressions next, each emitted as `FN(field) AS alias`.
	// Field is either "*" (count only — guarded by parser) or a
	// whitelisted column. The parser already enforces isSafeIdent on
	// alias and field, but we re-check here as defence in depth.
	for _, a := range params.Aggregations {
		if !isSafeIdent(a.Alias) {
			continue
		}
		fn := strings.ToUpper(string(a.Func))
		if a.Field == "*" {
			if a.Func != AggCount {
				continue
			}
			parts = append(parts, fmt.Sprintf("%s(*) AS %s", fn, a.Alias))
			continue
		}
		if !isSafeIdent(a.Field) {
			continue
		}
		// Whitelisting: a Builder bound to nil/empty meta has no
		// allowed columns, in which case we skip the check so test
		// fixtures can exercise the SQL emitter without a metadata
		// dance. Production callers will always have meta.
		if len(b.allowed) > 0 {
			if _, ok := b.allowed[a.Field]; !ok {
				continue
			}
		}
		parts = append(parts, fmt.Sprintf(
			"%s(%s) AS %s",
			fn, b.qualifyOwner(a.Field), a.Alias,
		))
	}

	if len(parts) == 0 {
		return db
	}
	return db.Select(strings.Join(parts, ", "))
}

// applyGroupBy emits the GROUP BY clause when params.GroupBy is
// non-empty. Columns are validated against the allowed set and the
// safe-ident regex; unknown or unsafe columns are dropped silently.
// When the builder has a table name set, columns are qualified so the
// GROUP BY survives a JOIN without ambiguity errors.
func (b *Builder) applyGroupBy(db *gorm.DB, params Params) *gorm.DB {
	if len(params.GroupBy) == 0 {
		return db
	}
	cols := make([]string, 0, len(params.GroupBy))
	for _, col := range params.GroupBy {
		col = strings.TrimSpace(col)
		if col == "" {
			continue
		}
		if _, ok := b.allowed[col]; !ok {
			continue
		}
		if !isSafeIdent(col) {
			continue
		}
		cols = append(cols, b.qualifyOwner(col))
	}
	if len(cols) == 0 {
		return db
	}
	return db.Group(strings.Join(cols, ", "))
}

// qualifyOwner prefixes col with the bound table name when one is set,
// otherwise returns col unchanged. Used by GROUP BY and aggregation
// SELECTs once a relation JOIN is in play; bare-column callers (no
// joins) get the unqualified form to keep emitted SQL readable.
func (b *Builder) qualifyOwner(col string) string {
	if b.tableName == "" {
		return col
	}
	return b.tableName + "." + col
}
