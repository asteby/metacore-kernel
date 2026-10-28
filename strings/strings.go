// Package strings holds small, allocation-light string transforms shared across
// apps. Scope: only helpers that at least two apps genuinely need. Anything
// app-specific stays in the app.
package strings

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
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

// Fold normalizes s for accent-, case- and spacing-insensitive comparison and
// lookup keys: NFD-decomposes, drops combining marks, lowercases, and collapses
// every run of whitespace into a single space (trimmed). "  Ñandú   PÉREZ "
// folds to "nandu perez". It does not transliterate: letters without a
// decomposition (ß, ø, æ) are kept as lowercase.
func Fold(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	pendingSpace := false
	for _, r := range norm.NFD.String(s) {
		switch {
		case unicode.Is(unicode.Mn, r):
			continue
		case unicode.IsSpace(r):
			pendingSpace = b.Len() > 0
			continue
		}
		if pendingSpace {
			b.WriteByte(' ')
			pendingSpace = false
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}
