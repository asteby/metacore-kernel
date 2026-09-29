package dynamic

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/modelbase"
)

// Enforcement modes of a unique rule (see UniqueRuleStatus.Enforcement).
const (
	// UniqueEnforcedByIndex: a valid partial UNIQUE index backs the rule, so
	// uniqueness also holds for writers that bypass the kernel.
	UniqueEnforcedByIndex = "index"
	// UniqueEnforcedByApp: only the pre-write check (plus the advisory lock)
	// enforces the rule — the table holds duplicates, or the dialect has no
	// partial unique indexes.
	UniqueEnforcedByApp = "app"
)

// UniqueRuleStatus reports how one unique rule of a table is enforced.
type UniqueRuleStatus struct {
	ErrorKey    string   `json:"error_key"`
	Columns     []string `json:"columns"`
	Index       string   `json:"index"`
	Enforcement string   `json:"enforcement"`
	// BlockedGroups is the number of duplicate groups, across the whole table,
	// that keep the index from being built (0 once materialized).
	BlockedGroups int64 `json:"blocked_groups"`
	// Error is the last materialization failure, if any. Never fatal.
	Error string `json:"error,omitempty"`
}

// UniqueViolationGroup is one set of live rows sharing the values of a unique
// rule inside one organization.
type UniqueViolationGroup struct {
	Values map[string]any `json:"values"`
	Count  int64          `json:"count"`
	IDs    []string       `json:"ids"`
}

// UniqueViolationReport lists, for one unique rule, the organization's
// duplicate groups and how the rule is enforced.
type UniqueViolationReport struct {
	ErrorKey        string                 `json:"error_key"`
	Columns         []string               `json:"columns"`
	Where           map[string]any         `json:"where,omitempty"`
	Field           string                 `json:"field"`
	Enforcement     string                 `json:"enforcement"`
	DuplicateGroups int64                  `json:"duplicate_groups"`
	Groups          []UniqueViolationGroup `json:"groups"`
}

const (
	uniqueReportMaxGroups = 50
	uniqueReportMaxIDs    = 20
)

// UniqueIndexName is the deterministic name of the partial UNIQUE index that
// materializes a unique rule on table: uq_<table>_<hash of columns+where>,
// kept within Postgres' 63-byte identifier limit.
func UniqueIndexName(table string, r manifest.CrossRuleDef) string {
	if i := strings.LastIndex(table, "."); i >= 0 {
		table = table[i+1:]
	}
	h := fnv.New32a()
	h.Write([]byte(strings.Join(r.Columns, ",")))
	keys := keysOf(r.Where)
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(h, "|%s=%v", k, crossList(r.Where[k]))
	}
	suffix := fmt.Sprintf("_%08x", h.Sum32())
	if max := 63 - len("uq_") - len(suffix); len(table) > max {
		table = table[:max]
	}
	return "uq_" + table + suffix
}

// uniqueTable is what the materializer and the report need to know about the
// physical table: its schema-qualified parts and which columns it has.
type uniqueTable struct {
	schema, name string
	cols         map[string]string // column -> information_schema data_type
}

func (t uniqueTable) quoted() string {
	if t.schema == "" {
		return fmt.Sprintf("%q", t.name)
	}
	return fmt.Sprintf("%q.%q", t.schema, t.name)
}

func (t uniqueTable) has(col string) bool { _, ok := t.cols[col]; return ok }

func (t uniqueTable) isBool(col string) bool {
	switch t.cols[col] {
	case "boolean", "bool":
		return true
	}
	return false
}

func (t uniqueTable) isText(col string) bool {
	switch t.cols[col] {
	case "text", "character varying", "character", "citext":
		return true
	}
	return false
}

func splitTable(table string) (schema, name string) {
	if i := strings.LastIndex(table, "."); i >= 0 {
		return strings.Trim(table[:i], `"`), strings.Trim(table[i+1:], `"`)
	}
	return "", strings.Trim(table, `"`)
}

