package dynamic

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/asteby/metacore-kernel/modelbase"
)

// SearchQuery is the input to Service.Search.
type SearchQuery struct {
	Model string
	// Q is the user-entered text. Empty Q returns recent rows bounded by Limit.
	Q string
	// Limit caps the number of hits. Defaults to DefaultSearchLimit.
	Limit int
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

	cfg, err := s.searchResolver(ctx, q.Model, instance)
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		return nil, ErrNoSearchConfig
	}

	tableName, err := tableName(instance)
	if err != nil {
		return nil, err
	}

	db := s.db.WithContext(ctx).Model(instance)

	// Scope only if the root model carries an OrganizationID column and a user
	// is available (read-without-auth is allowed, but obviously without scope).
	instType := reflect.TypeOf(instance)
	if instType.Kind() == reflect.Ptr {
		instType = instType.Elem()
	}
	if _, hasOrg := instType.FieldByName("OrganizationID"); hasOrg && user != nil {
		db = s.scope.ScopeQuery(db, user)
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
				finalAlias, finalField, joins := buildNestedJoins(tableName, field)
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
		db = db.Order(fmt.Sprintf("%s %s", orderBy, orderDir))
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
// convention matches the ops/link original: each relation name pluralises
// trivially to its table and is linked via <parent>.<relation>_id = <alias>.id.
func buildNestedJoins(rootTable, field string) (alias, column string, joins []string) {
	parts := strings.Split(field, ".")
	if len(parts) < 2 {
		return rootTable, field, nil
	}
	current := rootTable
	for i, part := range parts[:len(parts)-1] {
		a := fmt.Sprintf("search_%s_%d", part, i)
		joins = append(joins, fmt.Sprintf(
			"LEFT JOIN %ss AS %s ON %s.id = %s.%s_id",
			part, a, a, current, part,
		))
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
		out = append(out, opt)
	}
	return out
}

// tableName returns the database table associated with a model. It relies on
// the ModelDefiner contract already required elsewhere in kernel/dynamic.
func tableName(instance any) (string, error) {
	def, ok := instance.(modelbase.ModelDefiner)
	if !ok {
		return "", fmt.Errorf("%w: model must implement modelbase.ModelDefiner", ErrInvalidInput)
	}
	return def.TableName(), nil
}
