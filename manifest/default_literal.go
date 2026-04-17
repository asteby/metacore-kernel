package manifest

import (
	"fmt"
	"strconv"
	"strings"
)

// DefaultLiteral coerces a manifest ColumnDef.Default value (any of string,
// number, bool, nil) into a DDL-safe SQL literal. Returns ("", false) when
// the value cannot be represented safely — schema generators should treat
// that as "no default" rather than emit an unbounded string.
//
// The whitelist mirrors defaultRe:
//   - numeric literals (int / float, positive or negative)
//   - quoted strings (no embedded quote / semicolon / backslash)
//   - builtin calls: now() | gen_random_uuid() | uuid_generate_v4() | current_timestamp
//   - boolean: true | false | null
func DefaultLiteral(v any) (string, bool) {
	switch x := v.(type) {
	case nil:
		return "", true
	case string:
		if x == "" {
			return "", true
		}
		if defaultRe.MatchString(x) {
			return x, true
		}
		return "", false
	case bool:
		if x {
			return "true", true
		}
		return "false", true
	case float64:
		// JSON numbers decode as float64 in encoding/json.
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10), true
		}
		return strings.TrimRight(strconv.FormatFloat(x, 'f', -1, 64), "."), true
	case int:
		return strconv.Itoa(x), true
	case int64:
		return strconv.FormatInt(x, 10), true
	case float32:
		return strconv.FormatFloat(float64(x), 'f', -1, 32), true
	}
	// Unknown type — refuse rather than risk injection.
	return "", false
}

// FormatDefault returns a stringified default suitable for CREATE TABLE DDL,
// or an error when the value is not representable.
func FormatDefault(v any) (string, error) {
	s, ok := DefaultLiteral(v)
	if !ok {
		return "", fmt.Errorf("default value %v (%T) not allowed", v, v)
	}
	return s, nil
}
