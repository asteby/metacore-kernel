package inbound

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/asteby/metacore-kernel/modelbase"
)

// CodeMatchValueInvalid: a match value cannot be converted to the type of its
// column (422, before any effect). Without the conversion the value would
// reach the database as a parameter of the wrong type (Postgres refuses an
// integer bound to a text column) and surface as a 500.
const CodeMatchValueInvalid = "match_value_invalid"

// Column classes of the match conversion. "" is a column whose type the
// metadata does not tell (or one that is never a match key: json, vector): its
// value is left as the event sent it.
const (
	classText   = "text"
	classNumber = "number"
	classBool   = "bool"
	classTime   = "time"
)

// matchClass maps a column type of the table metadata (a storage type such as
// "integer" or "varchar(64)", or its UI form "number" / "boolean" / "datetime")
// to its class.
func matchClass(t string) string {
	t = strings.ToLower(strings.TrimSpace(t))
	switch t {
	case "":
		return ""
	case "text", "string", "uuid":
		return classText
	case "number", "integer", "int", "smallint", "bigint", "float", "double", "numeric", "decimal":
		return classNumber
	case "boolean", "bool":
		return classBool
	case "date", "datetime", "timestamp", "timestamptz", "timestamp with time zone":
		return classTime
	}
	for _, p := range []string{"varchar", "char", "character"} {
		if strings.HasPrefix(t, p) {
			return classText
		}
	}
	for _, p := range []string{"numeric", "decimal"} {
		if strings.HasPrefix(t, p) {
			return classNumber
		}
	}
	return ""
}

// columnClasses is the class of every column of the metadata, by key.
func columnClasses(meta *modelbase.TableMetadata) map[string]string {
	out := make(map[string]string, len(meta.Columns))
	for _, c := range meta.Columns {
		if cl := matchClass(c.Type); cl != "" {
			out[c.Key] = cl
		}
	}
	return out
}

// scalarText is the canonical text of a scalar: integers without a fraction,
// floats without exponent notation, booleans as true/false. ok is false for
// anything that is not a scalar (objects, arrays).
func scalarText(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case []byte:
		return string(t), true
	case bool:
		return strconv.FormatBool(t), true
	case json.Number:
		return t.String(), true
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) {
			return "", false
		}
		return strconv.FormatFloat(t, 'f', -1, 64), true
	case float32:
		f := float64(t)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return "", false
		}
		return strconv.FormatFloat(f, 'f', -1, 32), true
	case fmt.Stringer:
		return t.String(), true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(rv.Int(), 10), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.FormatUint(rv.Uint(), 10), true
	}
	return "", false
}

// parseMatchInstant reads an RFC 3339 / SQL timestamp or a calendar date.
func parseMatchInstant(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC(), true
	}
	if n, err := guardNormalizeString(s, true); err == nil {
		if t, ok := n.(time.Time); ok {
			return t, true
		}
	}
	return time.Time{}, false
}

// coerceMatchValue converts v to the Go type the database expects for a column
// of the class: text -> string, number -> int64 (whole) or float64, bool ->
// bool, time -> time.Time. A null or blank value passes through (matchValue
// reports it with its own code). A value that cannot be converted is an error.
func coerceMatchValue(class string, v any) (any, error) {
	if v == nil {
		return v, nil
	}
	if s, ok := v.(string); ok && strings.TrimSpace(s) == "" {
		return v, nil
	}
	bad := fmt.Errorf("value of type %T is not a valid %s", v, class)
	switch class {
	case classText:
		s, ok := scalarText(v)
		if !ok {
			return nil, bad
		}
		return s, nil
	case classNumber:
		return coerceNumber(v, bad)
	case classBool:
		if b, ok := v.(bool); ok {
			return b, nil
		}
		if s, ok := v.(string); ok {
			b, err := strconv.ParseBool(strings.TrimSpace(s))
			if err != nil {
				return nil, bad
			}
			return b, nil
		}
		if n, err := coerceNumber(v, bad); err == nil {
			if i, isInt := n.(int64); isInt && (i == 0 || i == 1) {
				return i == 1, nil
			}
		}
		return nil, bad
	case classTime:
		switch t := v.(type) {
		case time.Time:
			return t, nil
		case string:
			if tm, ok := parseMatchInstant(t); ok {
				return tm, nil
			}
		}
		return nil, bad
	}
	return v, nil
}

