package v3

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	documentFormKeyRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	extraColumnRe     = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	// reservedOptionKeys are the keys an /options entry already serializes; an
	// extra column may not reuse them.
	reservedOptionKeys = map[string]bool{
		"id": true, "value": true, "label": true, "name": true,
		"description": true, "image": true, "color": true, "icon": true,
	}
)

// createModelRe: a model key, optionally addon-qualified ("customers.Invoice").
var createModelRe = regexp.MustCompile(`^([a-z][a-z0-9_]*\.)?[A-Za-z][A-Za-z0-9_]*$`)

// kernelOwnedColumns are columns the kernel adds to every model, so a manifest
// may name them without declaring them.
var kernelOwnedColumns = map[string]bool{
	"organization_id": true, "created_at": true, "updated_at": true, "deleted_at": true,
	"created_by_id": true, "updated_by_id": true, "deleted_by_id": true,
}

// isOptionScalar reports whether v is a JSON scalar the SDK can compare
// (string, number, boolean). JSON numbers decode to float64.
func isOptionScalar(v any) bool {
	switch v.(type) {
	case string, bool, float64, float32, int, int64, int32, uint, uint64, uint32:
		return true
	}
	return false
}

// validateOptionFilter checks one field/column option_filter. where is the
// path of the owning field (e.g. "models[0].columns[2]").
func validateOptionFilter(where string, f OptionFilter) []string {
	var errs []string
	for i, r := range f {
		rw := fmt.Sprintf("%s.option_filter[%d]", where, i)
		if strings.TrimSpace(r.Field) == "" {
			errs = append(errs, fmt.Sprintf("%s.field is empty", rw))
		}
		hasOp := false
		if r.Equals != nil {
			hasOp = true
			if !isOptionScalar(r.Equals) {
				errs = append(errs, fmt.Sprintf("%s.equals must be a string, number or boolean", rw))
			}
		}
		if r.NotEquals != nil {
			hasOp = true
			if !isOptionScalar(r.NotEquals) {
				errs = append(errs, fmt.Sprintf("%s.not_equals must be a string, number or boolean", rw))
			}
		}
		for _, l := range []struct {
			name string
			vals []any
		}{{"in", r.In}, {"not_in", r.NotIn}} {
			if l.vals == nil {
				continue
			}
			hasOp = true
			if len(l.vals) == 0 {
				errs = append(errs, fmt.Sprintf("%s.%s is empty", rw, l.name))
			}
			for _, v := range l.vals {
				if !isOptionScalar(v) {
					errs = append(errs, fmt.Sprintf("%s.%s entries must be strings, numbers or booleans", rw, l.name))
					break
				}
			}
		}
		if !hasOp {
			errs = append(errs, fmt.Sprintf("%s declares no operator (equals, not_equals, in or not_in)", rw))
		}
	}
	return errs
}

// validateExtraColumns checks DynamicOptions.extra_columns. ownCols, when
// non-nil, is the column set of Source if it is a model of this manifest.
func validateExtraColumns(where string, d *DynamicOptions, ownCols map[string]struct{}) []string {
	if d == nil || len(d.ExtraColumns) == 0 {
		return nil
	}
	var errs []string
	seen := map[string]struct{}{}
	for i, c := range d.ExtraColumns {
		cw := fmt.Sprintf("%s.options.extra_columns[%d]", where, i)
		switch {
		case !extraColumnRe.MatchString(c):
			errs = append(errs, fmt.Sprintf("%s %q is not a lowercase snake_case column name", cw, c))
			continue
		case reservedOptionKeys[c]:
			errs = append(errs, fmt.Sprintf("%s %q collides with a key every option already carries (id, value, label, name, description, image, color, icon)", cw, c))
			continue
		}
		if _, dup := seen[c]; dup {
			errs = append(errs, fmt.Sprintf("%s %q is duplicated", cw, c))
		}
		seen[c] = struct{}{}
		if ownCols != nil && !kernelOwnedColumns[c] {
			if _, ok := ownCols[c]; !ok {
				errs = append(errs, fmt.Sprintf("%s %q is not a column of source %q", cw, c, d.Source))
			}
		}
	}
	return errs
}

