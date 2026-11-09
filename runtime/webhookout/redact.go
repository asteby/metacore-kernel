package webhookout

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Redacted is what a secret-bearing value is replaced with.
const Redacted = "[redacted]"

// defaultSecretKeys are JSON keys whose values are never returned to the
// caller (case-insensitive). The licence bearer values are the reason this
// exists: only a deliberate read of the source of truth may show them.
var defaultSecretKeys = []string{
	"license_key", "license_token", "token", "access_token", "refresh_token",
	"secret", "signing_secret", "client_secret", "password", "api_key",
	"authorization", "private_key",
}

func secretSet(extra []string) map[string]struct{} {
	m := make(map[string]struct{}, len(defaultSecretKeys)+len(extra))
	for _, k := range append(append([]string(nil), defaultSecretKeys...), extra...) {
		m[strings.ToLower(k)] = struct{}{}
	}
	return m
}

// redactJSON decodes body, replaces every secret-keyed string value at any
// depth and returns the value and its re-encoding. A body that is not JSON is
// returned as nil/nil: it is neither parsed nor surfaced (it could hold a
// secret we cannot locate).
func redactJSON(body []byte, secrets map[string]struct{}) (any, []byte) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, nil
	}
	var walk func(any)
	walk = func(n any) {
		switch t := n.(type) {
		case map[string]any:
			for k, c := range t {
				if _, bad := secrets[strings.ToLower(k)]; bad {
					if s, ok := c.(string); ok && s != "" {
						t[k] = Redacted
					} else if c != nil {
						t[k] = Redacted
					}
					continue
				}
				walk(c)
			}
		case []any:
			for _, c := range t {
				walk(c)
			}
		}
	}
	walk(v)
	out, err := json.Marshal(v)
	if err != nil {
		return nil, nil
	}
	return v, out
}