func coerceNumber(v any, bad error) (any, error) {
	switch t := v.(type) {
	case bool:
		return nil, bad
	case string:
		s := strings.TrimSpace(t)
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return n, nil
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil, bad
		}
		return wholeOrFloat(f, bad)
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return n, nil
		}
		f, err := t.Float64()
		if err != nil {
			return nil, bad
		}
		return wholeOrFloat(f, bad)
	case float64:
		return wholeOrFloat(t, bad)
	case float32:
		return wholeOrFloat(float64(t), bad)
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int(), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if u := rv.Uint(); u <= math.MaxInt64 {
			return int64(u), nil
		}
		return float64(rv.Uint()), nil
	}
	return nil, bad
}

// wholeOrFloat returns an integral float within the exact range of a float64
// as an int64 (what an integer column binds), any other finite float as is.
func wholeOrFloat(f float64, bad error) (any, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, bad
	}
	if f == math.Trunc(f) && math.Abs(f) <= 1<<53 {
		return int64(f), nil
	}
	return f, nil
}

// normalizeMatch converts the match values of the operation (Route.Match and
// Guard.Match) to the type of their column, so that a number that arrived for a
// text column (or a numeric string for an integer one) behaves as if it had
// arrived with the right type: the find, the typed comparison with the row, the
// tombstone lookup and the write all see the same value. Operation.Values is
// copied before it is changed (the caller's map may feed later steps). A value
// that cannot be converted fails closed (422 match_value_invalid). A model
// whose metadata cannot be read, or a column whose type it does not tell, is
// left as is.
func normalizeMatch(ctx context.Context, st dynStore, op *Operation) error {
	fields := op.Route.Match
	if op.Guard != nil {
		fields = append(append([]string(nil), fields...), op.Guard.Match...)
	}
	if len(fields) == 0 {
		return nil
	}
	meta, err := st.TableMetadata(ctx, op.Route.Model)
	if err != nil || meta == nil {
		return nil
	}
	classes := columnClasses(meta)
	var vals map[string]any
	for _, f := range fields {
		cl, v := classes[f], op.Values[f]
		if cl == "" || v == nil {
			continue
		}
		nv, cerr := coerceMatchValue(cl, v)
		if cerr != nil {
			return &ExecError{Kind: KindInvalid, Code: CodeMatchValueInvalid, Err: fmt.Errorf("match field %q: %w", f, cerr)}
		}
		if _, isTime := nv.(time.Time); isTime {
			continue // the event's own spelling stays; the tombstone query types it
		}
		if reflect.DeepEqual(nv, v) {
			continue
		}
		if vals == nil {
			vals = make(map[string]any, len(op.Values))
			for k, x := range op.Values {
				vals[k] = x
			}
		}
		vals[f] = nv
	}
	if vals != nil {
		op.Values = vals
	}
	return nil
}

// likeKind spells want as the kind of the stored value row, for a column whose
// type the metadata does not tell: a number against a text row becomes its
// canonical text, a numeric string against a number row becomes the number,
// and so on. ok is false when there is nothing to convert.
func likeKind(row, want any) (any, bool) {
	switch row.(type) {
	case string:
		if _, isStr := want.(string); isStr {
			return nil, false
		}
		s, ok := scalarText(want)
		return s, ok
	case bool:
		if s, isStr := want.(string); isStr {
			if b, err := strconv.ParseBool(strings.TrimSpace(s)); err == nil {
				return b, true
			}
		}
	case time.Time:
		if s, isStr := want.(string); isStr {
			return parseMatchInstant(s)
		}
	default:
		if _, isNum := asFloat(row); isNum {
			if s, isStr := want.(string); isStr {
				f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
				if err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
					return f, true
				}
			}
		}
	}
	return nil, false
}

// matchEqual is valuesEqual of a stored value against the event's, tolerant of
// the event having spelled it with another JSON type than the column's.
func matchEqual(row, want any) bool {
	if valuesEqual(row, want) {
		return true
	}
	alt, ok := likeKind(canonValue(row), want)
	return ok && valuesEqual(row, alt)
}

// filterString is the text a match value takes in a List filter.
func filterString(v any) string {
	if t, ok := v.(time.Time); ok {
		return t.UTC().Format(time.RFC3339Nano)
	}
	if s, ok := scalarText(v); ok {
		return s
	}
	return fmt.Sprint(v)
}
