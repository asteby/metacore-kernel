package dynamic

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/modelbase"
	"github.com/asteby/metacore-kernel/validate"
)

// ValidationSchemaResolver returns the declarative column definitions the
// pre-write validation pass enforces for a model name. Hosts wire it from their
// addon registry / installed manifest; the kernel owns no global model index.
// Returning (nil, false) — or an empty slice — disables field validation for
// that model, leaving Create/Update to the legacy coerce-and-drop behaviour.
type ValidationSchemaResolver func(ctx context.Context, model string) ([]manifest.ColumnDef, bool)

// validation codes (locale-agnostic — the SDK localizes them).
const (
	codeRequired      = "required"
	codeInvalidOption = "invalid_option"
	codeNotFound      = "not_found"
	codeDuplicate     = "duplicate"
	codeInvalidType   = "invalid_type"
)

// nilUUIDString is the all-zero UUID; an empty ref may arrive as "" or this.
const nilUUIDString = "00000000-0000-0000-0000-000000000000"

// managedColumns are the framework-owned columns the validation pass never
// enforces — they are stamped by the scoper / GORM hooks, not the caller.
var managedColumns = map[string]struct{}{
	"id":              {},
	"created_at":      {},
	"updated_at":      {},
	"deleted_at":      {},
	"organization_id": {},
	"created_by_id":   {},
}

// numericTypes are the manifest column types whose value must coerce to a
// number; a present-but-non-numeric value fails with invalid_type.
var numericTypes = map[string]struct{}{
	"int":     {},
	"integer": {},
	"bigint":  {},
	"decimal": {},
	"numeric": {},
	"number":  {},
	"float":   {},
	"double":  {},
}

// resolveValidationSchema returns the model's declarative columns, or nil when
// no resolver is wired / the model declares none.
func (s *Service) resolveValidationSchema(ctx context.Context, model string) []manifest.ColumnDef {
	if s.validationSchema == nil {
		return nil
	}
	cols, ok := s.validationSchema(ctx, model)
	if !ok || len(cols) == 0 {
		return nil
	}
	return cols
}

// isEmptyValue reports whether a raw input value counts as "absent" for
// validation: nil, an empty/whitespace string, or the nil UUID (for ref
// columns an empty relation may arrive as "" or the all-zero uuid).
func isEmptyValue(raw any) bool {
	if raw == nil {
		return true
	}
	if s, ok := raw.(string); ok {
		t := strings.TrimSpace(s)
		return t == "" || t == nilUUIDString
	}
	return false
}

// isNumeric reports whether raw is (or parses as) a number.
func isNumeric(raw any) bool {
	switch v := raw.(type) {
	case float64, float32, int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64:
		return true
	case string:
		_, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return err == nil
	default:
		return false
	}
}

// validateWrite runs the structured field-validation pass BEFORE the DB write.
//
//   - On CREATE, selfID is nil and every declared column is validated (required
//     applies to missing/empty columns).
//   - On UPDATE, selfID points at the record's own id (excluded from the unique
//     check) and validation is PATCH-style: only columns PRESENT in input are
//     checked — a required field already persisted is not re-flagged, but
//     blanking one (present but empty) fails `required`.
//
// All failures are collected in one pass; the accumulated *ValidationError is
// returned (nil when clean). DB round-trips (ref existence, unique) are skipped
// for empty/absent values.
func (s *Service) validateWrite(ctx context.Context, model, tableName string, user modelbase.AuthUser, input map[string]any, selfID *uuid.UUID) error {
	cols := s.resolveValidationSchema(ctx, model)
	if cols == nil {
		return nil
	}
	isUpdate := selfID != nil
	ve := &ValidationError{}

	for _, col := range cols {
		name := col.Name
		if name == "" {
			continue
		}
		if _, managed := managedColumns[name]; managed {
			continue
		}
		// Generated / sequence-stamped columns are populated server-side after
		// this pass, so a caller is never required to supply them.
		if col.Generated != "" || col.Sequence != "" {
			continue
		}

		raw, present := input[name]

		// PATCH semantics: on update only validate what the caller sent.
		if isUpdate && !present {
			continue
		}

		empty := isEmptyValue(raw)

		// required: a Required column with no usable value. Skipped when the
		// column carries a Default (the DB fills it) — an omitted defaulted
		// column is not "missing" from the row's perspective.
		if col.Required && empty && col.Default == nil {
			ve.add(name, codeRequired, nil)
			continue
		}

		// Everything below only applies to a value the caller actually supplied.
		if empty {
			continue
		}

		// invalid_type: a numeric column whose value will not coerce.
		if _, isNum := numericTypes[strings.ToLower(col.Type)]; isNum && !isNumeric(raw) {
			ve.add(name, codeInvalidType, map[string]any{"expected": "number"})
			continue
		}

		// invalid_option: a static enum whose value is out of range.
		if len(col.Options) > 0 {
			if allowed, ok := optionAllows(col.Options, raw); !ok {
				ve.add(name, codeInvalidOption, map[string]any{"allowed": allowed})
				continue
			}
		}

		// not_found: a ref (FK) pointing at a non-existent (tenant-scoped) row.
		if col.Ref != "" {
			exists, err := s.refExists(ctx, user, col.Ref, raw)
			if err != nil {
				return err
			}
			if !exists {
				ve.add(name, codeNotFound, map[string]any{"ref": col.Ref})
				continue
			}
		}

		// duplicate: a unique column whose value already exists (excluding self).
		if col.Unique {
			dup, err := s.valueExists(ctx, user, tableName, name, raw, selfID)
			if err != nil {
				return err
			}
			if dup {
				ve.add(name, codeDuplicate, nil)
			}
		}

		// Declarative ValidationRule (regex / min / max / custom) — Laravel-style
		// additive checks the kernel used to author-validate but never execute.
		for _, iss := range s.checkSpec(raw, specFromColumn(col)) {
			ve.add(name, iss.Code, iss.Params)
		}
	}

	if ve.Empty() {
		return nil
	}
	return ve
}

