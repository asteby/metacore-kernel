package dynamic

// option_when.go — el servidor también respeta el `when` de las opciones
// estáticas de un select (manifest v3 FieldOption.when).
//
// Un select cuyas opciones dependen de otro campo («99 · Por definir» solo con
// método PPD; con PPD solo 99) lo filtraba únicamente el SDK al pintar el
// formulario. Un payload armado a mano —o un cliente que no filtra— guardaba
// la combinación prohibida y el error salía después, del PAC. Aquí la misma
// regla se evalúa al guardar:
//
//   - payload de una acción (validateFieldList) y
//   - encabezado de un tipo de document_forms en Create/Update
//     (DocumentLinesType.Fields, derivado en DeriveDocumentLines).
//
// Una opción que no aplica con el valor actual del campo hermano es
// `invalid_option` con la lista de valores que sí aplican, el mismo código que
// una opción fuera de catálogo.

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/asteby/metacore-kernel/manifest"
)

// hasOptionWhen reports whether any static option of f is gated by `when`.
func hasOptionWhen(f manifest.FieldDef) bool {
	for _, o := range f.Options {
		if o.When != nil {
			return true
		}
	}
	return false
}

// optionApplies evaluates one option's `when` against the sibling values. The
// gating field is when.Field, else the field's DependsOn; without either the
// option always applies (same rule as the SDK's applyOptionWhen).
func optionApplies(o manifest.Option, dependsOn string, values func(string) any) bool {
	w := o.When
	if w == nil {
		return true
	}
	gate := strings.TrimSpace(w.Field)
	if gate == "" {
		gate = strings.TrimSpace(dependsOn)
	}
	if gate == "" {
		return true
	}
	current := valueToString(values(gate))
	if len(w.In) > 0 && !containsString(w.In, current) {
		return false
	}
	if len(w.NotIn) > 0 && containsString(w.NotIn, current) {
		return false
	}
	return true
}

func containsString(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// optionWhenAllows reports whether raw is one of f's options that APPLY under
// the sibling values, returning the applicable values for the error params.
// A value outside the catalog is not this check's business (optionAllows).
func optionWhenAllows(f manifest.FieldDef, raw any, values func(string) any) (allowed []string, ok bool) {
	want := valueToString(raw)
	ok = true
	allowed = make([]string, 0, len(f.Options))
	for _, o := range f.Options {
		applies := optionApplies(o, f.DependsOn, values)
		if applies {
			allowed = append(allowed, o.Value)
		}
		if o.Value == want && !applies {
			ok = false
		}
	}
	return allowed, ok
}

// fieldKey is the identifier a FieldDef is posted under.
func fieldKey(f manifest.FieldDef) string {
	if f.Key != "" {
		return f.Key
	}
	return f.Name
}

// lookupInput reads key from a write payload. A dotted key of a json column
// (`fiscal_data.metodo_pago`) is read flat as posted by a form, or nested in
// the column (map or JSON text) as stored.
func lookupInput(input map[string]any, key string) (any, bool) {
	if input == nil {
		return nil, false
	}
	if v, ok := input[key]; ok {
		return v, true
	}
	head, rest, dotted := strings.Cut(key, ".")
	if !dotted {
		return nil, false
	}
	var bag map[string]any
	switch v := derefRaw(input[head]).(type) {
	case map[string]any:
		bag = v
	case string:
		if json.Unmarshal([]byte(v), &bag) != nil {
			return nil, false
		}
	case []byte:
		if json.Unmarshal(v, &bag) != nil {
			return nil, false
		}
	default:
		return nil, false
	}
	return lookupInput(bag, rest)
}

// checkOptionWhen validates the `when`-gated selects of fields against input
// (values absent from input fall back to before, the persisted row on an
// update). Only a field whose value is present and non-empty is checked: the
// required gate is someone else's, and a PATCH that sends neither the field nor
// its gate leaves the pair as it was.
func checkOptionWhen(ve *ValidationError, fields []manifest.FieldDef, input, before map[string]any, prefix string) {
	values := func(k string) any {
		if v, ok := lookupInput(input, k); ok {
			return v
		}
		v, _ := lookupInput(before, k)
		return v
	}
	for _, f := range fields {
		if !hasOptionWhen(f) {
			continue
		}
		key := fieldKey(f)
		if key == "" {
			continue
		}
		raw := values(key)
		if isEmptyValue(raw) {
			continue
		}
		if allowed, ok := optionWhenAllows(f, raw, values); !ok {
			path := key
			if prefix != "" {
				path = prefix + "." + key
			}
			ve.add(path, codeInvalidOption, map[string]any{"allowed": allowed})
		}
	}
}

// optionWhenFields keeps the fields of a document type whose options carry a
// `when` — the only ones the save has to re-check.
func optionWhenFields(fields []manifest.FieldDef) []manifest.FieldDef {
	var out []manifest.FieldDef
	for _, f := range fields {
		if hasOptionWhen(f) {
			out = append(out, f)
		}
	}
	return out
}

// checkDocumentFormFields validates the header of a document_forms write: the
// `when`-gated selects of the payload's type. nil when nothing applies.
func (s *Service) checkDocumentFormFields(ctx context.Context, model string, input, before map[string]any) *ValidationError {
	sp := s.resolveDocumentLines(ctx, model)
	probe := input
	if sp != nil && sp.TypeField != "" && before != nil {
		// An update that does not resend the type keeps the persisted one.
		if _, sent := input[sp.TypeField]; !sent {
			probe = before
		}
	}
	typ := sp.typeFor(probe)
	if typ == nil || len(typ.Fields) == 0 {
		return nil
	}
	ve := &ValidationError{}
	checkOptionWhen(ve, typ.Fields, input, before, "")
	if ve.Empty() {
		return nil
	}
	return ve
}
