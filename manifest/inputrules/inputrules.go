// Package inputrules is the canonical, dependency-free runtime of the v3
// manifest `input_rules` block: declarative input constraints for a setting,
// a connector credential or an action-modal field (trim, pattern, predefined
// formats, lengths, "this looks like the wrong value" detection and browser
// autofill hints).
//
// The manifest grammar (manifest/v3.InputRules) is an alias of Rules, so the
// grammar and this checker cannot drift. Hosts use Check / Normalize on the
// server (the source of truth when a value is saved); the SDK/UI runs the same
// rules live with the same codes.
//
// Safety:
//   - Patterns are restricted to the common subset of RE2 and ECMAScript (see
//     ValidatePattern) so the browser runs the same expression, and nested
//     unbounded quantifiers are rejected (ReDoS in backtracking engines). Go's
//     regexp is linear-time anyway.
//   - A rule that is invalid at runtime (it should never pass the manifest
//     validator) is IGNORED and reported as a RuleIssue: a broken addon rule
//     must never lock a user out of saving credentials.
//   - Violations never carry the checked value (no echo in responses/logs).
package inputrules

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"regexp/syntax"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// Limits shared with the JSON schema and the SDK.
const (
	MaxPatternLen  = 512
	MaxValueLen    = 4096
	MaxExamples    = 5
	MaxExampleLen  = 256
	MaxForbid      = 10
	MaxMessageLen  = 256
	MaxPatternHint = 256
)

// Violation codes (stable, locale-agnostic).
const (
	CodeRequired           = "required"
	CodeForbidden          = "forbidden"
	CodeLooksLikeOther     = "looks_like_other_field"
	CodePattern            = "pattern"
	CodeFormat             = "format"
	CodeMinLength          = "min_length"
	CodeMaxLength          = "max_length"
	ParamMin               = "min"
	ParamMax               = "max"
	ParamFormat            = "format"
	ParamOtherField        = "other_field"
	ParamOtherLabel        = "other_label"
	AutocompleteOff        = "off"
	AutocompleteNewPasswrd = "new-password"
	MaskNone               = "none"
	MaskFull               = "full"
)

// Predefined formats.
const (
	FormatUUID     = "uuid"
	FormatURL      = "url"
	FormatURLHTTPS = "url_https"
	FormatEmail    = "email"
	FormatHex      = "hex"
	FormatNumeric  = "numeric"
	FormatAlnum    = "alnum"
)

// Formats lists every predefined format, in schema order.
var Formats = []string{FormatUUID, FormatURL, FormatURLHTTPS, FormatEmail, FormatHex, FormatNumeric, FormatAlnum}

// Rules is the `input_rules` block. Every field is optional.
type Rules struct {
	// Trim strips leading/trailing whitespace (incl. CR/LF, NBSP and
	// zero-width chars) before validating and when storing. Nil = true.
	Trim *bool `json:"trim,omitempty"`
	// Pattern the (trimmed) value must match. Max 512 chars, RE2∩ECMAScript.
	Pattern string `json:"pattern,omitempty"`
	// PatternHint is an i18n key of the addon (or literal text) describing
	// the expected format, shown under the field and with pattern errors.
	PatternHint string `json:"pattern_hint,omitempty"`
	// Examples of valid values (max 5); the manifest validator checks them.
	Examples []string `json:"examples,omitempty"`
	// MinLength / MaxLength bound the trimmed value length in runes.
	MinLength *int `json:"min_length,omitempty"`
	MaxLength *int `json:"max_length,omitempty"`
	// Format is one of Formats.
	Format string `json:"format,omitempty"`
	// Autocomplete is the browser autofill hint: off | new-password.
	Autocomplete string `json:"autocomplete,omitempty"`
	// Mask: none (plain) | full (password with show/hide).
	Mask string `json:"mask,omitempty"`
	// ForbidPatterns flag a value with the shape of SOMETHING ELSE (e.g. a
	// public key pasted into the access-token field). Max 10.
	ForbidPatterns []ForbidPattern `json:"forbid_patterns,omitempty"`
}

