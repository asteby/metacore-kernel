package redact

import (
	"regexp"
	"strings"
)

// Rule is one redaction step: every non-empty match of Pattern for which
// Check (when set) returns true is replaced by
//
//	expand(Prefix) + marker + expand(Suffix)
//
// where Prefix and Suffix are regexp templates ($1, ${name}) that keep parts
// of the match (a key name, a URL scheme) and marker is "[redacted:<Name>]"
// unless overridden with WithMarker.
type Rule struct {
	// Name is the stable rule id used in markers, Only/Without and the specs.
	Name string
	// Pattern is a Go RE2 expression. It is matched against the whole text.
	Pattern *regexp.Regexp
	// Prefix and Suffix are expansion templates kept around the marker.
	Prefix, Suffix string
	// Check, when set, must accept the full match for it to be redacted
	// (used for checksums such as Luhn). It must be deterministic and cheap.
	Check func(match string) bool
	// CheckName documents Check for the portable spec (e.g. "luhn").
	CheckName string
	// Extra marks rules that are not part of Legacy().
	Extra bool
	// Optional rules are off unless enabled (WithIP) or selected by Only.
	Optional bool
	// LegacyMarker is the exact replacement text used by Legacy(); empty
	// means the typed marker.
	LegacyMarker string
}

// Built-in rule names.
const (
	RulePrivateKey     = "private_key"
	RuleURLCredentials = "url_credentials"
	RuleURLSignature   = "url_signature"
	RuleBearer         = "bearer"
	RuleBasicAuth      = "basic_auth"
	RuleJWT            = "jwt"
	RuleConnString     = "conn_string"
	RuleAPIKey         = "api_key"
	RuleProviderKey    = "provider_key"
	RuleSecret         = "secret"
	RuleSecretExt      = "secret_ext"
	RuleEmail          = "email"
	RuleIDPerson       = "id_person"
	RuleIDEntity       = "id_entity"
	RuleCard           = "card"
	RuleNumber         = "number"
	RuleIP             = "ip"
	RulePhone          = "phone"
	RuleHex            = "hex"
	RuleBlob           = "blob"
	RuleBlobURLSafe    = "blob_urlsafe"
)

func re(p string) *regexp.Regexp { return regexp.MustCompile(p) }

