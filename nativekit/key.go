package nativekit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// Key joins the non-empty parts of an idempotency key with ":" so every module
// derives keys the same way ("sale:<id>:line-2:out"). Parts are trimmed; an
// all-empty key is "", which Once treats as "no idempotency".
func Key(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, ":")
}

// Fingerprint is a stable digest of v's JSON form. Pass it to Once so reusing a
// key with a different payload is rejected instead of silently replaying.
func Fingerprint(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16])
}