// ForbidPattern rejects a value matching Pattern with Message (i18n key of
// the addon or literal text).
type ForbidPattern struct {
	Pattern string `json:"pattern"`
	Message string `json:"message"`
}

// TrimEnabled reports whether the value must be trimmed (default true).
func (r *Rules) TrimEnabled() bool { return r == nil || r.Trim == nil || *r.Trim }

// FieldSpec is one field the checker validates: its key, its label (an i18n
// key, echoed in looks_like_other_field params), whether it is required and
// its rules (nil = only the required check).
type FieldSpec struct {
	Key      string
	Label    string
	Required bool
	Rules    *Rules
}

// Violation is the first rule a value breaks. It never carries the value.
type Violation struct {
	Field string `json:"field"`
	Code  string `json:"code"`
	// Hint is the addon-provided i18n key / literal that explains the error:
	// the forbid_patterns message for "forbidden", the pattern_hint for
	// "pattern"/"format"/"looks_like_other_field" when declared.
	Hint   string         `json:"hint,omitempty"`
	Params map[string]any `json:"params,omitempty"`
}

// RuleIssue reports a rule that was ignored at runtime because it is invalid
// (bad or unsafe regex). Carries no value — safe to log.
type RuleIssue struct {
	Field  string
	Rule   string
	Reason string
}

func (i RuleIssue) String() string {
	return fmt.Sprintf("input_rules %s.%s ignored: %s", i.Field, i.Rule, i.Reason)
}

// isTrimmable is whitespace plus the invisible chars a copy/paste drags along.
func isTrimmable(r rune) bool {
	switch r {
	case '\u200b', '\u200c', '\u200d', '\u2060', '\ufeff':
		return true
	}
	return unicode.IsSpace(r)
}

// Normalize returns the value as the host must validate and store it: trimmed
// unless the rules disable it. Nil rules still trim (the default).
func Normalize(r *Rules, value string) string {
	if !r.TrimEnabled() {
		return value
	}
	return strings.TrimFunc(value, isTrimmable)
}

// ── pattern safety ──────────────────────────────────────────────────────────

var patternCache sync.Map // pattern → *regexp.Regexp | error

// ValidatePattern reports whether p is a usable pattern: non-empty, ≤ 512
// chars, compiles as Go RE2, stays inside the ECMAScript-compatible subset and
// has no nested unbounded quantifier.
func ValidatePattern(p string) error {
	_, err := compile(p)
	return err
}

func compile(p string) (*regexp.Regexp, error) {
	if v, ok := patternCache.Load(p); ok {
		if re, ok := v.(*regexp.Regexp); ok {
			return re, nil
		}
		return nil, v.(error)
	}
	re, err := compileUncached(p)
	if err != nil {
		patternCache.Store(p, err)
		return nil, err
	}
	patternCache.Store(p, re)
	return re, nil
}

func compileUncached(p string) (*regexp.Regexp, error) {
	if strings.TrimSpace(p) == "" {
		return nil, errors.New("pattern is empty")
	}
	if len(p) > MaxPatternLen {
		return nil, fmt.Errorf("pattern is too long (%d chars, max %d)", len(p), MaxPatternLen)
	}
	if err := checkPortable(p); err != nil {
		return nil, err
	}
	parsed, err := syntax.Parse(p, syntax.Perl)
	if err != nil {
		return nil, fmt.Errorf("pattern does not compile: %v", err)
	}
	if nestedQuantifier(parsed, false) {
		return nil, errors.New("pattern has a nested unbounded quantifier such as (a+)+ (unsafe in browsers: ReDoS)")
	}
	re, err := regexp.Compile(p)
	if err != nil {
		return nil, fmt.Errorf("pattern does not compile: %v", err)
	}
	return re, nil
}