// builtin is the ordered default rule list. Order matters: the most specific
// patterns go first so a JWT is not half-eaten by the generic long-secret
// rule. Rules that are neither Extra nor Optional form the legacy list, in
// this order.
var builtin = []Rule{
	{Name: RulePrivateKey, Extra: true,
		Pattern: re(`-----BEGIN [A-Z ]*PRIVATE KEY-----[A-Za-z0-9+/=:,.\s]*(?:-----END [A-Z ]*PRIVATE KEY-----)?`)},
	{Name: RuleURLCredentials, Extra: true, Prefix: "$1", Suffix: "$2",
		Pattern: re(`\b([A-Za-z][A-Za-z0-9+.-]*://)[^\s/?#@'"]+(@)`)},
	{Name: RuleURLSignature, Extra: true, Prefix: "$1",
		Pattern: re(`(?i)([?&;](?:x-amz-signature|x-amz-credential|x-amz-security-token|x-goog-signature|x-goog-credential|signature|sig)=)[^&\s#'"]+`)},
	{Name: RuleBearer, Prefix: "Bearer ", LegacyMarker: "[REDACTED]",
		Pattern: re(`(?i)\bbearer\s+[a-z0-9._~+/=-]{8,}`)},
	{Name: RuleBasicAuth, Extra: true, Prefix: "Basic ", Check: checkBasic, CheckName: "basic_userpass",
		Pattern: re(`(?i)\bbasic\s+[a-z0-9+/_-]{8,}={0,2}`)},
	{Name: RuleJWT, LegacyMarker: "[JWT]",
		Pattern: re(`\beyJ[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}`)},
	{Name: RuleConnString, Prefix: "$1://", LegacyMarker: "[REDACTED]",
		Pattern: re(`(?i)\b(postgres(ql)?|mysql|mongodb(\+srv)?|redis|amqp)://[^\s'"]+`)},
	{Name: RuleAPIKey, LegacyMarker: "[KEY]",
		Pattern: re(`(?i)\b(sk|pk|rk|lic|ghp|gho|ghs|xox[bpa]|whsec)_[A-Za-z0-9_-]{8,}`)},
	{Name: RuleProviderKey, Extra: true,
		Pattern: re(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b|\bAIza[0-9A-Za-z_-]{35}\b|\bgithub_pat_[A-Za-z0-9_]{20,}|\bglpat-[A-Za-z0-9_-]{16,}|\bsk-[A-Za-z0-9_-]{16,}|\bnpm_[A-Za-z0-9]{30,}|\bxapp-[A-Za-z0-9-]{10,}|\bshp(?:at|ss|ca|pa)_[A-Fa-f0-9]{16,}|\bSG\.[A-Za-z0-9_-]{16,}\.[A-Za-z0-9_-]{16,}`)},
	{Name: RuleSecret, Prefix: "$1", LegacyMarker: "[REDACTED]",
		Pattern: re(`(?i)("?(password|passwd|pwd|secret|token|api[_-]?key|authorization|access_token|refresh_token|client_secret)"?\s*[:=]\s*)("[^"]*"|'[^']*'|[^\s,&;}]+)`)},
	{Name: RuleSecretExt, Extra: true, Prefix: "$1",
		Pattern: re(`(?i)("?(passphrase|private[_-]?key|x-api-key|id_token|session[_-]?id|set-cookie|cookie|credentials?|auth[_-]?token)"?\s*[:=]\s*)("[^"]*"|'[^']*'|[^\s,&;}]+)`)},
	{Name: RuleEmail, LegacyMarker: "[EMAIL]",
		Pattern: re(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)},
	// National person / entity identifiers of the Latin-American style:
	// 18 characters with a sex marker, and 12-13 characters tax numbers.
	{Name: RuleIDPerson,
		Pattern: re(`\b[A-Z\x{00D1}&]{4}\d{6}[HM][A-Z]{5}[A-Z0-9]\d\b`)},
	{Name: RuleIDEntity,
		Pattern: re(`\b[A-Z\x{00D1}&]{3,4}\d{6}[A-Z0-9]{3}\b`)},
	// Payment card numbers: 13-19 digits (optional space or dash separators)
	// that pass the Luhn checksum.
	{Name: RuleCard, Extra: true, Check: LuhnValid, CheckName: "luhn",
		Pattern: re(`\b(?:\d[ -]?){12,18}\d\b`)},
	// Any other long digit run of the same shape.
	{Name: RuleNumber, LegacyMarker: "[NUMBER]",
		Pattern: re(`\b(?:\d[ -]?){12,18}\d\b`)},
	{Name: RuleIP, Extra: true, Optional: true, Check: checkIP, CheckName: "ip",
		Pattern: re(`\b(?:\d{1,3}\.){3}\d{1,3}\b|(?:[0-9A-Fa-f]{0,4}:){2,7}[0-9A-Fa-f]{0,4}`)},
	// Phone numbers: +52 55 1234 5678, (555) 123-4567, 10 bare digits.
	{Name: RulePhone, LegacyMarker: "[PHONE]",
		Pattern: re(`(\+\d{1,3}[\s-]?)?\(?\d{2,3}\)?[\s-]?\d{3,4}[\s-]?\d{4}\b`)},
	{Name: RuleHex, LegacyMarker: "[HEX]",
		Pattern: re(`\b[A-Fa-f0-9]{40,}\b`)},
	{Name: RuleBlob, LegacyMarker: "[BLOB]",
		Pattern: re(`\b[A-Za-z0-9+/]{48,}={0,2}`)},
	{Name: RuleBlobURLSafe, Extra: true, Check: checkMixedBlob, CheckName: "mixed_alnum",
		Pattern: re(`\b[A-Za-z0-9_-]{48,}`)},
}

// BuiltinNames returns every built-in rule name in application order.
func BuiltinNames() []string {
	out := make([]string, len(builtin))
	for i, r := range builtin {
		out[i] = r.Name
	}
	return out
}

func marker(name string) string { return "[redacted:" + name + "]" }

// ---- options ----

type config struct {
	legacy        bool
	only          map[string]bool
	without       map[string]bool
	enable        map[string]bool
	custom        []Rule
	markers       map[string]string
	stripControls bool
	trim          bool
	maxInput      int
	maxBytes      int
	maxRunes      int
}

// Option configures New and String.
type Option func(*config)

// Legacy selects exactly the original shared rule list (no extra rules, the
// old markers such as "[EMAIL]" and "Bearer [REDACTED]"). The two identifier
// rules have no legacy marker here (it is niche vocabulary): hosts that must
// keep theirs pass WithMarker(RuleIDPerson, ...) and WithMarker(RuleIDEntity, ...).
func Legacy() Option { return func(c *config) { c.legacy = true } }

// Only restricts the engine to the named rules (order stays canonical).
// Naming an Optional rule enables it.
func Only(names ...string) Option {
	return func(c *config) {
		if c.only == nil {
			c.only = map[string]bool{}
		}
		for _, n := range names {
			c.only[n] = true
		}
	}
}

// Without disables the named rules.
func Without(names ...string) Option {
	return func(c *config) {
		if c.without == nil {
			c.without = map[string]bool{}
		}
		for _, n := range names {
			c.without[n] = true
		}
	}
}

// WithIP enables the IPv4 / IPv6 rule (off by default: addresses are often
// operational data worth keeping).
func WithIP() Option { return With(RuleIP) }

// With enables the named Optional rules.
func With(names ...string) Option {
	return func(c *config) {
		if c.enable == nil {
			c.enable = map[string]bool{}
		}
		for _, n := range names {
			c.enable[n] = true
		}
	}
}

// WithRule adds a custom rule. Custom rules run before the built-ins (they
// are assumed to be more specific); Name must be non-empty and Pattern set.
func WithRule(r Rule) Option {
	return func(c *config) {
		if r.Name != "" && r.Pattern != nil {
			c.custom = append(c.custom, r)
		}
	}
}

// WithMarker overrides the replacement text of a rule (a literal, not a
// template). It also overrides the legacy marker under Legacy().
func WithMarker(rule, text string) Option {
	return func(c *config) { c.markers[rule] = text }
}

// WithStripControls removes control characters (except \n and \t) first.
func WithStripControls() Option { return func(c *config) { c.stripControls = true } }

// WithTrim trims surrounding whitespace after redaction.
func WithTrim() Option { return func(c *config) { c.trim = true } }

// WithMaxInput bounds the bytes scanned; anything beyond is dropped, never
// passed through unscanned. 0 means unbounded.
func WithMaxInput(n int) Option { return func(c *config) { c.maxInput = n } }

// WithMaxBytes truncates the result to n bytes on a rune boundary.
func WithMaxBytes(n int) Option { return func(c *config) { c.maxBytes = n } }

// WithMaxRunes truncates the result to n runes.
func WithMaxRunes(n int) Option { return func(c *config) { c.maxRunes = n } }

func (c *config) build() *Engine {
	e := &Engine{
		legacy: c.legacy, stripControls: c.stripControls, trim: c.trim,
		maxInput: c.maxInput, maxBytes: c.maxBytes, maxRunes: c.maxRunes,
	}
	add := func(r Rule) {
		m := marker(r.Name)
		if c.legacy && r.LegacyMarker != "" {
			m = r.LegacyMarker
		}
		if o, ok := c.markers[r.Name]; ok {
			m = o
		}
		e.rules = append(e.rules, compiled{rule: r, marker: m})
	}
	for _, r := range c.custom {
		add(r)
	}
	for _, r := range builtin {
		if c.legacy && (r.Extra || r.Optional) {
			continue
		}
		if len(c.only) > 0 && !c.only[r.Name] {
			continue
		}
		if r.Optional && !c.enable[r.Name] && !c.only[r.Name] {
			continue
		}
		if c.without[r.Name] {
			continue
		}
		add(r)
	}
	return e
}

// ---- checks ----

// LuhnValid reports whether the digits of s (separators ignored) are 13 to 19
// long and pass the Luhn checksum.
func LuhnValid(s string) bool {
	n, sum, double := 0, 0, false
	for i := len(s) - 1; i >= 0; i-- {
		ch := s[i]
		if ch == ' ' || ch == '-' {
			continue
		}
		if ch < '0' || ch > '9' {
			return false
		}
		d := int(ch - '0')
		if double {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
		n++
	}
	return n >= 13 && n <= 19 && sum%10 == 0
}

// checkBasic accepts "Basic <b64>" only when the payload decodes to
// "user:pass", so prose such as "basic configuration" is left alone.
func checkBasic(m string) bool {
	f := strings.Fields(m)
	if len(f) < 2 {
		return false
	}
	return decodesToUserPass(f[len(f)-1])
}

func checkMixedBlob(m string) bool {
	digit, letter := false, false
	for i := 0; i < len(m); i++ {
		switch {
		case m[i] >= '0' && m[i] <= '9':
			digit = true
		case m[i] >= 'A' && m[i] <= 'Z', m[i] >= 'a' && m[i] <= 'z':
			letter = true
		}
	}
	return digit && letter
}