// validateActionFieldPickerMeta checks option_filter / extra_columns on an
// action field and recursively on its item_fields.
func validateActionFieldPickerMeta(where string, f ActionField, colsByModel map[string]map[string]struct{}) []string {
	errs := validateOptionFilter(where, f.OptionFilter)
	if f.Options.Dynamic != nil {
		errs = append(errs, validateExtraColumns(where, f.Options.Dynamic, colsByModel[f.Options.Dynamic.Source])...)
	}
	for ii, it := range f.ItemFields {
		errs = append(errs, validateActionFieldPickerMeta(fmt.Sprintf("%s.item_fields[%d]", where, ii), it, colsByModel)...)
	}
	return errs
}

// validateDocumentForms checks a model's document_forms block. ownCols is the
// model's column set (type_field must be one of them).
func validateDocumentForms(where string, df *DocumentForms, ownCols map[string]struct{}, colsByModel map[string]map[string]struct{}) []string {
	if df == nil {
		return nil
	}
	var errs []string
	if df.TypeField != "" {
		if _, ok := ownCols[df.TypeField]; !ok {
			errs = append(errs, fmt.Sprintf("%s.type_field %q is not a column of the model", where, df.TypeField))
		}
	}
	if df.LinesField != "" && !documentFormKeyRe.MatchString(df.LinesField) {
		errs = append(errs, fmt.Sprintf("%s.lines_field %q is not a lowercase snake_case name", where, df.LinesField))
	}
	if len(df.Types) == 0 {
		errs = append(errs, fmt.Sprintf("%s.types is empty (declare at least one document type)", where))
	}
	keys := map[string]struct{}{}
	values := map[string]string{}
	for ti, t := range df.Types {
		tw := fmt.Sprintf("%s.types[%d]", where, ti)
		if !documentFormKeyRe.MatchString(t.Key) {
			errs = append(errs, fmt.Sprintf("%s.key %q is not a lowercase snake_case name", tw, t.Key))
		} else if _, dup := keys[t.Key]; dup {
			errs = append(errs, fmt.Sprintf("%s.key %q is duplicated", tw, t.Key))
		}
		keys[t.Key] = struct{}{}
		if strings.TrimSpace(t.Label) == "" {
			errs = append(errs, fmt.Sprintf("%s.label is empty", tw))
		}
		eff := t.Value
		if eff == "" {
			eff = t.Key
		}
		if prev, dup := values[eff]; dup && df.TypeField != "" {
			errs = append(errs, fmt.Sprintf("%s writes value %q into type_field, already written by type %q", tw, eff, prev))
		}
		values[eff] = t.Key
		fkeys := map[string]struct{}{}
		for fi, f := range t.Fields {
			fw := fmt.Sprintf("%s.fields[%d]", tw, fi)
			if strings.TrimSpace(f.Key) == "" {
				errs = append(errs, fmt.Sprintf("%s.key is empty", fw))
			} else if _, dup := fkeys[f.Key]; dup {
				errs = append(errs, fmt.Sprintf("%s.key %q is duplicated within the type", fw, f.Key))
			}
			fkeys[f.Key] = struct{}{}
			if f.Options.Len() > 0 {
				errs = append(errs, validateOptionWhen(fw, f.DependsOn, f.Options.Static)...)
			}
			errs = append(errs, validateActionFieldPickerMeta(fw, f, colsByModel)...)
		}
		if t.Lines.Enabled() {
			lw := tw + ".lines"
			l := t.Lines
			if l.Field != "" && !documentFormKeyRe.MatchString(l.Field) {
				errs = append(errs, fmt.Sprintf("%s.field %q is not a lowercase snake_case name", lw, l.Field))
			}
			if l.PriceSource != "" && l.PriceSource != "sale" && l.PriceSource != "cost" {
				errs = append(errs, fmt.Sprintf("%s.price_source %q is not one of sale|cost", lw, l.PriceSource))
			}
			seen := map[string]struct{}{}
			for ci, c := range l.Columns {
				if strings.TrimSpace(c) == "" {
					errs = append(errs, fmt.Sprintf("%s.columns[%d] is empty", lw, ci))
					continue
				}
				if _, dup := seen[c]; dup {
					errs = append(errs, fmt.Sprintf("%s.columns[%d] %q is duplicated", lw, ci, c))
				}
				seen[c] = struct{}{}
			}
			target := l.Field
			if target == "" {
				target = df.LinesField
			}
			if target == "" {
				target = "lines"
			}
			if _, clash := fkeys[target]; clash {
				errs = append(errs, fmt.Sprintf("%s: the lines payload field %q collides with a header field of the same key", lw, target))
			}
		}
		errs = append(errs, validateDocumentFormSources(tw, t.Sources)...)
		errs = append(errs, validateDocumentFormCreateModel(tw, t)...)
	}
	return errs
}

