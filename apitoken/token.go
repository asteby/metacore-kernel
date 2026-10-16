package apitoken

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"strings"
)

const (
	// secretBytes is the entropy of a token (256 bits).
	secretBytes = 32
	// PrefixLen is how many leading plaintext characters are kept for display.
	PrefixLen = 12
)

// encodedLen is the unpadded base64url length of secretBytes.
var encodedLen = base64.RawURLEncoding.EncodedLen(secretBytes)

// generate returns a new plaintext for kind using rnd.
func generate(kind Kind, rnd io.Reader) (string, error) {
	b := make([]byte, secretBytes)
	if _, err := io.ReadFull(rnd, b); err != nil {
		return "", err
	}
	return kind.Prefix() + base64.RawURLEncoding.EncodeToString(b), nil
}

// HashToken returns the hex SHA-256 of a plaintext token — the value stored
// in APIToken.TokenHash.
func HashToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// KindOf returns the kind a plaintext claims by its prefix, and whether the
// rest has the exact shape of a token (so malformed input never reaches the
// store).
func KindOf(plaintext string) (Kind, bool) {
	for _, k := range []Kind{KindPublishable, KindSecret, KindPlatform} {
		rest, ok := strings.CutPrefix(plaintext, k.Prefix())
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

// displayPrefix returns the first PrefixLen characters of a plaintext.
func displayPrefix(plaintext string) string {
	if len(plaintext) <= PrefixLen {
		return plaintext
	}
	return plaintext[:PrefixLen]
}

// defaultRand is the entropy source (overridable in tests via WithRand).
var defaultRand io.Reader = rand.Reader