// loadUniqueTable reads the table's columns from information_schema
// (Postgres) or the migrator (other dialects).
func loadUniqueTable(ctx context.Context, db *gorm.DB, table string) (uniqueTable, error) {
	schema, name := splitTable(table)
	t := uniqueTable{schema: schema, name: name, cols: map[string]string{}}
	if db.Dialector.Name() != "postgres" {
		types, err := db.WithContext(ctx).Migrator().ColumnTypes(name)
		if err != nil {
			return t, err
		}
		for _, ct := range types {
			t.cols[ct.Name()] = strings.ToLower(ct.DatabaseTypeName())
		}
		return t, nil
	}
	var rows []struct {
		ColumnName string
		DataType   string
	}
	q := db.WithContext(ctx).Table("information_schema.columns").Select("column_name, data_type").Where("table_name = ?", name)
	if schema != "" {
		q = q.Where("table_schema = ?", schema)
	} else {
		q = q.Where("table_schema = current_schema()")
	}
	if err := q.Scan(&rows).Error; err != nil {
		return t, err
	}
	for _, r := range rows {
		t.cols[r.ColumnName] = r.DataType
	}
	if len(t.cols) == 0 {
		return t, fmt.Errorf("%w: table %s not found", ErrInvalidInput, table)
	}
	return t, nil
}

func sqlLiteral(v any) string {
	return "'" + strings.ReplaceAll(crossStr(v), "'", "''") + "'"
}

// uniquePredicate is the partial-index predicate of a rule: live rows, every
// key column set, and the rule's Where. It mirrors evalUniqueRule exactly (a
// row the app check skips is not in the index either). Identifiers are checked
// and quoted, values are escaped literals — DDL takes no bind parameters.
func uniquePredicate(t uniqueTable, r manifest.CrossRuleDef) (string, error) {
	var terms []string
	if t.has("deleted_at") {
		terms = append(terms, `"deleted_at" IS NULL`)
	}
	for _, c := range r.Columns {
		if !crossIdent.MatchString(c) || !t.has(c) {
			return "", fmt.Errorf("%w: unique rule %q: column %q", ErrInvalidInput, r.ErrorKey, c)
		}
		if t.isText(c) {
			terms = append(terms, fmt.Sprintf(`%q <> ''`, c))
		} else {
			terms = append(terms, fmt.Sprintf(`%q IS NOT NULL`, c))
		}
	}
	keys := keysOf(r.Where)
	sort.Strings(keys)
	for _, col := range keys {
		if !crossIdent.MatchString(col) || !t.has(col) {
			return "", fmt.Errorf("%w: unique rule %q: where column %q", ErrInvalidInput, r.ErrorKey, col)
		}
		vals := crossList(r.Where[col])
		lits := make([]string, len(vals))
		for i, v := range vals {
			lits[i] = sqlLiteral(v)
			// SQLite keeps booleans as 1/0, so a quoted 'true' would never match.
			if b, err := strconv.ParseBool(crossStr(v)); err == nil && t.isBool(col) {
				lits[i] = strings.ToUpper(strconv.FormatBool(b))
			}
		}
		terms = append(terms, fmt.Sprintf(`%q IN (%s)`, col, strings.Join(lits, ", ")))
	}
	return strings.Join(terms, " AND "), nil
}

// uniqueKeyColumns are the index / GROUP BY columns: the tenant first (the
// rule is always per organization), then the rule's columns.
func uniqueKeyColumns(t uniqueTable, r manifest.CrossRuleDef) []string {
	var cols []string
	if t.has("organization_id") {
		cols = append(cols, `"organization_id"`)
	}
	for _, c := range r.Columns {
		cols = append(cols, fmt.Sprintf("%q", c))
	}
	return cols
}