func specFromColumn(col manifest.ColumnDef) validate.Spec {
	spec := validate.Spec{Type: col.Type}
	if col.Validation == nil {
		return spec
	}
	spec.Regex = col.Validation.Regex
	spec.Min = col.Validation.Min
	spec.Max = col.Validation.Max
	spec.Custom = col.Validation.Custom
	return spec
}

func specFromField(f manifest.FieldDef) validate.Spec {
	spec := validate.Spec{Required: f.Required, Type: f.Type}
	if f.Validation != nil {
		spec.Regex = f.Validation.Regex
		spec.Min = f.Validation.Min
		spec.Max = f.Validation.Max
		spec.Custom = f.Validation.Custom
	}
	if len(f.Options) > 0 {
		for _, o := range f.Options {
			spec.Options = append(spec.Options, o.Value)
		}
	}
	return spec
}

func (s *Service) checkSpec(raw any, spec validate.Spec) []validate.Issue {
	if s.customValidators != nil {
		return validate.CheckWithResolver(raw, spec, s.customValidators)
	}
	return validate.Check(raw, spec)
}

// validateActionPayload runs the same structured validator against an action
// form payload BEFORE the trigger dispatches, so a missing/invalid field is a
// 422 {errors:{field:[{code,params}]}} instead of a generic guest error.
// Walks Fields and every wizard step; line-items use dotted keys `items.0.qty`.
func (s *Service) validateActionPayload(def *manifest.ActionDef, payload map[string]any) error {
	if def == nil {
		return nil
	}
	fields := append([]manifest.FieldDef{}, def.Fields...)
	for _, st := range def.Steps {
		fields = append(fields, st.Fields...)
	}
	if len(fields) == 0 {
		return nil
	}
	if payload == nil {
		payload = map[string]any{}
	}
	ve := &ValidationError{}
	validateFieldList(s, ve, fields, payload, "")
	if ve.Empty() {
		return nil
	}
	return ve
}

func validateFieldList(s *Service, ve *ValidationError, fields []manifest.FieldDef, payload map[string]any, prefix string) {
	for _, f := range fields {
		key := f.Key
		if key == "" {
			key = f.Name
		}
		if key == "" {
			continue
		}
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		raw := payload[key]
		if len(f.ItemFields) > 0 {
			rows := asRowSlice(raw)
			if f.Required && len(rows) == 0 {
				ve.add(path, validate.CodeLineItemsRequired, nil)
				continue
			}
			for i, row := range rows {
				validateFieldList(s, ve, f.ItemFields, row, fmt.Sprintf("%s.%d", path, i))
			}
			continue
		}
		for _, iss := range s.checkSpec(raw, specFromField(f)) {
			ve.add(path, iss.Code, iss.Params)
		}
	}
}

func asRowSlice(raw any) []map[string]any {
	switch v := raw.(type) {
	case []map[string]any:
		return v
	case []any:
		out := make([]map[string]any, 0, len(v))
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	default:
		return nil
	}
}

// optionAllows reports whether raw matches one of the static options and, when
// it does not, returns the allowed value list for the error params.
func optionAllows(opts []manifest.Option, raw any) (allowed []string, ok bool) {
	want := valueToString(raw)
	allowed = make([]string, 0, len(opts))
	for _, o := range opts {
		allowed = append(allowed, o.Value)
		if o.Value == want {
			ok = true
		}
	}
	return allowed, ok
}

// refExists reports whether a row with the given id exists in the ref target
// table, scoped to the caller's tenant.
func (s *Service) refExists(ctx context.Context, user modelbase.AuthUser, ref string, raw any) (bool, error) {
	table, ok := refTable(ref)
	if !ok {
		// Unrecognized / unsafe ref target — do not block the write on it.
		return true, nil
	}
	var count int64
	q := s.scope.ScopeQuery(s.db.WithContext(ctx).Table(table), user).
		Where("id = ?", valueToString(raw))
	if err := q.Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

// valueExists reports whether another row (excluding selfID) already holds the
// given value in column, scoped to the caller's tenant.
func (s *Service) valueExists(ctx context.Context, user modelbase.AuthUser, table, column string, raw any, selfID *uuid.UUID) (bool, error) {
	if !safeColumn.MatchString(column) {
		return false, nil
	}
	var count int64
	q := s.scope.ScopeQuery(s.db.WithContext(ctx).Table(table), user).
		Where(column+" = ?", raw)
	if selfID != nil {
		q = q.Where("id <> ?", selfID.String())
	}
	if err := q.Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

// refTable extracts the queryable table name from a manifest Ref. A ref is
// either a bare table ("orders") or "schema.table" (addon-isolated). Both forms
// are safe to splice as a GORM .Table target after an identifier check.
func refTable(ref string) (string, bool) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", false
	}
	for _, part := range strings.SplitN(ref, ".", 2) {
		if !safeColumn.MatchString(part) {
			return "", false
		}
	}
	return ref, true
}

// valueToString renders a raw input value as its string form for comparison
// against option values / row ids.
func valueToString(raw any) string {
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	case nil:
		return ""
	default:
		return strings.TrimSpace(fmt.Sprintf("%v", v))
	}
}
