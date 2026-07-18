package dynamic

import (
	"reflect"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// uuidType is the reflect.Type of google/uuid.UUID, used to detect FK / id
// columns that need a valid-uuid-or-drop policy.
var uuidType = reflect.TypeOf(uuid.UUID{})

// coerceInputToStruct normalizes a create/update input map against the Go types
// of the target struct BEFORE it is unmarshalled. A dynamic CRUD form sends
// every value as a string, but typed struct fields (uuid / numeric / bool)
// can't accept an arbitrary string — a single bad field fails the whole
// unmarshal, which surfaces to the caller as a generic "invalid input" / 400.
// This makes the engine forgiving and host-agnostic:
//
//   - bool field, string value     → "true"/"false"/"1"/"0" parsed; else dropped.
//   - numeric field, string value  → parsed; empty/invalid dropped.
//   - uuid field, string value     → kept only if a valid uuid; empty/invalid
//     dropped (so an optional FK like parent_id left as free text doesn't sink
//     the create — it persists as the zero value / NULL).
//   - string field                 → left untouched.
//
// Keys with no matching struct field, and non-string values, are left as-is.
// Mutates `input` in place. Safe on any struct (compiled or reflect-built).
func coerceInputToStruct(input map[string]any, instance any) {
	if len(input) == 0 || instance == nil {
		return
	}
	t := reflect.TypeOf(instance)
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return
	}
	byJSON := jsonFieldTypes(t)
	for key, raw := range input {
		s, isStr := raw.(string)
		if !isStr {
			continue
		}
		ft, ok := byJSON[key]
		if !ok {
			continue
		}
		ts := strings.TrimSpace(s)

		// uuid: keep valid, drop empty/invalid. A nullable FK is a *uuid.UUID
		// (see columnToField) — deref so both the value and pointer forms are
		// recognized. Dropping the key on empty/invalid leaves a pointer field
		// nil → GORM writes NULL.
		baseFt := ft
		if baseFt.Kind() == reflect.Ptr {
			baseFt = baseFt.Elem()
		}
		if baseFt == uuidType {
			if ts == "" {
				delete(input, key)
				continue
			}
			if _, err := uuid.Parse(ts); err != nil {
				delete(input, key)
			}
			continue
		}

		switch ft.Kind() {
		case reflect.String:
			// strings accept anything (incl. "")
		case reflect.Bool:
			if b, err := strconv.ParseBool(ts); err == nil {
				input[key] = b
			} else {
				delete(input, key)
			}
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			if n, err := strconv.ParseInt(ts, 10, 64); err == nil {
				input[key] = n
			} else {
				delete(input, key)
			}
		case reflect.Float32, reflect.Float64:
			if f, err := strconv.ParseFloat(ts, 64); err == nil {
				input[key] = f
			} else {
				delete(input, key)
			}
		default:
			// pointers, structs (time.Time, etc.), slices, maps: drop empty
			// strings (can't unmarshal "" into them) but keep populated values.
			if ts == "" {
				delete(input, key)
			}
		}
	}
}

// jsonFieldTypes walks a struct (including embedded/anonymous fields) and maps
// each field's JSON key → its reflect.Type. Anonymous structs are flattened so
// promoted fields (e.g. BaseUUIDModel.ID → "id") resolve.
func jsonFieldTypes(t reflect.Type) map[string]reflect.Type {
	out := make(map[string]reflect.Type)
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
					continue
				}
			}
			tag := f.Tag.Get("json")
			if tag == "" || tag == "-" {
				continue
			}
			name := strings.Split(tag, ",")[0]
			if name == "" {
				continue
			}
			if _, exists := out[name]; !exists {
				out[name] = f.Type
			}
		}
	}
	walk(t)
	return out
}