// indexValid reports whether the named index exists and whether it is valid
// (an interrupted CREATE INDEX CONCURRENTLY leaves an INVALID one behind).
func indexValid(ctx context.Context, db *gorm.DB, schema, index string) (exists, valid bool, err error) {
	ref := fmt.Sprintf("%q", index)
	if schema != "" {
		ref = fmt.Sprintf("%q.%q", schema, index)
	}
	var rows []struct{ Indisvalid bool }
	if err := db.WithContext(ctx).Raw(`SELECT indisvalid FROM pg_index WHERE indexrelid = to_regclass(?)`, ref).Scan(&rows).Error; err != nil {
		return false, false, err
	}
	if len(rows) == 0 {
		return false, false, nil
	}
	return true, rows[0].Indisvalid, nil
}

// MaterializeUniqueRules backs each unique rule of table with a partial UNIQUE
// index when — and only when — the table holds no duplicates for it. It never
// fails the caller: a table with duplicates (or any DDL error) leaves the rule
// enforced by the application check, which is logged and reported in the
// returned statuses. Idempotent: a valid index is kept, an INVALID one (from
// an interrupted CONCURRENTLY build) is dropped and rebuilt.
//
// Outside a transaction the index is built CONCURRENTLY (no write lock on the
// table); inside one it is built plainly under a SAVEPOINT so a failure does
// not poison the caller's transaction. Non-Postgres dialects report "app".
func MaterializeUniqueRules(ctx context.Context, db *gorm.DB, table string, rules []manifest.CrossRuleDef) []UniqueRuleStatus {
	var out []UniqueRuleStatus
	for _, r := range rules {
		if r.Kind != "unique" || len(r.Columns) == 0 {
			continue
		}
		st := UniqueRuleStatus{ErrorKey: r.ErrorKey, Columns: r.Columns, Index: UniqueIndexName(table, r), Enforcement: UniqueEnforcedByApp}
		if err := materializeUnique(ctx, db, table, r, &st); err != nil {
			st.Error = err.Error()
			slog.Warn("dynamic.unique_rule.materialize_failed", "table", table, "rule", r.ErrorKey, "index", st.Index, "err", err)
		} else if st.BlockedGroups > 0 {
			slog.Warn("dynamic.unique_rule.duplicates", "table", table, "rule", r.ErrorKey, "duplicate_groups", st.BlockedGroups,
				"hint", "the rule is enforced by the application check; resolve the duplicates and retry to materialize the index")
		}
		out = append(out, st)
	}
	return out
}

func materializeUnique(ctx context.Context, db *gorm.DB, table string, r manifest.CrossRuleDef, st *UniqueRuleStatus) error {
	if db.Dialector.Name() != "postgres" {
		return nil
	}
	t, err := loadUniqueTable(ctx, db, table)
	if err != nil {
		return err
	}
	exists, valid, err := indexValid(ctx, db, t.schema, st.Index)
	if err != nil {
		return err
	}
	if valid {
		st.Enforcement = UniqueEnforcedByIndex
		return nil
	}
	pred, err := uniquePredicate(t, r)
	if err != nil {
		return err
	}
	keys := strings.Join(uniqueKeyColumns(t, r), ", ")
	if err := db.WithContext(ctx).Raw(fmt.Sprintf(
		`SELECT count(*) FROM (SELECT 1 FROM %s WHERE %s GROUP BY %s HAVING count(*) > 1) g`,
		t.quoted(), pred, keys)).Scan(&st.BlockedGroups).Error; err != nil {
		return err
	}
	if st.BlockedGroups > 0 {
		return nil
	}

	idx := fmt.Sprintf("%q", st.Index)
	if t.schema != "" {
		idx = fmt.Sprintf("%q.%q", t.schema, st.Index)
	}
	_, inTx := db.Statement.ConnPool.(gorm.TxCommitter)
	concurrently := " CONCURRENTLY"
	if inTx {
		concurrently = ""
	}
	create := fmt.Sprintf(`CREATE UNIQUE INDEX%s IF NOT EXISTS %q ON %s (%s) WHERE %s`, concurrently, st.Index, t.quoted(), keys, pred)
	drop := fmt.Sprintf(`DROP INDEX%s IF EXISTS %s`, concurrently, idx)
	run := func(tx *gorm.DB) error {
		if exists {
			if err := tx.Exec(drop).Error; err != nil {
				return err
			}
		}
		if err := tx.Exec(create).Error; err != nil {
			// A duplicate written while the index was being built leaves it
			// INVALID: drop it, the rule stays enforced by the app check.
			_ = tx.Exec(drop).Error
			return err
		}
		return nil
	}
	if inTx {
		const sp = "metacore_unique_rule"
		if err := db.SavePoint(sp).Error; err != nil {
			return err
		}
		if err := run(db.WithContext(ctx)); err != nil {
			db.RollbackTo(sp)
			return err
		}
	} else if err := run(db.WithContext(ctx)); err != nil {
		return err
	}
	if _, valid, err := indexValid(ctx, db, t.schema, st.Index); err != nil {
		return err
	} else if valid {
		st.Enforcement = UniqueEnforcedByIndex
	}
	return nil
}

