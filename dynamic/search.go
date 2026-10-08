package dynamic

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/modelbase"
)

// SearchQuery is the input to Service.Search.
type SearchQuery struct {
	Model string
	// Q is the user-entered text. Empty Q returns recent rows bounded by Limit.
	Q string
	// Limit caps the number of hits. Defaults to DefaultSearchLimit.
	Limit int
	// Filters carries the client's query params. Only keys listed in
	// SearchConfig.AllowFilters are applied (equality, bound as a placeholder
	// value); every other key is ignored. Nil = no client filters.
	Filters map[string]string
}

const (
	// DefaultSearchLimit is applied when SearchQuery.Limit is 0.
	DefaultSearchLimit = 50
	// MaxSearchLimit bounds any caller-supplied Limit.
	MaxSearchLimit = 200
)

// Search performs a text search over the columns listed in SearchConfig.SearchIn
// for the given model. Nested dotted paths (e.g. "patient.user.name") are
// rewritten into LEFT JOINs. Case/accent normalization is provided by the
// configured SearchNormalizer (identity by default).
func (s *Service) Search(ctx context.Context, user modelbase.AuthUser, q SearchQuery) ([]Option, error) {
	if s.searchResolver == nil {
		return nil, ErrNoSearchConfig
	}
	instance, ok := s.lookupModel(ctx, q.Model)
	if !ok {
		return nil, ErrModelNotFound
	}
	// A model AccessPolicy gates every enumeration of its rows (no policy =
	// unchanged).
	if err := s.checkAccess(ctx, user, q.Model, instance, modelbase.AccessList); err != nil {
		return nil, err
	}

	cfg, err := s.searchResolver(ctx, q.Model, instance)
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		return nil, ErrNoSearchConfig
	}

	tableName, err := s.tableNameFor(ctx, q.Model, instance)
	if err != nil {
		return nil, err
	}

	// .Model(instance) drives column mapping; .Table pins the FROM so
	// reflect-built addon models (no TableName() method) resolve correctly.
	db := s.db.WithContext(ctx).Model(instance).Table(tableName)

	// Scope only if the root model carries an organization_id column and a user
	// is available (read-without-auth is allowed, but obviously without scope).
	// Column-based detection (see hasOrgColumn) so reflect-built addon models —
	// whose org field is "OrganizationId", not "OrganizationID" — are scoped
	// instead of silently leaking other tenants' rows.
	scoped, err := s.scopeOrDeny(db, instance, user)
	if err != nil {
		return nil, err
	}
	db = scoped

	// Compiled-model joins and fixed restriction (see SearchConfig.BaseWhere).
	for _, j := range cfg.Joins {
		db = db.Joins(j)
	}
	if cfg.BaseWhere != "" {
		db = db.Where(cfg.BaseWhere, cfg.BaseArgs...)
	}

	// Allow-listed equality filters: the column must be in the model's list AND
	// a safe identifier; the client value is always a bound placeholder.
	for _, col := range cfg.AllowFilters {
		if !safeColumn.MatchString(col) {
			continue
		}
		if v, ok := q.Filters[col]; ok && v != "" {
			db = db.Where(fmt.Sprintf("%s.%s = ?", tableName, col), v)
		}
	}

	for _, rel := range cfg.Preload {
		db = db.Preload(rel)
	}

	if q.Q != "" && len(cfg.SearchIn) > 0 {
		var conditions []string
		var values []any
		joinCache := map[string]struct{}{}

		for _, field := range cfg.SearchIn {
			var col string
			if strings.Contains(field, ".") {
				finalAlias, finalField, joins := buildNestedJoinsWith(tableName, field, func(table string) bool {
					return safeColumn.MatchString(table) && db.Migrator().HasColumn(table, "deleted_at")
				})
				for _, j := range joins {
					if _, seen := joinCache[j]; seen {
						continue
					}
					db = db.Joins(j)
					joinCache[j] = struct{}{}
				}
				col = fmt.Sprintf("%s.%s", finalAlias, finalField)
			} else {
				if !safeColumn.MatchString(field) {
					continue
				}
				col = fmt.Sprintf("%s.%s", tableName, field)
			}
			frag, val := s.matchClause(col, q.Q)
			if frag == "" {
				continue
			}
			conditions = append(conditions, frag)
			values = append(values, val)
		}
		if len(conditions) > 0 {
			db = db.Where(strings.Join(conditions, " OR "), values...)
		}
	}

	orderBy := cfg.OrderBy
	if orderBy == "" {
		orderBy = "id"
	}
	orderDir := strings.ToLower(cfg.OrderDir)
	if orderDir != "desc" {
		orderDir = "asc"
	}
	if safeColumn.MatchString(orderBy) {
		// Qualified so a SearchConfig.Joins table with the same column is not ambiguous.
		db = db.Order(fmt.Sprintf("%s.%s %s", tableName, orderBy, orderDir))
	}

	limit := q.Limit
	if limit <= 0 {
		limit = DefaultSearchLimit
	}
	if limit > MaxSearchLimit {
		limit = MaxSearchLimit
	}
	db = db.Limit(limit)

	sliceType := reflect.SliceOf(reflect.TypeOf(instance))
	resultsPtr := reflect.New(sliceType)
	if err := db.Find(resultsPtr.Interface()).Error; err != nil {
		return nil, fmt.Errorf("dynamic: search query: %w", err)
	}

	return projectSearch(resultsPtr.Elem(), *cfg), nil
}

