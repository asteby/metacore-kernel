// Package redact strips secrets and personal data from free text.
//
// It is a small, stdlib-only primitive meant to be shared by every layer that
// writes text to a place the operator does not fully control (event streams,
// support cases, agent hand-offs, logs). Rules are composable regular
// expressions applied in order; each match is replaced by a typed marker such
// as "[redacted:email]". All patterns use Go's RE2 engine, so matching is
// linear in the input size: there is no catastrophic backtracking.
//
// An Engine is immutable after New and safe for concurrent use.
//
//	clean := redact.String(msg)                       // default rules
//	r := redact.New(redact.WithIP(), redact.WithMaxRunes(500))
//	agent.WithHandoffRedactor(redact.AsFunc(r))
//
// Legacy() reproduces, byte for byte, the rule list that used to be
// duplicated in the fleet event emitters, for hosts that must keep their
// stored markers unchanged. Language ports (PHP, TypeScript) validate against
// testdata/vectors.json and testdata/rules.json; see docs/redact.md.
package redact

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Redactor is anything that can scrub a string. *Engine implements it.
type Redactor interface {
	Redact(s string) string
}

// Func adapts a plain function to Redactor.
type Func func(string) string

// Redact calls f.
func (f Func) Redact(s string) string { return f(s) }

// AsFunc returns r.Redact as a plain func(string) string, the shape expected
// by hand-off and sink options that take a redaction callback. A nil r yields
// the default engine, so the result is never nil and never lets text through
// unscrubbed.
func AsFunc(r Redactor) func(string) string {
	if r == nil {
		r = Default()
	}
	return r.Redact
}

// Engine applies an ordered list of rules plus optional normalisation and
// size bounds. Build one with New and reuse it.
type Engine struct {
	rules         []compiled
	legacy        bool
	stripControls bool
	trim          bool
	maxInput      int
	maxBytes      int
	maxRunes      int
}

type compiled struct {
	rule   Rule
	marker string // literal replacement text appended after Prefix
}

// New builds an Engine. With no options it uses the default rule set.
func New(opts ...Option) *Engine {
	c := &config{markers: map[string]string{}}
	for _, o := range opts {
		o(c)
	}
	return c.build()
}

var defaultEngine = New()

// Default returns the shared engine built with no options.
func Default() *Engine { return defaultEngine }

// String redacts s. With no options it reuses the shared default engine.
func String(s string, opts ...Option) string {
	if len(opts) == 0 {
		return defaultEngine.Redact(s)
	}
	return New(opts...).Redact(s)
}

// Redact scrubs s: optional control-character stripping, the rules in order,
// then optional trimming and truncation. Truncation always runs last so a
// partially cut secret can never be exposed.
func (e *Engine) Redact(s string) string {
	if s == "" {
		return s
	}
	if e.maxInput > 0 && len(s) > e.maxInput {
		// Never emit text that was not scanned: drop the unscanned tail.
		s = cutBytes(s, e.maxInput)
	}
	if e.stripControls {
		s = StripControls(s)
	}
	for i := range e.rules {
		s = e.rules[i].apply(s)
	}
	if e.trim {
		s = strings.TrimSpace(s)
	}
	if e.maxBytes > 0 {
		s = TruncateBytes(s, e.maxBytes)
	}
	if e.maxRunes > 0 {
		s = TruncateRunes(s, e.maxRunes)
	}
	return s
}

// Rules lists the names of the active rules in application order.
func (e *Engine) Rules() []string {
	out := make([]string, len(e.rules))
	for i, c := range e.rules {
		out[i] = c.rule.Name
	}
	return out
}

func (c *compiled) apply(s string) string {
	re := c.rule.Pattern
	idx := re.FindAllStringSubmatchIndex(s, -1)
	if len(idx) == 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	last := 0
	for _, m := range idx {
		if m[1] == m[0] {
			continue
		}
		if c.rule.Check != nil && !c.rule.Check(s[m[0]:m[1]]) {
			continue
		}
		b.WriteString(s[last:m[0]])
		var tmp []byte
		if c.rule.Prefix != "" {
			tmp = re.ExpandString(tmp, c.rule.Prefix, s, m)
		}
		b.Write(tmp)
		b.WriteString(c.marker)
		if c.rule.Suffix != "" {
			b.Write(re.ExpandString(nil, c.rule.Suffix, s, m))
		}
		last = m[1]
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

// StripControls removes control characters except newline and tab.
func StripControls(s string) string {
	clean := true
	for _, r := range s {
		if r != '\n' && r != '\t' && unicode.IsControl(r) {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	return strings.Map(func(r rune) rune {
		if r != '\n' && r != '\t' && unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

// TruncateRunes keeps at most n runes of s. n <= 0 returns s unchanged.
func TruncateRunes(s string, n int) string {
	if n <= 0 || len(s) <= n { // len(s) >= rune count
		return s
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}

// TruncateBytes keeps at most n bytes of s without splitting a UTF-8 rune.
// n <= 0 returns s unchanged.
func TruncateBytes(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return cutBytes(s, n)
}

func cutBytes(s string, n int) string {
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