// uniqueViolationGroups lists one organization's duplicate groups for a rule
// (at most uniqueReportMaxGroups, each with up to uniqueReportMaxIDs ids) and
// the total number of groups.
func uniqueViolationGroups(ctx context.Context, db *gorm.DB, table string, orgID uuid.UUID, r manifest.CrossRuleDef) ([]UniqueViolationGroup, int64, error) {
	t, err := loadUniqueTable(ctx, db, table)
	if err != nil {
		return nil, 0, err
	}
	pred, err := uniquePredicate(t, r)
	if err != nil {
		return nil, 0, err
	}
	var args []any
	if t.has("organization_id") && orgID != uuid.Nil {
		pred += ` AND "organization_id" = ?`
		args = append(args, orgID)
	}
	cols := make([]string, len(r.Columns))
	for i, c := range r.Columns {
		cols[i] = fmt.Sprintf("%q", c)
	}
	group := strings.Join(cols, ", ")
	base := fmt.Sprintf(`FROM %s WHERE %s GROUP BY %s HAVING count(*) > 1`, t.quoted(), pred, group)

	var total int64
	if err := db.WithContext(ctx).Raw(`SELECT count(*) FROM (SELECT 1 `+base+`) g`, args...).Scan(&total).Error; err != nil {
		return nil, 0, err
	}
	if total == 0 {
		return []UniqueViolationGroup{}, 0, nil
	}
	// Values are selected as text so every dialect scans them the same way;
	// ids come back as one comma-joined string (string_agg / group_concat).
	sel := make([]string, len(r.Columns))
	for i, c := range r.Columns {
		sel[i] = fmt.Sprintf(`CAST(%q AS TEXT) AS %q`, c, "v_"+c)
	}
	agg := `string_agg(CAST("id" AS TEXT), ',' ORDER BY "id")`
	if db.Dialector.Name() != "postgres" {
		agg = `group_concat(CAST("id" AS TEXT), ',')`
	}
	rows, err := db.WithContext(ctx).Raw(fmt.Sprintf(`SELECT %s, count(*) AS n, %s AS ids %s ORDER BY count(*) DESC LIMIT %d`,
		strings.Join(sel, ", "), agg, base, uniqueReportMaxGroups), args...).Rows()
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	groups := []UniqueViolationGroup{}
	for rows.Next() {
		dest := make([]any, len(r.Columns)+2)
		vals := make([]*string, len(r.Columns))
		for i := range vals {
			dest[i] = &vals[i]
		}
		var n int64
		var ids string
		dest[len(r.Columns)], dest[len(r.Columns)+1] = &n, &ids
		if err := rows.Scan(dest...); err != nil {
			return nil, 0, err
		}
		g := UniqueViolationGroup{Values: map[string]any{}, Count: n, IDs: strings.Split(ids, ",")}
		for i, c := range r.Columns {
			if vals[i] != nil {
				g.Values[c] = *vals[i]
			} else {
				g.Values[c] = nil
			}
		}
		if len(g.IDs) > uniqueReportMaxIDs {
			g.IDs = g.IDs[:uniqueReportMaxIDs]
		}
		groups = append(groups, g)
	}
	return groups, total, rows.Err()
}