// validateDocumentFormCreateModel: a type delegated to another model's create
// flow declares nothing that would be created HERE.
func validateDocumentFormCreateModel(tw string, t DocumentFormType) []string {
	if t.CreateModel == "" {
		return nil
	}
	var errs []string
	if !createModelRe.MatchString(t.CreateModel) {
		errs = append(errs, fmt.Sprintf("%s.create_model %q is not a model key (Model or addon.Model)", tw, t.CreateModel))
	}
	var clash []string
	if len(t.Fields) > 0 {
		clash = append(clash, "fields")
	}
	if t.Lines.Enabled() {
		clash = append(clash, "lines")
	}
	if t.Endpoint != "" {
		clash = append(clash, "endpoint")
	}
	if t.SubmitAction != "" {
		clash = append(clash, "submit_action")
	}
	if t.Layout != "" || t.Party != nil || len(t.Sources) > 0 || t.Preview != nil {
		clash = append(clash, "the editor block (layout/party/sources/preview)")
	}
	if len(clash) > 0 {
		errs = append(errs, fmt.Sprintf("%s.create_model delegates the create to %q: %s would never be used here", tw, t.CreateModel, strings.Join(clash, ", ")))
	}
	return errs
}

// sourceOrEmpty is the Source of a dynamic options block, "" when nil.
func (d *DynamicOptions) sourceOrEmpty() string {
	if d == nil {
		return ""
	}
	return d.Source
}

// validateDocumentFormSources checks the «crear desde» origins of a type: keys
// unique, the pending-quantity columns are identifiers, the addon endpoint is a
// path and exclude_states only makes sense with line_link_field (the computed
// pending quantity is the only one that reads this document's states).
func validateDocumentFormSources(tw string, sources []DocumentFormSource) []string {
	var errs []string
	seen := map[string]struct{}{}
	for si, src := range sources {
		sw := fmt.Sprintf("%s.sources[%d]", tw, si)
		if !documentFormKeyRe.MatchString(src.Key) {
			errs = append(errs, fmt.Sprintf("%s.key %q is not a lowercase snake_case name", sw, src.Key))
		} else if _, dup := seen[src.Key]; dup {
			errs = append(errs, fmt.Sprintf("%s.key %q is duplicated", sw, src.Key))
		}
		seen[src.Key] = struct{}{}
		if strings.TrimSpace(src.Model) == "" || strings.TrimSpace(src.Lines) == "" {
			errs = append(errs, fmt.Sprintf("%s: model and lines are required", sw))
		}
		for name, col := range map[string]string{
			"qty_field": src.QtyField, "line_link_field": src.LineLinkField,
			"remaining_qty_field": src.RemainingQtyField, "link_field": src.LinkField,
		} {
			if col != "" && !documentFormKeyRe.MatchString(col) {
				errs = append(errs, fmt.Sprintf("%s.%s %q is not a lowercase snake_case column", sw, name, col))
			}
		}
		if src.RemainingEndpoint != "" && !strings.HasPrefix(src.RemainingEndpoint, "/") {
			errs = append(errs, fmt.Sprintf("%s.remaining_endpoint %q must be an absolute path (/...)", sw, src.RemainingEndpoint))
		}
		if len(src.ExcludeStates) > 0 && src.LineLinkField == "" {
			errs = append(errs, fmt.Sprintf("%s.exclude_states needs line_link_field (it filters the documents that consume the source)", sw))
		}
		for i, st := range src.ExcludeStates {
			if strings.TrimSpace(st) == "" {
				errs = append(errs, fmt.Sprintf("%s.exclude_states[%d] is empty", sw, i))
			}
		}
	}
	return errs
}