// checkPortable rejects RE2-only (or JS-only) syntax so the same expression
// behaves identically in the browser.
func checkPortable(p string) error {
	inClass := false
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c == '\\':
			if i+1 >= len(p) {
				return errors.New("pattern ends with a lone backslash")
			}
			n := p[i+1]
			switch n {
			case 'A', 'z', 'Q', 'E', 'C', 'p', 'P':
				return fmt.Errorf(`pattern uses \%c, which is not portable between Go and JavaScript`, n)
			case 'x':
				if i+2 < len(p) && p[i+2] == '{' {
					return errors.New(`pattern uses \x{...}, which is not portable between Go and JavaScript (use \xHH)`)
				}
			}
			i++
		case inClass:
			if c == '[' && i+1 < len(p) && p[i+1] == ':' {
				return errors.New("pattern uses a POSIX class [[:...:]], which JavaScript does not support")
			}
			if c == ']' {
				inClass = false
			}
		case c == '[':
			inClass = true
			// A leading ']' or '^]' is literal inside the class.
			if i+1 < len(p) && p[i+1] == '^' {
				i++
			}
			if i+1 < len(p) && p[i+1] == ']' {
				i++
			}
			if i+1 < len(p) && p[i+1] == '[' && i+2 < len(p) && p[i+2] == ':' {
				return errors.New("pattern uses a POSIX class [[:...:]], which JavaScript does not support")
			}
		case c == '(' && i+1 < len(p) && p[i+1] == '?':
			if i+2 >= len(p) || p[i+2] != ':' {
				return errors.New("pattern uses an inline flag or special group (?...); only (?:...) is allowed")
			}
		}
	}
	return nil
}

// isMulti reports a repetition that can match more than once (*, +, {n,},
// {n,m} with m>1).
func isMulti(re *syntax.Regexp) bool {
	switch re.Op {
	case syntax.OpStar, syntax.OpPlus:
		return true
	case syntax.OpRepeat:
		return re.Max == -1 || re.Max > 1
	}
	return false
}

// isUnbounded reports a repetition with no upper bound (*, +, {n,}).
func isUnbounded(re *syntax.Regexp) bool {
	switch re.Op {
	case syntax.OpStar, syntax.OpPlus:
		return true
	case syntax.OpRepeat:
		return re.Max == -1
	}
	return false
}

// nestedQuantifier reports a multi-match repetition inside an UNBOUNDED one
// ((a+)+, (x*)*, ([0-9]{2})+). A bounded outer repetition such as
// (?:[0-9a-f]{4}-){3} is fine: it cannot backtrack exponentially.
func nestedQuantifier(re *syntax.Regexp, insideUnbounded bool) bool {
	if insideUnbounded && isMulti(re) {
		return true
	}
	for _, sub := range re.Sub {
		if nestedQuantifier(sub, insideUnbounded || isUnbounded(re)) {
			return true
		}
	}
	return false
}

// ── formats ────────────────────────────────────────────────────────────────

var (
	reUUID    = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	reEmail   = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)
	reHex     = regexp.MustCompile(`^[0-9a-fA-F]+$`)
	reNumeric = regexp.MustCompile(`^[0-9]+$`)
	reAlnum   = regexp.MustCompile(`^[A-Za-z0-9]+$`)
)

// KnownFormat reports whether f is a predefined format.
func KnownFormat(f string) bool {
	for _, k := range Formats {
		if k == f {
			return true
		}
	}
	return false
}

// MatchFormat reports whether v satisfies the predefined format f. An unknown
// format matches (ignored rule; the manifest validator rejects it).
func MatchFormat(f, v string) bool {
	switch f {
	case FormatUUID:
		return reUUID.MatchString(v)
	case FormatEmail:
		return reEmail.MatchString(v)
	case FormatHex:
		return reHex.MatchString(v)
	case FormatNumeric:
		return reNumeric.MatchString(v)
	case FormatAlnum:
		return reAlnum.MatchString(v)
	case FormatURL, FormatURLHTTPS:
		if strings.ContainsFunc(v, unicode.IsSpace) {
			return false
		}
		u, err := url.Parse(v)
		if err != nil || u.Host == "" {
			return false
		}
		if f == FormatURLHTTPS {
			return u.Scheme == "https"
		}
		return u.Scheme == "http" || u.Scheme == "https"
	}
	return true
}

// ── checking ───────────────────────────────────────────────────────────────

