package dynamic

import "reflect"

// derefRaw flattens an input value before validation: pointers are
// dereferenced and a typed nil (a (*int)(nil) stored in an interface, which
// `raw == nil` does NOT catch) becomes an untyped nil. In-process hosts hand
// the runtime Go maps straight from typed structs, so nullable numerics arrive
// as pointers; without this a nil pointer read as "present but not a number"
// (invalid_type) and a non-nil pointer failed the scalar switch in isNumeric.
// JSON-decoded input is unaffected: it never carries pointers.
func derefRaw(raw any) any {
	if raw == nil {
		return nil
	}
	rv := reflect.ValueOf(raw)
	for rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return nil
		}
		rv = rv.Elem()
	}
	return rv.Interface()
}
