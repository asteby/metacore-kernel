package dynamic

import (
	"context"
	"errors"
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
	// A missing config is only fatal when the self-options fallback can't
	// rescue it — defer the decision until after we try to synthesize one.
	if err != nil && !errors.Is(err, ErrNoOptionsConfig) {
		return nil, err
	}

	var fieldCfg FieldOptionsConfig
	found := false
	if cfg != nil {
		fieldCfg, found = cfg.Fields[q.Field]
	}
	if !found {
		// Self-referential fallback (opt-in via Config.EnableSelfOptions):
		// list the model itself as options for its identity field. This is what
		// makes a declarative `dynamic_select` (ref: "<Model>") searchable with
		// zero manifest config. It never overrides a declared field.
		synth, ok := s.selfOptionsConfig(q.Model, instance, q.Field)
		if !ok {
			if cfg == nil {
				return nil, ErrNoOptionsConfig
			}
			return nil, ErrOptionsFieldNotFound
		}
		fieldCfg = synth
	}

	switch fieldCfg.Type {
	case "static":
		return &OptionsResult{Type: "static", Options: renderStatic(fieldCfg.Options)}, nil
	case "dynamic":
		items, err := s.queryDynamicOptions(ctx, user, fieldCfg, q)
		if err != nil {
			return nil, err
		}
		if fieldCfg.LabelRef != "" {
			if err := s.enrichOptionsFromRef(ctx, user, fieldCfg, items); err != nil {
				return nil, err
			}
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

	// Pin the FROM table the same way Service.Search does: .Model(instance)
	// drives column mapping/scanning while .Table(name) resolves the real
	// table for reflect-built addon models that carry no TableName() method.
	// Without this, GORM pluralizes the anonymous struct name and the options
	// query for an addon model hits a non-existent table.
	tableName, err := s.tableNameFor(ctx, fieldCfg.Source, sourceInstance)
	if err != nil {
		return nil, err
	}
	db := s.db.WithContext(ctx).Model(sourceInstance).Table(tableName)

	// Tenant scoping: only when the source model actually carries an
	// organization_id column. Detection is COLUMN-based (json tag), not Go
	// field-name based: reflect-built addon models derive the field name from
	// the column ("organization_id" → "OrganizationId"), which a
	// FieldByName("OrganizationID") lookup misses — silently dropping tenant
	// scoping and leaking other orgs' rows into the picker. hasOrgColumn
	// matches both compiled (OrganizationID, json:"organization_id") and
	// reflect-built models.
	if user != nil && hasOrgColumn(sourceInstance) {
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
		// Match against the label column; when none is declared (a dependent
		// picker whose label comes from LabelRef, not a column on Source) match
		// against the value column so q still narrows the candidate ids.
		labelCol := fieldCfg.Label
		if labelCol == "" {
			labelCol = fieldCfg.Value
		}
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
		// Prefer the label column; fall back to the value column (a dependent
		// picker whose label is resolved from LabelRef has no label column ON
		// the Source — e.g. a stock ledger has no "name"), then to "name".
		orderBy = fieldCfg.Label
		if orderBy == "" {
			orderBy = fieldCfg.Value
		}
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

// enrichOptionsFromRef fills in option labels by resolving them from a RELATED
// model (fieldCfg.LabelRef) keyed by the option's value (a foreign id). It is
// the relational-label primitive behind a dependent picker whose Source is a
// scope/ledger table (e.g. stock rows scoped by warehouse) but whose label is
// the name of ANOTHER record (the product). It runs ONLY for options whose
// projected label is missing or equal to their value — i.e. the value is a raw
// id with no usable label yet — so a Source that already projects a real label
// is left untouched.
//
// The lookup is a SINGLE batched `WHERE <pk> IN (...)` query (no N+1), mirroring
// the image-enrich batch pattern. The target table and label column are derived
// generically from LabelRef's model metadata (the same name-like column
// preference EnableSelfOptions uses) — no domain model name is hardcoded.
func (s *Service) enrichOptionsFromRef(ctx context.Context, user modelbase.AuthUser, fieldCfg FieldOptionsConfig, items []Option) error {
	// Collect the ids that still need a label: blank label, or label == value
	// (the projection fell back to the id). De-duplicate so the IN list is tight.
	idSet := map[string]struct{}{}
	ids := make([]any, 0, len(items))
	for i := range items {
		if !optionNeedsLabel(items[i]) {
			continue
		}
		key := fmt.Sprint(items[i].Value)
		if key == "" {
			continue
		}
		if _, dup := idSet[key]; dup {
			continue
		}
		idSet[key] = struct{}{}
		ids = append(ids, items[i].Value)
	}
	if len(ids) == 0 {
		return nil
	}

	refInstance, ok := s.lookupModel(ctx, fieldCfg.LabelRef)
	if !ok {
		return ErrSourceModelNotFound
	}
	tableName, err := s.tableNameFor(ctx, fieldCfg.LabelRef, refInstance)
	if err != nil {
		return err
	}
	labelCol := deriveLabelColumn(refInstance)
	if labelCol == "" || !safeColumn.MatchString(labelCol) {
		// No usable label column on the ref model — nothing to enrich with.
		return nil
	}

	// Pin Model+Table the same way queryDynamicOptions does so reflect-built
	// addon models resolve to their real table. Resolve labels by primary key
	// (id) regardless of org scope: the value came from a row the caller can
	// already see, and a label lookup by id must not be filtered out by tenant
	// scoping (which would blank legitimate labels).
	db := s.db.WithContext(ctx).Model(refInstance).Table(tableName).
		Where("id IN ?", ids)

	sliceType := reflect.SliceOf(reflect.TypeOf(refInstance))
	resultsPtr := reflect.New(sliceType)
	if err := db.Find(resultsPtr.Interface()).Error; err != nil {
		return fmt.Errorf("dynamic: label_ref query: %w", err)
	}

	// Build id → label from the fetched ref rows.
	labelByID := map[string]any{}
	results := resultsPtr.Elem()
	for i := 0; i < results.Len(); i++ {
		row := results.Index(i)
		if row.Kind() == reflect.Ptr {
			row = row.Elem()
		}
		id := fieldValue(row, "id")
		if id == nil {
			continue
		}
		labelByID[fmt.Sprint(id)] = fieldValue(row, labelCol)
	}

	// Fill labels for the options that needed one.
	for i := range items {
		if !optionNeedsLabel(items[i]) {
			continue
		}
		if lbl, ok := labelByID[fmt.Sprint(items[i].Value)]; ok && lbl != nil && lbl != "" {
			items[i].Label = lbl
			items[i].Name = lbl
		}
	}
	return nil
}

// optionNeedsLabel reports whether an option still lacks a human label: its
// label is empty, or it merely echoes the value (the projection fell back to the
// id). Such options are the ones enrichOptionsFromRef resolves from LabelRef.
func optionNeedsLabel(o Option) bool {
	if o.Label == nil || o.Label == "" {
		return true
	}
	return fmt.Sprint(o.Label) == fmt.Sprint(o.Value)
}

// selfOptionsLabelPreference is the ordered list of column names tried, in
// priority order, as the human-readable label of a self-referential options
// listing. The first column the model actually has wins. Domain-agnostic — it
// favours the columns a user most expects to recognise a record by.
var selfOptionsLabelPreference = []string{
	"name", "title", "label", "display_name", "full_name",
	"code", "number", "reference", "slug", "email", "description",
}

// selfOptionsConfig synthesizes a self-referential dynamic options config so a
// field's "options" are simply the rows of the model itself. It is the engine
// behind the declarative `dynamic_select` picker: `ref: "<Model>"` resolves to
// GET /api/options/<Model>?field=id and lists <Model>'s rows as
// {value: <id>, label: <name-like column>} — a searchable cross-module FK
// lookup with zero manifest configuration.
//
// Returns ok=false (so the caller surfaces the normal not-found error) when the
// fallback is disabled, the requested field is not the model's identity field
// (self-listing only makes sense when the model itself is being picked), or no
// usable label column can be derived.
func (s *Service) selfOptionsConfig(model string, instance any, field string) (FieldOptionsConfig, bool) {
	if !s.selfOptions || field == "" {
		return FieldOptionsConfig{}, false
	}
	// Only the identity field is well-defined for self-listing. A query for an
	// unconfigured non-id field (e.g. a FK whose target wasn't declared) must
	// still fail loudly so the manifest bug surfaces, rather than silently
	// listing the wrong model.
	if field != "id" {
		return FieldOptionsConfig{}, false
	}
	label := deriveLabelColumn(instance)
	if label == "" {
		return FieldOptionsConfig{}, false
	}
	return FieldOptionsConfig{
		Type:   "dynamic",
		Source: model,
		Value:  field,
		Label:  label,
	}, true
}

// deriveLabelColumn picks the best display column for a self-referential
// options listing by walking the model's columns against
// selfOptionsLabelPreference. Falls back to "id" (label == value) so the picker
// still functions for a model with no name-like column.
func deriveLabelColumn(instance any) string {
	cols := structColumnSet(instance)
	for _, c := range selfOptionsLabelPreference {
		if _, ok := cols[c]; ok {
			return c
		}
	}
	return "id"
}

// structColumnSet collects the column names of a (possibly pointer, possibly
// embedded) struct, reading the json tag (the column convention used across the
// kernel) and falling back to a snake_case of the field name when untagged.
func structColumnSet(instance any) map[string]struct{} {
	out := map[string]struct{}{}
	t := reflect.TypeOf(instance)
	for t != nil && t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		return out
	}
	var walk func(reflect.Type)
	walk = func(t reflect.Type) {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.Anonymous {
				ft := f.Type
				for ft.Kind() == reflect.Ptr {
					ft = ft.Elem()
				}
				if ft.Kind() == reflect.Struct {
					walk(ft)
				}
				continue
			}
			if name := columnNameFromField(f); name != "" && name != "-" {
				out[name] = struct{}{}
			}
		}
	}
	walk(t)
	return out
}

// hasOrgColumn reports whether a model carries an organization_id column —
// the signal that it is tenant-scoped and its queries must be filtered by the
// caller's organization. Column-based (json tag) so it is correct for both
// compiled models and reflect-built addon models.
func hasOrgColumn(instance any) bool {
	_, ok := structColumnSet(instance)["organization_id"]
	return ok
}

func columnNameFromField(f reflect.StructField) string {
	if tag := f.Tag.Get("json"); tag != "" {
		if c := strings.Split(tag, ",")[0]; c != "" {
			return c
		}
	}
	return toSnakeASCII(f.Name)
}

func toSnakeASCII(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r + ('a' - 'A'))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
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
	// JSON-tag fallback: resolves columns whose Go field name doesn't
	// PascalCase cleanly — most importantly "id" → BaseUUIDModel.ID (json:"id"),
	// which neither FieldByName("id") nor FieldByName(toPascal("id")="Id")
	// matches because Go's reflect lookup is case-sensitive. Walks promoted
	// fields from embedded structs too.
	if f := fieldByJSONTag(v, name); f.IsValid() {
		return f.Interface()
	}
	return nil
}

// fieldByJSONTag finds the struct field (including fields promoted from
// embedded structs) whose `json` tag names the given column, and returns its
// value. Returns the zero reflect.Value when no field matches.
func fieldByJSONTag(v reflect.Value, col string) reflect.Value {
	if v.Kind() != reflect.Struct {
		return reflect.Value{}
	}
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Anonymous {
			fv := v.Field(i)
			for fv.Kind() == reflect.Ptr {
				if fv.IsNil() {
					break
				}
				fv = fv.Elem()
			}
			if fv.Kind() == reflect.Struct {
				if got := fieldByJSONTag(fv, col); got.IsValid() {
					return got
				}
			}
			continue
		}
		if name := strings.Split(f.Tag.Get("json"), ",")[0]; name == col {
			return v.Field(i)
		}
	}
	return reflect.Value{}
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
