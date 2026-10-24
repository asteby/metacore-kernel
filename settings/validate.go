package settings

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"

	"github.com/asteby/metacore-kernel/manifest"
)

// validateValue checks v against the declared setting type. Unknown types are
// accepted (the manifest vocabulary is open); known ones are enforced.
func validateValue(def manifest.SettingDef, v any) error {
	switch def.Type {
	case "string", "text", "textarea", "url", "email", "phone", "color", "date", "datetime", "time":
		if _, ok := v.(string); !ok {
			return fmt.Errorf("expected string, got %T", v)
		}
	case "number", "decimal", "currency", "percent":
		if _, ok := asFloat(v); !ok {
			return fmt.Errorf("expected number, got %T", v)
		}
	case "integer", "int":
		f, ok := asFloat(v)
		if !ok || f != math.Trunc(f) {
			return fmt.Errorf("expected integer, got %v", v)
		}
	case "boolean", "bool", "switch", "toggle":
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("expected boolean, got %T", v)
		}
	case "select":
		str, ok := v.(string)
		if !ok {
			return fmt.Errorf("expected string, got %T", v)
		}
		if len(def.Options) > 0 && !hasOption(def.Options, str) {
			return fmt.Errorf("%q is not one of the declared options", str)
		}
	case "multiselect", "multi_select":
		var list []any
		switch t := v.(type) {
		case []any:
			list = t
		case []string:
			for _, x := range t {
				list = append(list, x)
			}
		default:
			return fmt.Errorf("expected list, got %T", v)
		}
		for _, x := range list {
			str, ok := x.(string)
			if !ok {
				return fmt.Errorf("expected list of strings, got element %T", x)
			}
			if len(def.Options) > 0 && !hasOption(def.Options, str) {
				return fmt.Errorf("%q is not one of the declared options", str)
			}
		}
	}
	// Whatever the type, the value must round-trip through JSON.
	if _, err := json.Marshal(v); err != nil {
		return fmt.Errorf("not JSON-encodable: %w", err)
	}
	return nil
}

func hasOption(opts []manifest.Option, v string) bool {
	for _, o := range opts {
		if o.Value == v {
			return true
		}
	}
	return false
}

func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

// jsonEqual compares two value bags by their JSON form (so int 1 == float 1).
func jsonEqual(a, b Values) bool {
	var na, nb any
	ba, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	_ = json.Unmarshal(ba, &na)
	_ = json.Unmarshal(bb, &nb)
	return reflect.DeepEqual(na, nb)
}
