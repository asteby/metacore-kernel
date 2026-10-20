package inbox

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

// StoredHeaders is the allowlist of request headers kept in the journal
// (lowercase). Everything else (authorization, cookie, ...) is dropped.
var StoredHeaders = []string{
	"x-request-id", "x-signature", "stripe-signature", "user-agent",
	"content-type", "content-length", "cf-ray", "x-forwarded-for",
}

// signatureHeaders are stored in redacted shape (RedactSignature).
var signatureHeaders = map[string]bool{"x-signature": true, "stripe-signature": true}

// RedactHeaders keeps only the allowlisted headers, with signature headers
// reduced to their shape. Repeated values are joined with ", ".
func RedactHeaders(h http.Header) map[string]string {
	out := map[string]string{}
	allowed := map[string]bool{}
	for _, k := range StoredHeaders {
		allowed[k] = true
	}
	for k, vs := range h {
		lk := strings.ToLower(strings.TrimSpace(k))
		if !allowed[lk] || len(vs) == 0 {
			continue
		}
		v := strings.Join(vs, ", ")
		if signatureHeaders[lk] {
			v = RedactSignature(v)
		}
		if len(v) > 512 {
			v = v[:512]
		}
		out[lk] = v
	}
	return out
}

var hexRe = regexp.MustCompile(`^[0-9a-fA-F]+$`)

// RedactSignature keeps the shape of a signature header and never a hash:
// "ts=1760997480123,v1=<64 hex>" becomes "ts=1760997480123,v1=[64h:ab12…]"
// (length plus the first four characters, enough to tell a rotation apart).
// Stripe's "t=...,v1=...,v1=..." is handled the same way, one marker per hash.
func RedactSignature(v string) string {
	parts := strings.Split(v, ",")
	for i, p := range parts {
		p = strings.TrimSpace(p)
		k, val, ok := strings.Cut(p, "=")
		if !ok {
			parts[i] = redactToken(p)
			continue
		}
		parts[i] = k + "=" + redactToken(val)
	}
	return strings.Join(parts, ",")
}

// redactToken masks a value that looks like a hash or secret (32+ characters)
// and leaves short values such as timestamps and version tags alone.
func redactToken(s string) string {
	if len(s) < 32 {
		return s
	}
	kind := "c"
	if hexRe.MatchString(s) {
		kind = "h"
	}
	return fmt.Sprintf("[%d%s:%s…]", len(s), kind, s[:4])
}

// piiKeys are masked wherever they appear (case-insensitive).
var piiKeys = map[string]bool{
	"email": true, "phone": true, "first_name": true, "last_name": true, "name": true,
	"identification": true, "address": true, "payer": true, "billing_details": true,
	"shipping": true, "customer_email": true, "customer_details": true,
	"access_token": true, "client_secret": true, "secret": true, "authorization": true,
}

// cardKeep are the only card.* fields shown.
var cardKeep = map[string]bool{"last_four_digits": true, "brand": true, "last4": true}

const redacted = "[redacted]"

var longHexRe = regexp.MustCompile(`[0-9a-fA-F]{64}`)
var secretLike = regexp.MustCompile(`(APP_USR-|TEST-|sk_(live|test)_|whsec_)[A-Za-z0-9_-]+`)

// RedactBody returns the JSON shown in the UI: personal data masked, secrets
// and 64-hex strings removed. A body that is not JSON is replaced by a marker.
func RedactBody(body []byte) []byte {
	trim := bytes.TrimSpace(body)
	if len(trim) == 0 {
		return []byte(`{}`)
	}
	dec := json.NewDecoder(bytes.NewReader(trim))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil || dec.More() {
		out, _ := json.Marshal(map[string]any{"_unparsed": true, "length": len(body)})
		return out
	}
	out, err := json.Marshal(redactValue(v, "", false))
	if err != nil {
		out, _ = json.Marshal(map[string]any{"_unparsed": true, "length": len(body)})
	}
	return out
}

func redactValue(v any, key string, inCard bool) any {
	lk := strings.ToLower(key)
	if key != "" {
		if inCard {
			if !cardKeep[lk] {
				return redacted
			}
		} else if piiKeys[lk] {
			return redacted
		}
	}
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		card := inCard || lk == "card"
		for k, x := range t {
			out[k] = redactValue(x, k, card)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = redactValue(x, "", inCard)
		}
		return out
	case string:
		s := secretLike.ReplaceAllString(t, redacted)
		return longHexRe.ReplaceAllString(s, redacted)
	default:
		return v
	}
}