// modelUniqueRules resolves a model's physical table and its unique rules.
func (s *Service) modelUniqueRules(ctx context.Context, user modelbase.AuthUser, model string, action modelbase.AccessAction) (string, []manifest.CrossRuleDef, error) {
	instance, _, err := s.resolveModel(ctx, model)
	if err != nil {
		return "", nil, err
	}
	if err := s.authorize(ctx, user, model, instance, action); err != nil {
		return "", nil, err
	}
	table, err := s.tableNameFor(ctx, model, instance)
	if err != nil {
		return "", nil, err
	}
	var rules []manifest.CrossRuleDef
	if mc := s.resolveConstraints(ctx, model); mc != nil {
		for _, r := range mc.Rules {
			if r.Kind == "unique" && len(r.Columns) > 0 {
				rules = append(rules, r)
			}
		}
	}
	return table, rules, nil
}

// UniqueViolations reports, for every unique rule of model, the caller's
// organization's duplicate groups and how the rule is enforced — what a host
// shows as "there are N repeated codes, resolve them". Gated like a list.
func (s *Service) UniqueViolations(ctx context.Context, model string, user modelbase.AuthUser) ([]UniqueViolationReport, error) {
	table, rules, err := s.modelUniqueRules(ctx, user, model, modelbase.AccessList)
	if err != nil {
		return nil, err
	}
	out := []UniqueViolationReport{}
	for _, r := range rules {
		groups, total, err := uniqueViolationGroups(ctx, s.db, table, user.GetOrganizationID(), r)
		if err != nil {
			return nil, err
		}
		enf := UniqueEnforcedByApp
		if s.db.Dialector.Name() == "postgres" {
			schema, _ := splitTable(table)
			if _, valid, err := indexValid(ctx, s.db, schema, UniqueIndexName(table, r)); err == nil && valid {
				enf = UniqueEnforcedByIndex
			}
		}
		field := r.Field
		if field == "" {
			field = r.Columns[len(r.Columns)-1]
		}
		out = append(out, UniqueViolationReport{ErrorKey: r.ErrorKey, Columns: r.Columns, Where: r.Where, Field: field,
			Enforcement: enf, DuplicateGroups: total, Groups: groups})
	}
	return out, nil
}

// MaterializeUniqueIndexes retries backing model's unique rules with their
// partial UNIQUE index (see MaterializeUniqueRules) — e.g. after the
// duplicates of the report were resolved. Gated like an update. The statuses
// carry only table-wide counts, never another organization's rows.
func (s *Service) MaterializeUniqueIndexes(ctx context.Context, model string, user modelbase.AuthUser) ([]UniqueRuleStatus, error) {
	table, rules, err := s.modelUniqueRules(ctx, user, model, modelbase.AccessUpdate)
	if err != nil {
		return nil, err
	}
	out := MaterializeUniqueRules(ctx, s.db, table, rules)
	if out == nil {
		out = []UniqueRuleStatus{}
	}
	return out, nil
}

// uniqueIndexViolation turns a Postgres unique violation (23505) raised by a
// rule's materialized index — a writer that raced past the application check
// — into the same field-level *UniqueViolationError the check produces.
// Anything else comes back unchanged.
func uniqueIndexViolation(err error, table string, rules []manifest.CrossRuleDef) error {
	var pgErr *pgconn.PgError
	if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		return err
	}
	for _, r := range rules {
		if r.Kind == "unique" && len(r.Columns) > 0 && pgErr.ConstraintName == UniqueIndexName(table, r) {
			return newUniqueViolation(r)
		}
	}
	return err
}

func newUniqueViolation(r manifest.CrossRuleDef) *UniqueViolationError {
	field := r.Field
	if field == "" {
		field = r.Columns[len(r.Columns)-1]
	}
	return &UniqueViolationError{
		ErrorKey: r.ErrorKey,
		Columns:  r.Columns,
		Field:    field,
		Validation: NewValidationError().Add(field, codeDuplicate, map[string]any{
			"error_key": r.ErrorKey,
			"columns":   r.Columns,
		}),
	}
}
