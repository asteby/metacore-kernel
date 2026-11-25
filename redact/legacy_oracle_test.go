package redact

import "regexp"

// legacyRule and legacyRules are a verbatim copy of the list that was
// duplicated in the two original emitters (identical in both, checked with a
// line diff when this test was written). They are the oracle for the parity
// tests: do not "improve" them.
type legacyRule struct {
	re   *regexp.Regexp
	repl string
}

var legacyRules = []legacyRule{
	{regexp.MustCompile(`(?i)\bbearer\s+[a-z0-9._~+/=-]{8,}`), "Bearer [REDACTED]"},
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}`), "[JWT]"},
	{regexp.MustCompile(`(?i)\b(postgres(ql)?|mysql|mongodb(\+srv)?|redis|amqp)://[^\s'"]+`), "$1://[REDACTED]"},
	{regexp.MustCompile(`(?i)\b(sk|pk|rk|lic|ghp|gho|ghs|xox[bpa]|whsec)_[A-Za-z0-9_-]{8,}`), "[KEY]"},
	{regexp.MustCompile(`(?i)("?(password|passwd|pwd|secret|token|api[_-]?key|authorization|access_token|refresh_token|client_secret)"?\s*[:=]\s*)("[^"]*"|'[^']*'|[^\s,&;}]+)`), "$1[REDACTED]"},
	{regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`), "[EMAIL]"},
	{regexp.MustCompile(`\b[A-ZÑ&]{4}\d{6}[HM][A-Z]{5}[A-Z0-9]\d\b`), "[CURP]"},
	{regexp.MustCompile(`\b[A-ZÑ&]{3,4}\d{6}[A-Z0-9]{3}\b`), "[RFC]"},
	{regexp.MustCompile(`\b(?:\d[ -]?){12,18}\d\b`), "[NUMBER]"},
	{regexp.MustCompile(`(\+\d{1,3}[\s-]?)?\(?\d{2,3}\)?[\s-]?\d{3,4}[\s-]?\d{4}\b`), "[PHONE]"},
	{regexp.MustCompile(`\b[A-Fa-f0-9]{40,}\b`), "[HEX]"},
	{regexp.MustCompile(`\b[A-Za-z0-9+/]{48,}={0,2}`), "[BLOB]"},
}

func legacyRedact(s string) string {
	if s == "" {
		return s
	}
	for _, r := range legacyRules {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	return s
}