// buildNestedJoins translates a dotted path like "patient.user.name" into
// LEFT JOIN statements plus the final (alias, column) to match against. The
// convention is: each relation name pluralises trivially to its table and is
// linked via <parent>.<relation>_id = <alias>.id.
func buildNestedJoins(rootTable, field string) (alias, column string, joins []string) {
	return buildNestedJoinsWith(rootTable, field, nil)
}

// buildNestedJoinsWith is buildNestedJoins plus soft-delete awareness: when
// hasSoftDelete reports that a joined table carries a deleted_at column, its
// ON clause gets "AND <alias>.deleted_at IS NULL" so rows whose parent was
// soft-deleted never match a nested search path. A nil hasSoftDelete keeps
// the original join shape.
func buildNestedJoinsWith(rootTable, field string, hasSoftDelete func(table string) bool) (alias, column string, joins []string) {
	parts := strings.Split(field, ".")
	if len(parts) < 2 {
		return rootTable, field, nil
	}
	current := rootTable
	for i, part := range parts[:len(parts)-1] {
		a := fmt.Sprintf("search_%s_%d", part, i)
		table := part + "s"
		on := fmt.Sprintf("%s.id = %s.%s_id", a, current, part)
		if hasSoftDelete != nil && hasSoftDelete(table) {
			on += fmt.Sprintf(" AND %s.deleted_at IS NULL", a)
		}
		joins = append(joins, fmt.Sprintf("LEFT JOIN %s AS %s ON %s", table, a, on))
		current = a
	}
	return current, parts[len(parts)-1], joins
}

func projectSearch(results reflect.Value, cfg SearchConfig) []Option {
	valueCol := cfg.Value
	if valueCol == "" {
		valueCol = "id"
	}
	labelCol := cfg.Label
	if labelCol == "" {
		labelCol = "name"
	}

	n := results.Len()
	out := make([]Option, 0, n)
	for i := 0; i < n; i++ {
		item := results.Index(i)
		if item.Kind() == reflect.Ptr {
			item = item.Elem()
		}
		opt := Option{
			ID:    fieldValue(item, valueCol),
			Value: fieldValue(item, valueCol),
			Label: fieldValue(item, labelCol),
			Name:  fieldValue(item, labelCol),
		}
		if cfg.Description != "" {
			opt.Description = fieldValue(item, cfg.Description)
		}
		if cfg.Image != "" {
			opt.Image = fieldValue(item, cfg.Image)
		}
		if cfg.Icon != "" {
			opt.Icon = fieldValue(item, cfg.Icon)
		}
		opt.Extra = extraColumnValues(item, cfg.ExtraFields)
		out = append(out, opt)
	}
	return out
}

// tableNameFor resolves the database table for a model. Resolution order:
//  1. a host-supplied TableNameResolver (required for reflect-built addon models
//     that cannot carry a Go TableName() method),
//  2. modelbase.ModelDefiner.TableName() when the instance implements it,
//  3. gorm's own schema parsing so models that rely on default pluralized
//     naming keep working without implementing the interface explicitly.
func (s *Service) tableNameFor(ctx context.Context, model string, instance any) (string, error) {
	if s.tableNameResolver != nil {
		if name, ok := s.tableNameResolver(ctx, model); ok && name != "" {
			return name, nil
		}
	}
	if def, ok := instance.(modelbase.ModelDefiner); ok {
		return def.TableName(), nil
	}
	stmt := &gorm.Statement{DB: s.db}
	if err := stmt.Parse(instance); err != nil {
		return "", fmt.Errorf("%w: resolve table name for %T: %v", ErrInvalidInput, instance, err)
	}
	return stmt.Schema.Table, nil
}
