package dynamic

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/asteby/metacore-kernel/modelbase"
)

// FacetsQuery is the input to Service.Facets.
type FacetsQuery struct {
	// Model key (e.g. "github_issues"). Must resolve via the model resolver.
	Model string
	// Field is the column whose distinct values are being enumerated. It must
	// exist on the model and pass safeColumn.
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
// The query is:
//
//	SELECT <col> AS value, COUNT(*) AS count
//	  FROM <table>
//	 WHERE <org/branch scope> AND <col> IS NOT NULL [AND <col> <> '']
//	   [AND <col> ILIKE <q>]
//	 GROUP BY <col>
//	 ORDER BY count DESC, value ASC
//	 LIMIT <n>
//
// The `<> ''` guard is applied only for text columns (a non-text column would
// raise a type error on some dialects); non-text values are stringified in Go so
// the {value,label} contract stays uniform. Table resolution mirrors
// queryDynamicOptions (.Model(instance).Table(name)) so reflect-built addon
// models resolve to their real table, and tenant scoping fires only when the
// model carries an organization_id column (hasOrgColumn).
func (s *Service) Facets(ctx context.Context, user modelbase.AuthUser, q FacetsQuery) ([]FacetBucket, error) {
	if q.Field == "" {
		return nil, ErrFieldRequired
	}
	if !safeColumn.MatchString(q.Field) {
		return nil, fmt.Errorf("%w: unsafe field name %q", ErrInvalidInput, q.Field)
	}
	instance, ok := s.lookupModel(ctx, q.Model)
	if !ok {
		return nil, ErrModelNotFound
	}
	// The column must be part of the model's structure. Mirrors the options
	// "field not configured" semantics (mapped to 404 by the handler).
	if _, ok := structColumnSet(instance)[q.Field]; !ok {
		return nil, ErrOptionsFieldNotFound
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

	// Exclude empty buckets: NULL always, and the empty string only for text
	// columns (a `<> ''` predicate against e.g. an integer column errors on
	// Postgres).
	db = db.Where(fmt.Sprintf("%s IS NOT NULL", q.Field))
	if columnIsText(instance, q.Field) {
		db = db.Where(fmt.Sprintf("%s <> ''", q.Field))
	}

	// Q: narrow the values with the configured SearchMatchClause so the same
	// dialect override used for Service.Options/Search (unaccent/ILIKE on
	// Postgres) applies here. Escape % and _ exactly like queryDynamicOptions.
	if q.Q != "" {
		escaped := strings.NewReplacer("%", `\%`, "_", `\_`).Replace(q.Q)
		frag, val := s.matchClause(q.Field, escaped)
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
		Select(fmt.Sprintf("%s AS value, COUNT(*) AS count", q.Field)).
		Group(q.Field).
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
	t := reflect.TypeOf(instance)
	for t != nil && t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		return false
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
	ft, ok := walk(t)
	if !ok {
		return false
	}
	for ft.Kind() == reflect.Ptr {
		ft = ft.Elem()
	}
	return ft.Kind() == reflect.String
}
