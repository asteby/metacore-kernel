package validate

import (
	"fmt"
	"net/mail"
	"net/url"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

func builtinEmail(value any) (string, map[string]any) {
	s := valueToString(value)
	if s == "" {
		return "", nil
	}
	addr, err := mail.ParseAddress(s)
	if err != nil || addr.Address != s {
		return CodeEmail, nil
	}
	return "", nil
}

func builtinUUID(value any) (string, map[string]any) {
	s := valueToString(value)
	if s == "" {
		return "", nil
	}
	if _, err := uuid.Parse(s); err != nil {
		return CodeUUID, nil
	}
	return "", nil
}

func builtinURL(value any) (string, map[string]any) {
	s := valueToString(value)
	if s == "" {
		return "", nil
	}
	u, err := url.ParseRequestURI(s)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return CodeURL, nil
	}
	return "", nil
}

func builtinNumeric(value any) (string, map[string]any) {
	if isNumeric(value) {
		return "", nil
	}
	return CodeNumeric, map[string]any{"expected": "number"}
}

func builtinInteger(value any) (string, map[string]any) {
	switch v := value.(type) {
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return "", nil
	case float64:
		if v == float64(int64(v)) {
			return "", nil
		}
	case string:
		if _, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			return "", nil
		}
	}
	return CodeInteger, map[string]any{"expected": "integer"}
}

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

func valueToString(raw any) string {
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case nil:
		return ""
	default:
		return strings.TrimSpace(fmt.Sprintf("%v", v))
	}
}
