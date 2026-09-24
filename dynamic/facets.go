package dynamic

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/asteby/metacore-kernel/modelbase"
	"gorm.io/gorm"
)

// FacetsQuery is the input to Service.Facets.
type FacetsQuery struct {
	// Model key (e.g. "github_issues"). Must resolve via the model resolver.
	Model string
	// Field is the column whose distinct values are being enumerated. It must
	// exist on the model and pass safeColumn, OR be a jsonb bag path
	// (`product_specs.dot`, `fiscal_data.rfc`) whose bag column is jsonb on
	// the model and whose key segment is a safe identifier.
	Field string
	// Q optionally narrows the returned values with an ILIKE/unaccent match
	// (same dialect mechanism Service.Options uses).
	Q string
	// Limit caps the number of buckets returned. Falls back to
	// DefaultOptionsLimit when zero and is clamped to MaxOptionsLimit.
	Limit int
}

// FacetBucket is one distinct value of a column together with the number of
// (org/branch-scoped) rows that carry it. It powers a column filter that offers
// the values that actually exist in the table instead of a free-text box.
type FacetBucket struct {
	Value string `json:"value"`
	Label string `json:"label"`
	Count int64  `json:"count"`
}

// Facets returns the distinct values of a text (or castable) column together
// with their row counts, scoped to the caller's organization/branch. It is the
// engine behind a "pick from existing values" column filter: the frontend loads
// the real repos / assignees / statuses present in the data rather than only
// offering a "contains…" text match.
//
// Plain columns:
//
//	SELECT <col> AS value, COUNT(*) AS count
//	  FROM <table>
//	 WHERE <org/branch scope> AND <col> IS NOT NULL [AND <col> <> '']
//	   [AND <col> ILIKE <q>]
//	 GROUP BY <col>
//	 ORDER BY count DESC, value ASC
//	 LIMIT <n>
//
// JSONB bag paths (`product_specs.dot`, UI-N05 / products_tires): the expression
// is dialect-aware (`col->>'key'` on Postgres, `json_extract(col, '$.key')` on
// SQLite). The `<> ''` empty-string guard always applies because both operators
// yield text.
//
// Table resolution mirrors queryDynamicOptions (.Model(instance).Table(name))
// so reflect-built addon models resolve to their real table, and tenant scoping
// fires only when the model carries an organization_id column (hasOrgColumn).
func (s *Service) Facets(ctx context.Context, user modelbase.AuthUser, q FacetsQuery) ([]FacetBucket, error) {
	if q.Field == "" {
		return nil, ErrFieldRequired
	}
	instance, ok := s.lookupModel(ctx, q.Model)
	if !ok {
		return nil, ErrModelNotFound
	}
	expr, emptyGuard, err := resolveFacetExpr(s.db, instance, q.Field)
	if err != nil {
		return nil, err
	}

	// Pin the FROM table explicitly (like queryDynamicOptions) so reflect-built
	// addon models (no TableName() method) hit the real table. We deliberately
	// do NOT chain .Model(instance) here: options.go needs it because it scans
	// INTO the model struct, but facets scans into facetRow — a .Model would make
	// GORM map the projected value/count columns against the model's schema and
	// fail. .Table alone resolves the FROM, which is all this query needs.
	tableName, err := s.tableNameFor(ctx, q.Model, instance)
	if err != nil {
		return nil, err
	}
	db := s.db.WithContext(ctx).Table(tableName)

	// Tenant scoping: only when the model actually carries an organization_id
	// column. Column-based detection (hasOrgColumn) is correct for both compiled
	// and reflect-built addon models.
	if user != nil && hasOrgColumn(instance) {
		db = s.scope.ScopeQuery(db, user)
	}
	// facetRow scans bypass the model schema, so add the soft-delete filter by
	// hand — facet counts must match the list the chips filter.
	db = scopeSoftDelete(db, instance)

	// Exclude empty buckets: NULL always, and the empty string when the
	// expression yields text (plain text columns + every jsonb ->> path).
	db = db.Where(fmt.Sprintf("%s IS NOT NULL", expr))
	if emptyGuard {
		db = db.Where(fmt.Sprintf("%s <> ''", expr))
	}

	// Q: narrow the values with the configured SearchMatchClause so the same
	// dialect override used for Service.Options/Search (unaccent/ILIKE on
	// Postgres) applies here. Escape % and _ exactly like queryDynamicOptions.
	if q.Q != "" {
		escaped := strings.NewReplacer("%", `\%`, "_", `\_`).Replace(q.Q)
		frag, val := s.matchClause(expr, escaped)
		if frag != "" {
			db = db.Where(frag, val)
		}
	}

	limit := q.Limit
	if limit <= 0 {
		limit = DefaultOptionsLimit
	}
	if limit > MaxOptionsLimit {
		limit = MaxOptionsLimit
	}

	db = db.
		Select(fmt.Sprintf("%s AS value, COUNT(*) AS count", expr)).
		Group(expr).
		Order("count DESC, value ASC").
		Limit(limit)

	var rows []facetRow
	if err := db.Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("dynamic: facets query: %w", err)
	}

	out := make([]FacetBucket, 0, len(rows))
	for _, r := range rows {
		out = append(out, FacetBucket{Value: r.Value, Label: r.Value, Count: r.Count})
	}
	return out, nil
}

