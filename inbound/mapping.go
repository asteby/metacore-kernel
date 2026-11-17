package inbound

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

const maxPathDepth = 8

// parsePath splits "a.b.0.c" into segments; only [A-Za-z0-9_-] segments.
func parsePath(p string) ([]string, error) {
	if p == "" {
		return nil, errors.New("empty path")
	}
	segs := strings.Split(p, ".")
	if len(segs) > maxPathDepth {
		return nil, errors.New("path too deep")
	}
	for _, s := range segs {
		if s == "" {
			return nil, fmt.Errorf("bad path %q", p)
		}
		for _, r := range s {
			if !(r == '_' || r == '-' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
				return nil, fmt.Errorf("bad path %q", p)
			}
		}
	}
	return segs, nil
}

// lookup walks a decoded JSON value. ok=false when absent or null.
func lookup(v any, path string) (any, bool) {
	segs, err := parsePath(path)
	if err != nil {
		return nil, false
	}
	cur := v
	for _, s := range segs {
		switch t := cur.(type) {
		case map[string]any:
			n, ok := t[s]
			if !ok {
				return nil, false
			}
			cur = n
		case []any:
			i, err := strconv.Atoi(s)
			if err != nil || i < 0 || i >= len(t) {
				return nil, false
			}
			cur = t[i]
		default:
			return nil, false
		}
	}
	if cur == nil {
		return nil, false
	}
	return cur, true
}

// normalize turns json.Number into int64/float64 so executors get plain types.
func normalize(v any) any {
	switch t := v.(type) {
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i
		}
		f, _ := t.Float64()
		return f
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = normalize(x)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = normalize(x)
		}
		return out
	}
	return v
}

// applyMapping builds the target values. Absent optional sources are skipped
// (the schema decides what is required). Route.Defaults are NOT merged here: they travel as Operation.CreateDefaults and apply on create only.
func applyMapping(r Route, payload any) map[string]any {
	out := make(map[string]any, len(r.Mapping))
	for to, from := range r.Mapping {
		if v, ok := lookup(payload, from); ok {
			out[to] = normalize(v)
		}
	}
	return out
}

func scalarString(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case json.Number:
		return t.String(), true
	case bool:
		return strconv.FormatBool(t), true
	}
	return "", false
}

// eventKey renders the idempotency template. Non-scalar or missing values
// make the key invalid (the caller answers 422).
func eventKey(parts []tmplPart, payload any, h http.Header) (string, error) {
	var b strings.Builder
	for _, p := range parts {
		switch {
		case p.lit != "":
			b.WriteString(p.lit)
		case p.header != "":
			v := strings.TrimSpace(h.Get(p.header))
			if v == "" {
				return "", errors.New("missing header")
			}
			b.WriteString(v)
		default:
			v, ok := lookup(payload, p.path)
			if !ok {
				return "", errors.New("missing field")
			}
			s, ok := scalarString(v)
			if !ok || s == "" {
				return "", errors.New("non-scalar field")
			}
			b.WriteString(s)
		}
	}
	k := b.String()
	if k == "" || len(k) > 256 {
		return "", errors.New("bad key length")
	}
	return k, nil
}
