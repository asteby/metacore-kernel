package dynamic

import (
	"context"
	"fmt"
	"reflect"
	"regexp"
	"strings"

	"github.com/asteby/metacore-kernel/modelbase"
)

// OptionsQuery is the input to Service.Options.
type OptionsQuery struct {
	// Model key (e.g. "product", "user"). Must resolve via modelbase.Get.
	Model string
	// Field name on Model whose options are being requested.
	Field string
	// Q is a free-text filter applied to the label column of dynamic options.
	Q string
	// FilterValue is passed through the configured FilterBy column on dynamic
	// options (e.g. scoping products to a category passed in the query).
	FilterValue string
	// Limit caps the number of rows returned for dynamic options. Falls back
	// to DefaultOptionsLimit when zero, and is clamped to MaxOptionsLimit.
	Limit int
	// Offset shifts the window for pagination.
	Offset int
}

// OptionsResult is the output of Service.Options.
type OptionsResult struct {
	// Type mirrors FieldOptionsConfig.Type so the caller can render static vs
	// dynamic results differently without re-reading the config.
	Type    string   `json:"type"`
	Options []Option `json:"options"`
}

const (
	// DefaultOptionsLimit is applied when OptionsQuery.Limit is 0.
	DefaultOptionsLimit = 50
	// MaxOptionsLimit is the upper bound enforced regardless of the caller.
	MaxOptionsLimit = 200
)

// safeColumn matches identifiers we are willing to splice into SQL verbatim.
var safeColumn = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// Options returns the options a field exposes — either the configured static
// list or a query against a related model. Apps wire the mapping from model
// to config via Config.OptionsConfigResolver; wiring is required before this
// call can succeed.
func (s *Service) Options(ctx context.Context, user modelbase.AuthUser, q OptionsQuery) (*OptionsResult, error) {
	if q.Field == "" {
		return nil, ErrFieldRequired
	}
	if s.optsResolver == nil {
		return nil, ErrNoOptionsConfig
	}
	instance, ok := s.lookupModel(ctx, q.Model)
	if !ok {
		return nil, ErrModelNotFound
	}

	cfg, err := s.optsResolver(ctx, q.Model, instance)
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		return nil, ErrNoOptionsConfig
	}
	fieldCfg, ok := cfg.Fields[q.Field]
	if !ok {
		return nil, ErrOptionsFieldNotFound
	}

	switch fieldCfg.Type {
	case "static":
		return &OptionsResult{Type: "static", Options: renderStatic(fieldCfg.Options)}, nil
	case "dynamic":
		items, err := s.queryDynamicOptions(ctx, user, fieldCfg, q)
		if err != nil {
			return nil, err
		}
		return &OptionsResult{Type: "dynamic", Options: items}, nil
	default:
		return nil, fmt.Errorf("%w: unknown field type %q", ErrInvalidInput, fieldCfg.Type)
	}
}

func renderStatic(opts []StaticOption) []Option {
	out := make([]Option, 0, len(opts))
	for _, o := range opts {
		item := Option{
			ID:    o.Value,
			Value: o.Value,
			Label: o.Label,
			Name:  o.Label,
		}
		if o.Icon != "" {
			item.Icon = o.Icon
		}
		if o.Color != "" {
			item.Color = o.Color
		}
		out = append(out, item)
	}
	return out
}

func (s *Service) queryDynamicOptions(ctx context.Context, user modelbase.AuthUser, fieldCfg FieldOptionsConfig, q OptionsQuery) ([]Option, error) {
	if fieldCfg.Source == "" {
		return nil, fmt.Errorf("%w: source required for dynamic options", ErrInvalidInput)
	}
	sourceInstance, ok := s.lookupModel(ctx, fieldCfg.Source)
	if !ok {
		return nil, ErrSourceModelNotFound
	}

	db := s.db.WithContext(ctx).Model(sourceInstance)

	// Tenant scoping: only when the source model actually has organization_id.
	sourceType := reflect.TypeOf(sourceInstance)
	if sourceType.Kind() == reflect.Ptr {
		sourceType = sourceType.Elem()
	}
	if _, hasOrg := sourceType.FieldByName("OrganizationID"); hasOrg && user != nil {
		db = s.scope.ScopeQuery(db, user)
	}

	// FilterBy: optional predicate driven by ?filter_value=.
	if fieldCfg.FilterBy != "" && q.FilterValue != "" && safeColumn.MatchString(fieldCfg.FilterBy) {
		db = db.Where(fmt.Sprintf("%s = ?", fieldCfg.FilterBy), q.FilterValue)
	}

	// Q: label-column filter. Uses the configured SearchMatchClause so the
	// same dialect override used for Service.Search (e.g. unaccent/ILIKE on
	// Postgres) also applies to the options endpoint.
	if q.Q != "" {
		labelCol := fieldCfg.Label
		if labelCol == "" {
			labelCol = "name"
		}
		if safeColumn.MatchString(labelCol) {
			escaped := strings.NewReplacer("%", `\%`, "_", `\_`).Replace(q.Q)
			frag, val := s.matchClause(labelCol, escaped)
			if frag != "" {
				db = db.Where(frag, val)
			}
		}
	}

	orderBy := fieldCfg.OrderBy
	if orderBy == "" {
		orderBy = fieldCfg.Label
		if orderBy == "" {
			orderBy = "name"
		}
	}
	orderDir := strings.ToLower(fieldCfg.OrderDir)
	if orderDir != "desc" {
		orderDir = "asc"
	}
	if safeColumn.MatchString(orderBy) {
		db = db.Order(fmt.Sprintf("%s %s", orderBy, orderDir))
	}

	limit := q.Limit
	if limit <= 0 {
		limit = DefaultOptionsLimit
	}
	if limit > MaxOptionsLimit {
		limit = MaxOptionsLimit
	}
	db = db.Limit(limit)
	if q.Offset > 0 {
		db = db.Offset(q.Offset)
	}

	sliceType := reflect.SliceOf(reflect.TypeOf(sourceInstance))
	resultsPtr := reflect.New(sliceType)
	if err := db.Find(resultsPtr.Interface()).Error; err != nil {
		return nil, fmt.Errorf("dynamic: options query: %w", err)
	}

	return projectOptions(resultsPtr.Elem(), fieldCfg), nil
}

func projectOptions(results reflect.Value, cfg FieldOptionsConfig) []Option {
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
		if v := fieldValue(item, "Color"); v != nil && v != "" {
			opt.Color = v
		}
		out = append(out, opt)
	}
	return out
}

// fieldValue reads a struct field by name or by JSON/db tag. Used to project
// dynamic-options / search results without forcing apps to name their fields
// to match the config verbatim.
func fieldValue(v reflect.Value, name string) any {
	if !v.IsValid() {
		return nil
	}
	if v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return nil
	}
	if f := v.FieldByName(name); f.IsValid() {
		return f.Interface()
	}
	// PascalCase fallback — config often uses snake_case from JSON.
	pascal := toPascal(name)
	if pascal != name {
		if f := v.FieldByName(pascal); f.IsValid() {
			return f.Interface()
		}
	}
	return nil
}

func toPascal(s string) string {
	var b strings.Builder
	upper := true
	for _, r := range s {
		if r == '_' || r == '-' {
			upper = true
			continue
		}
		if upper {
			b.WriteRune(toUpperASCII(r))
			upper = false
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func toUpperASCII(r rune) rune {
	if r >= 'a' && r <= 'z' {
		return r - ('a' - 'A')
	}
	return r
}
