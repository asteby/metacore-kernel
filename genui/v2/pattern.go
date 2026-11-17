package genuiv2

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"
)

// The schema's patterns are written for JavaScript (the SDK validates with
// them verbatim). Go's RE2 has no lookahead and its \s is ASCII-only, so each
// pattern is translated once: the lookaheads the schema uses become guards in
// code (same semantics, same order of failure: pattern) and \s becomes the
// JavaScript whitespace set.

const jsSpace = `\t\n\v\f\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}`

type compiledPattern struct {
	re     *regexp.Regexp
	guards []func(string) bool
}

func (c *compiledPattern) match(s string) bool {
	for _, g := range c.guards {
		if !g(s) {
			return false
		}
	}
	return c.re.MatchString(s)
}

var patternCache sync.Map // string -> *compiledPattern

func compilePattern(p string) *compiledPattern {
	if v, ok := patternCache.Load(p); ok {
		return v.(*compiledPattern)
	}
	c := translatePattern(p)
	patternCache.Store(p, c)
	return c
}

// hasForbiddenSegment reports whether any segment of s (split on seps) is a
// prototype key.
func hasForbiddenSegment(s, seps string) bool {
	for _, seg := range strings.FieldsFunc(s, func(r rune) bool { return strings.ContainsRune(seps, r) }) {
		if forbidden(seg) {
			return true
		}
	}
	// FieldsFunc drops empty segments, which are never forbidden keys.
	return false
}

func translatePattern(p string) *compiledPattern {
	out := &compiledPattern{}
	var b strings.Builder
	inClass := false
	for i := 0; i < len(p); i++ {
		ch := p[i]
		switch {
		case ch == '\\' && i+1 < len(p):
			if p[i+1] == 's' {
				if inClass {
					b.WriteString(jsSpace)
				} else {
					b.WriteString("[" + jsSpace + "]")
				}
			} else {
				b.WriteByte(ch)
				b.WriteByte(p[i+1])
			}
			i++
		case inClass:
			if ch == ']' {
				inClass = false
			}
			b.WriteByte(ch)
		case ch == '[':
			inClass = true
			b.WriteByte(ch)
			if i+1 < len(p) && p[i+1] == '^' {
				b.WriteByte('^')
				i++
			}
			// A leading ']' is literal in JS only as \]; the schema never uses it bare.
		case ch == '(' && i+2 < len(p) && p[i+1] == '?' && (p[i+2] == '!' || p[i+2] == '='):
			end := closeGroup(p, i)
			content := p[i : end+1]
			out.guards = append(out.guards, guardFor(content, p))
			i = end
		default:
			b.WriteByte(ch)
		}
	}
	out.re = regexp.MustCompile(b.String())
	return out
}

// closeGroup returns the index of the ')' that closes the group opened at i.
func closeGroup(p string, i int) int {
	depth := 0
	inClass := false
	for j := i; j < len(p); j++ {
		switch {
		case p[j] == '\\':
			j++
		case inClass:
			if p[j] == ']' {
				inClass = false
			}
		case p[j] == '[':
			inClass = true
		case p[j] == '(':
			depth++
		case p[j] == ')':
			depth--
			if depth == 0 {
				return j
			}
		}
	}
	panic(fmt.Sprintf("genui/v2: patron con grupo sin cerrar: %s", p))
}

func guardFor(group, whole string) func(string) bool {
	const protos = "__proto__|constructor|prototype"
	switch {
	case strings.HasPrefix(group, `(?!(?:.*[.:@])?(?:`+protos):
		return func(s string) bool { return !hasForbiddenSegment(s, ".:@") }
	case strings.HasPrefix(group, `(?!(?:.*\.)?(?:`+protos):
		return func(s string) bool { return !hasForbiddenSegment(s, ".") }
	case group == `(?!(?:`+protos+`)$)`:
		// Only used right after "asset:" in media.src.
		return func(s string) bool {
			if rest, ok := strings.CutPrefix(s, "asset:"); ok {
				return !forbidden(rest)
			}
			return true
		}
	case group == `(?!/)`:
		return func(s string) bool { return !strings.HasPrefix(s, "//") }
	case group == `(?=.{1,64}$)`:
		return func(s string) bool {
			n := utf8.RuneCountInString(s)
			return n >= 1 && n <= 64 && !strings.ContainsAny(s, "\n\r  ")
		}
	}
	panic(fmt.Sprintf("genui/v2: lookahead sin traduccion %q en %q", group, whole))
}