// Check validates value (raw, as received) against f and returns the first
// violation, or nil. siblings are the other fields of the same form (e.g. the
// connector's other credentials) used to detect a value pasted into the wrong
// field. Invalid rules are ignored (see CheckDetailed to get them reported).
func Check(f FieldSpec, value string, siblings []FieldSpec) *Violation {
	v, _ := CheckDetailed(f, value, siblings)
	return v
}

// CheckDetailed is Check plus the list of rules ignored because they are
// invalid (to log, without the value).
func CheckDetailed(f FieldSpec, value string, siblings []FieldSpec) (*Violation, []RuleIssue) {
	var issues []RuleIssue
	r := f.Rules
	v := Normalize(r, value)
	if v == "" {
		if f.Required {
			return &Violation{Field: f.Key, Code: CodeRequired}, nil
		}
		return nil, nil
	}
	n := utf8.RuneCountInString(v)
	if n > MaxValueLen {
		return &Violation{Field: f.Key, Code: CodeMaxLength, Params: map[string]any{ParamMax: MaxValueLen}}, nil
	}
	if r == nil {
		return nil, nil
	}
	for i, fp := range r.ForbidPatterns {
		re, err := compile(fp.Pattern)
		if err != nil {
			issues = append(issues, RuleIssue{Field: f.Key, Rule: fmt.Sprintf("forbid_patterns[%d]", i), Reason: err.Error()})
			continue
		}
		if re.MatchString(v) {
			return &Violation{Field: f.Key, Code: CodeForbidden, Hint: fp.Message}, issues
		}
	}
	var shapeFail *Violation
	if r.MinLength != nil && n < *r.MinLength {
		shapeFail = &Violation{Field: f.Key, Code: CodeMinLength, Hint: r.PatternHint, Params: map[string]any{ParamMin: *r.MinLength}}
	} else if r.MaxLength != nil && *r.MaxLength > 0 && n > *r.MaxLength {
		shapeFail = &Violation{Field: f.Key, Code: CodeMaxLength, Hint: r.PatternHint, Params: map[string]any{ParamMax: *r.MaxLength}}
	}
	if shapeFail == nil && r.Format != "" {
		if !KnownFormat(r.Format) {
			issues = append(issues, RuleIssue{Field: f.Key, Rule: "format", Reason: fmt.Sprintf("unknown format %q", r.Format)})
		} else if !MatchFormat(r.Format, v) {
			shapeFail = &Violation{Field: f.Key, Code: CodeFormat, Hint: r.PatternHint, Params: map[string]any{ParamFormat: r.Format}}
		}
	}
	if shapeFail == nil && r.Pattern != "" {
		re, err := compile(r.Pattern)
		if err != nil {
			issues = append(issues, RuleIssue{Field: f.Key, Rule: "pattern", Reason: err.Error()})
		} else if !re.MatchString(v) {
			shapeFail = &Violation{Field: f.Key, Code: CodePattern, Hint: r.PatternHint}
		}
	}
	if shapeFail == nil {
		return nil, issues
	}
	if other := looksLike(f.Key, v, siblings); other != nil {
		return &Violation{Field: f.Key, Code: CodeLooksLikeOther, Hint: r.PatternHint, Params: map[string]any{
			ParamOtherField: other.Key,
			ParamOtherLabel: other.Label,
		}}, issues
	}
	return shapeFail, issues
}

// looksLike returns the first sibling (other than key) whose declared shape
// (format and/or pattern) the value satisfies.
func looksLike(key, v string, siblings []FieldSpec) *FieldSpec {
	for i := range siblings {
		s := &siblings[i]
		if s.Key == key || s.Rules == nil || (s.Rules.Pattern == "" && s.Rules.Format == "") {
			continue
		}
		if matchesShape(s.Rules, v) {
			return s
		}
	}
	return nil
}

