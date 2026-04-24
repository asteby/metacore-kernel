// Package strings holds small, allocation-light string transforms shared across
// apps. Scope: only helpers that at least two apps genuinely need. Anything
// app-specific stays in the app.
package strings

import (
	"strings"
	"unicode"
)

// TitleCase lowercases the input, then uppercases the first letter of every
// word (runs of letters separated by non-letters). It is locale-agnostic and
// ASCII-aware for the uppercase step, matching the behavior apps relied on
// via strings.Title(strings.ToLower(s)) before strings.Title was deprecated.
func TitleCase(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	b.Grow(len(s))
	atWordStart := true
	for _, r := range s {
		if atWordStart && unicode.IsLetter(r) {
			b.WriteRune(unicode.ToUpper(r))
		} else {
			b.WriteRune(r)
		}
		atWordStart = !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}
	return b.String()
}
