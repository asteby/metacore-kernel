package webhookout

import (
	"encoding/json"
	"fmt"
	"net/mail"
	"net/url"
	"strconv"
	"strings"
)

// ExpandPath resolves the {record.<col>} and {payload.<field>} placeholders of
// handler.path. Every value is path-escaped, must be a non-empty scalar and
// must not contain "/", "\", control characters or be "." / "..": a value can
// never add, drop or climb a path segment. An unknown placeholder, an empty
// value or a leftover brace is an error (nothing is sent).
func ExpandPath(tmpl string, record, payload map[string]any) (string, error) {
	if !strings.HasPrefix(tmpl, "/") || strings.HasPrefix(tmpl, "//") {
		return "", fmt.Errorf("path must start with a single '/'")
	}
	if strings.ContainsAny(tmpl, "?#\\") {
		return "", fmt.Errorf("path must not carry a query, fragment or backslash")
	}
	var b strings.Builder
	for i := 0; i < len(tmpl); {
		c := tmpl[i]
		switch c {
		case '{':
			end := strings.IndexByte(tmpl[i:], '}')
			if end < 0 {
				return "", fmt.Errorf("unclosed '{' in path")
			}
			name := tmpl[i+1 : i+end]
			v, err := placeholder(name, record, payload)
			if err != nil {
				return "", err
			}
			b.WriteString(url.PathEscape(v))
			i += end + 1
		case '}':
			return "", fmt.Errorf("unexpected '}' in path")
		default:
			b.WriteByte(c)
			i++
		}
	}
	out := b.String()
	for _, seg := range strings.Split(out, "/") {
		if seg == ".." || seg == "." {
			return "", fmt.Errorf("path must not contain '.' or '..' segments")
		}
	}
	return out, nil
}

func placeholder(name string, record, payload map[string]any) (string, error) {
	var src map[string]any
	var key string
	switch {
	case strings.HasPrefix(name, "record."):
		src, key = record, strings.TrimPrefix(name, "record.")
	case strings.HasPrefix(name, "payload."):
		src, key = payload, strings.TrimPrefix(name, "payload.")
	default:
		return "", fmt.Errorf("unknown placeholder {%s}", name)
	}
	raw, ok := src[key]
	if !ok {
		return "", fmt.Errorf("placeholder {%s} has no value", name)
	}
	s, ok := scalarString(raw)
	if !ok || s == "" {
		return "", fmt.Errorf("placeholder {%s} is empty or not a scalar", name)
	}
	if s == "." || s == ".." || strings.ContainsAny(s, "/\\") {
		return "", fmt.Errorf("placeholder {%s} holds a value that is not a safe path segment", name)
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("placeholder {%s} holds control characters", name)
		}
	}
	return s, nil
}

// scalarString renders a string, number or bool. Anything else is not a scalar.
func scalarString(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t), true
	case json.Number:
		return t.String(), true
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), true
	case float32:
		return strconv.FormatFloat(float64(t), 'f', -1, 32), true
	case int:
		return strconv.Itoa(t), true
	case int32:
		return strconv.FormatInt(int64(t), 10), true
	case int64:
		return strconv.FormatInt(t, 10), true
	case uint:
		return strconv.FormatUint(uint64(t), 10), true
	case uint64:
		return strconv.FormatUint(t, 10), true
	case bool:
		return strconv.FormatBool(t), true
	case fmt.Stringer: // uuid.UUID and similar identifiers
		return strings.TrimSpace(t.String()), true
	}
	return "", false
}

// validActor mirrors the receiver's rule for X-Actor: a bare, well-formed
// staff e-mail address.
func validActor(s string) bool {
	a, err := mail.ParseAddress(s)
	if err != nil || a.Address != s || len(s) > 254 {
		return false
	}
	at := strings.LastIndexByte(s, '@')
	return at > 0 && strings.Contains(s[at+1:], ".") && !strings.HasSuffix(s, ".")
}