// matchesShape: every declared shape rule of r accepts v (and no forbid
// pattern rejects it). Invalid rules make the shape unknown → false.
func matchesShape(r *Rules, v string) bool {
	n := utf8.RuneCountInString(v)
	if r.MinLength != nil && n < *r.MinLength {
		return false
	}
	if r.MaxLength != nil && *r.MaxLength > 0 && n > *r.MaxLength {
		return false
	}
	for _, fp := range r.ForbidPatterns {
		re, err := compile(fp.Pattern)
		if err == nil && re.MatchString(v) {
			return false
		}
	}
	if r.Format != "" && (!KnownFormat(r.Format) || !MatchFormat(r.Format, v)) {
		return false
	}
	if r.Pattern != "" {
		re, err := compile(r.Pattern)
		if err != nil || !re.MatchString(v) {
			return false
		}
	}
	return true
}

// ── manifest-time validation ───────────────────────────────────────────────

// Validate returns the problems of a rules block as relative messages
// ("pattern: ...", "examples[1]: ..."); the caller prefixes the field path.
func Validate(r *Rules) []string {
	if r == nil {
		return nil
	}
	var errs []string
	if r.Pattern != "" {
		if err := ValidatePattern(r.Pattern); err != nil {
			errs = append(errs, "pattern: "+err.Error())
		}
	}
	if len(r.PatternHint) > MaxPatternHint {
		errs = append(errs, fmt.Sprintf("pattern_hint: too long (max %d)", MaxPatternHint))
	}
	if len(r.ForbidPatterns) > MaxForbid {
		errs = append(errs, fmt.Sprintf("forbid_patterns: too many (%d, max %d)", len(r.ForbidPatterns), MaxForbid))
	}
	for i, fp := range r.ForbidPatterns {
		if err := ValidatePattern(fp.Pattern); err != nil {
			errs = append(errs, fmt.Sprintf("forbid_patterns[%d].pattern: %v", i, err))
		}
		if strings.TrimSpace(fp.Message) == "" {
			errs = append(errs, fmt.Sprintf("forbid_patterns[%d].message: is empty", i))
		}
	}
	if r.MinLength != nil && *r.MinLength < 0 {
		errs = append(errs, "min_length: must be >= 0")
	}
	if r.MaxLength != nil && (*r.MaxLength < 1 || *r.MaxLength > MaxValueLen) {
		errs = append(errs, fmt.Sprintf("max_length: must be between 1 and %d", MaxValueLen))
	}
	if r.MinLength != nil && r.MaxLength != nil && *r.MinLength > *r.MaxLength {
		errs = append(errs, fmt.Sprintf("min_length (%d) is greater than max_length (%d)", *r.MinLength, *r.MaxLength))
	}
	if r.Format != "" && !KnownFormat(r.Format) {
		errs = append(errs, fmt.Sprintf("format: %q is not one of %s", r.Format, strings.Join(Formats, "|")))
	}
	switch r.Autocomplete {
	case "", AutocompleteOff, AutocompleteNewPasswrd:
	default:
		errs = append(errs, fmt.Sprintf("autocomplete: %q is not one of off|new-password", r.Autocomplete))
	}
	switch r.Mask {
	case "", MaskNone, MaskFull:
	default:
		errs = append(errs, fmt.Sprintf("mask: %q is not one of none|full", r.Mask))
	}
	if len(r.Examples) > MaxExamples {
		errs = append(errs, fmt.Sprintf("examples: too many (%d, max %d)", len(r.Examples), MaxExamples))
	}
	if len(errs) > 0 {
		// Examples are only meaningful against valid rules.
		return errs
	}
	for i, ex := range r.Examples {
		if len(ex) > MaxExampleLen {
			errs = append(errs, fmt.Sprintf("examples[%d]: too long (max %d)", i, MaxExampleLen))
			continue
		}
		if Normalize(r, ex) == "" {
			errs = append(errs, fmt.Sprintf("examples[%d]: is empty", i))
			continue
		}
		// The example is the addon author's own text, safe to name the code.
		if v := Check(FieldSpec{Key: "example", Rules: r}, ex, nil); v != nil {
			errs = append(errs, fmt.Sprintf("examples[%d]: does not pass its own rules (%s)", i, v.Code))
		}
	}
	return errs
}