// resolveFacetExpr maps a FacetsQuery.Field onto a SQL expression safe to
// splice into SELECT/WHERE/GROUP BY. Plain columns must exist on the model.
// Dotted paths (`bag.key`) require the bag to be a jsonb column and both
// segments to pass safeColumn — anything else is ErrInvalidInput (400).
func resolveFacetExpr(db *gorm.DB, instance any, field string) (expr string, emptyGuard bool, err error) {
	if safeColumn.MatchString(field) {
		if _, ok := structColumnSet(instance)[field]; !ok {
			return "", false, ErrOptionsFieldNotFound
		}
		return field, columnIsText(instance, field), nil
	}
	bag, key, ok := splitFacetPath(field)
	if !ok || !safeColumn.MatchString(bag) || !safeColumn.MatchString(key) {
		return "", false, fmt.Errorf("%w: unsafe field name %q", ErrInvalidInput, field)
	}
	if _, exists := structColumnSet(instance)[bag]; !exists {
		return "", false, ErrOptionsFieldNotFound
	}
	if !columnIsJSONB(instance, bag) {
		return "", false, fmt.Errorf("%w: %q is not a jsonb bag", ErrInvalidInput, bag)
	}
	return jsonbTextExpr(db, bag, key), true, nil
}

// splitFacetPath splits "bag.key" on the first dot. Nested paths
// (a.b.c) are rejected — extension bags are one level deep.
func splitFacetPath(field string) (bag, key string, ok bool) {
	i := strings.IndexByte(field, '.')
	if i <= 0 || i == len(field)-1 {
		return "", "", false
	}
	if strings.ContainsRune(field[i+1:], '.') {
		return "", "", false
	}
	return field[:i], field[i+1:], true
}

// jsonbTextExpr returns a dialect-safe SQL expression that extracts a jsonb
// object key as text. key must already pass safeColumn.
func jsonbTextExpr(db *gorm.DB, col, key string) string {
	if db != nil && db.Dialector != nil && db.Dialector.Name() == "sqlite" {
		return fmt.Sprintf("json_extract(%s, '$.%s')", col, key)
	}
	return fmt.Sprintf("%s->>'%s'", col, key)
}

// facetRow is the scan target for the grouped facets query. Value is scanned as
// a string so the {value,label} contract stays uniform regardless of the
// column's SQL type — database/sql coerces numeric/bool columns to their string
// form and decodes text columns returned as bytes.
type facetRow struct {
	Value string `gorm:"column:value"`
	Count int64  `gorm:"column:count"`
}

// columnIsText reports whether the model's column maps to a string-kind Go
// field — the signal that the `<col> <> ''` empty-string guard is safe to add.
// Column-based (json tag) so it is correct for reflect-built addon models whose
// Go field names are derived from the column.
func columnIsText(instance any, col string) bool {
	ft, ok := columnFieldType(instance, col)
	if !ok {
		return false
	}
	for ft.Kind() == reflect.Ptr {
		ft = ft.Elem()
	}
	return ft.Kind() == reflect.String
}

// columnIsJSONB reports whether the model's column is the jsonb bag type
// (JSONBValue) used by reflect-built addon models and the fiscal_data /
// product_specs extension bags.
func columnIsJSONB(instance any, col string) bool {
	ft, ok := columnFieldType(instance, col)
	if !ok {
		return false
	}
	for ft.Kind() == reflect.Ptr {
		ft = ft.Elem()
	}
	return ft == reflect.TypeOf(JSONBValue{})
}

func columnFieldType(instance any, col string) (reflect.Type, bool) {
	t := reflect.TypeOf(instance)
	for t != nil && t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		return nil, false
	}
	var walk func(reflect.Type) (reflect.Type, bool)
	walk = func(t reflect.Type) (reflect.Type, bool) {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.Anonymous {
				ft := f.Type
				for ft.Kind() == reflect.Ptr {
					ft = ft.Elem()
				}
				if ft.Kind() == reflect.Struct {
					if got, ok := walk(ft); ok {
						return got, true
					}
				}
				continue
			}
			if columnNameFromField(f) == col {
				return f.Type, true
			}
		}
		return nil, false
	}
	return walk(t)
}
