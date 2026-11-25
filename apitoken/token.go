package apitoken

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"io"
	"strings"
)

const (
	// secretBytes is the entropy of a token (256 bits).
	secretBytes = 32
	// PrefixLen is how many leading plaintext characters are kept for display
	// (a namespaced token keeps the namespace and its separator on top).
	PrefixLen = 12
	// maxDisplayPrefix is the size of the APIToken.Prefix column.
	maxDisplayPrefix = 16
)

// Hash schemes. The TokenHash column holds "<scheme>$<hex digest>"; a value
// with no scheme is the legacy bare hex SHA-256 written before schemes existed
// and keeps verifying.
const (
	// SchemeSHA256 — SHA-256 of the plaintext.
	SchemeSHA256 = "sha256"
	// SchemeHMACSHA256 — HMAC-SHA256 of the plaintext keyed by the deployment
	// pepper.
	SchemeHMACSHA256 = "hmac-sha256"
)

// encodedLen is the unpadded base64url length of secretBytes.
var encodedLen = base64.RawURLEncoding.EncodedLen(secretBytes)

// generate returns a new plaintext for kind in namespace ns ("" = none) using
// rnd.
func generate(ns string, kind Kind, rnd io.Reader) (string, error) {
	b := make([]byte, secretBytes)
	if _, err := io.ReadFull(rnd, b); err != nil {
		return "", err
	}
	return kindPrefix(ns, kind) + base64.RawURLEncoding.EncodeToString(b), nil
}

// kindPrefix is the plaintext prefix of kind under ns: "<ns>_<kind>_", or the
// bare "<kind>_" when ns is empty.
func kindPrefix(ns string, kind Kind) string {
	p := kind.Prefix()
	if p == "" || ns == "" {
		return p
	}
	return ns + "_" + p
}

// validNamespace reports whether ns is usable: 2-8 lowercase letters or digits
// starting with a letter (no underscore, so the prefix stays unambiguous).
func validNamespace(ns string) bool {
	if len(ns) < 2 || len(ns) > 8 || ns[0] < 'a' || ns[0] > 'z' {
		return false
	}
	for i := 0; i < len(ns); i++ {
		c := ns[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// HashToken returns the bare hex SHA-256 of a plaintext token — the legacy
// (scheme-less) value of APIToken.TokenHash. It is unchanged so existing
// callers keep working; a Service may store the scheme-qualified forms instead
// (see SchemeSHA256, SchemeHMACSHA256).
func HashToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// HashTokenSchemed returns the "sha256$<hex>" column value of plaintext.
func HashTokenSchemed(plaintext string) string {
	return SchemeSHA256 + "$" + HashToken(plaintext)
}

// HashTokenHMAC returns the "hmac-sha256$<hex>" column value of plaintext
// keyed by pepper.
func HashTokenHMAC(plaintext string, pepper []byte) string {
	m := hmac.New(sha256.New, pepper)
	m.Write([]byte(plaintext))
	return SchemeHMACSHA256 + "$" + hex.EncodeToString(m.Sum(nil))
}

// hashCandidates lists every column value a plaintext can be stored as: the
// peppered form (only when a pepper is available), then the scheme-qualified
// and the legacy bare SHA-256.
func hashCandidates(plaintext string, pepper []byte) []string {
	out := make([]string, 0, 3)
	if len(pepper) > 0 {
		out = append(out, HashTokenHMAC(plaintext, pepper))
	}
	return append(out, HashTokenSchemed(plaintext), HashToken(plaintext))
}

// matchesAny compares stored against every candidate in constant time per
// comparison and without short-circuiting, so timing reveals neither which
// form matched nor how much of it.
func matchesAny(stored string, candidates []string) bool {
	ok := 0
	for _, c := range candidates {
		ok |= subtle.ConstantTimeCompare([]byte(stored), []byte(c))
	}
	return ok == 1
}

// KindOf returns the kind a plaintext claims by its prefix, and whether the
// rest has the exact shape of a token (so malformed input never reaches the
// store).
func KindOf(plaintext string) (Kind, bool) { return KindOfIn("", plaintext) }

// KindOfIn is KindOf for tokens issued under namespace ns ("" = none): the
// prefix must be exactly "<ns>_<kind>_".
func KindOfIn(ns, plaintext string) (Kind, bool) {
	for _, k := range []Kind{KindPublishable, KindSecret, KindPlatform} {
		rest, ok := strings.CutPrefix(plaintext, kindPrefix(ns, k))
		if !ok {
			continue
		}
		if len(rest) != encodedLen {
			return "", false
		}
		if _, err := base64.RawURLEncoding.DecodeString(rest); err != nil {
			return "", false
		}
		return k, true
	}
	return "", false
}

// displayPrefix returns the leading characters of a plaintext kept for
// display: PrefixLen, plus the namespace and its separator, capped at the
// column size.
func displayPrefix(ns, plaintext string) string {
	n := PrefixLen
	if ns != "" {
		n = min(PrefixLen+len(ns)+1, maxDisplayPrefix)
	}
	if len(plaintext) <= n {
		return plaintext
	}
	return plaintext[:n]
}

// defaultRand is the entropy source (overridable in tests via WithRand).
var defaultRand io.Reader = rand.Reader
