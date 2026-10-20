package inputrules

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const (
	mpAccessTokenPattern = `^(APP_USR|TEST)-[0-9]{8,}-[0-9]{4,}-[0-9a-f]{32}-[0-9]+$`
	mpPublicKeyPattern   = `^(APP_USR|TEST)-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`
	mpAccessToken        = "APP_USR-1234567890123456-101912-0123456789abcdef0123456789abcdef-123456789"
	mpPublicKey          = "APP_USR-0a1b2c3d-4e5f-6789-abcd-ef0123456789" // 44 chars, the real-case paste
)

func intp(n int) *int { return &n }

func mpFields() (access, public FieldSpec) {
	access = FieldSpec{Key: "access_token", Label: "mercadopago.connector.field.access_token", Required: true, Rules: &Rules{
		Pattern:     mpAccessTokenPattern,
		PatternHint: "mercadopago.connector.hint.access_token",
		ForbidPatterns: []ForbidPattern{{
			Pattern: mpPublicKeyPattern,
			Message: "mercadopago.connector.error.public_key_in_access_token",
		}},
		Autocomplete: AutocompleteNewPasswrd,
	}}
	public = FieldSpec{Key: "public_key", Label: "mercadopago.connector.field.public_key", Rules: &Rules{
		Pattern:     mpPublicKeyPattern,
		PatternHint: "mercadopago.connector.hint.public_key",
	}}
	return
}

func TestCheckTable(t *testing.T) {
	access, public := mpFields()
	accessNoForbid := access
	accessNoForbid.Rules = &Rules{Pattern: mpAccessTokenPattern}
	uuidField := FieldSpec{Key: "id", Rules: &Rules{Format: FormatUUID}}
	httpsField := FieldSpec{Key: "base", Rules: &Rules{Format: FormatURLHTTPS}}
	lenField := FieldSpec{Key: "short", Rules: &Rules{MinLength: intp(3), MaxLength: intp(5)}}
	noTrim := FieldSpec{Key: "raw", Rules: &Rules{Trim: new(bool), Pattern: `^[a-z]+$`}}

	cases := []struct {
		name     string
		field    FieldSpec
		siblings []FieldSpec
		value    string
		code     string // "" = valid
		hint     string
	}{
		{"public key pasted in access_token", access, []FieldSpec{access, public}, mpPublicKey, CodeForbidden, "mercadopago.connector.error.public_key_in_access_token"},
		{"public key in access_token without forbid → looks like other field", accessNoForbid, []FieldSpec{accessNoForbid, public}, mpPublicKey, CodeLooksLikeOther, ""},
		{"valid access token", access, []FieldSpec{access, public}, mpAccessToken, "", ""},
		{"spaces and CRLF are trimmed", access, nil, "  \r\n" + mpAccessToken + " \n\t", "", ""},
		{"NBSP and zero-width are trimmed", access, nil, "\u00a0\u200b" + mpAccessToken + "\ufeff", "", ""},
		{"empty required", access, nil, "   \n", CodeRequired, ""},
		{"empty optional", public, nil, "", "", ""},
		{"garbage in access_token", access, []FieldSpec{access, public}, "hola", CodePattern, "mercadopago.connector.hint.access_token"},
		{"valid uuid", uuidField, nil, "0a1b2c3d-4e5f-6789-abcd-ef0123456789", "", ""},
		{"bad uuid", uuidField, nil, "0a1b2c3d", CodeFormat, ""},
		{"https ok", httpsField, nil, "https://api.example.com/v1", "", ""},
		{"http rejected for url_https", httpsField, nil, "http://api.example.com", CodeFormat, ""},
		{"too short", lenField, nil, "ab", CodeMinLength, ""},
		{"too long", lenField, nil, "abcdef", CodeMaxLength, ""},
		{"trim disabled keeps spaces", noTrim, nil, " abc", CodePattern, ""},
		{"hard cap", FieldSpec{Key: "big"}, nil, strings.Repeat("x", MaxValueLen+1), CodeMaxLength, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := Check(tc.field, tc.value, tc.siblings)
			if tc.code == "" {
				if v != nil {
					t.Fatalf("want valid, got %+v", v)
				}
				return
			}
			if v == nil || v.Code != tc.code {
				t.Fatalf("want %s, got %+v", tc.code, v)
			}
			if tc.hint != "" && v.Hint != tc.hint {
				t.Fatalf("hint = %q, want %q", v.Hint, tc.hint)
			}
			if v.Field != tc.field.Key {
				t.Fatalf("field = %q", v.Field)
			}
			// Never echo the value.
			raw, _ := json.Marshal(v)
			if strings.TrimSpace(tc.value) != "" && strings.Contains(string(raw), strings.TrimSpace(tc.value)) {
				t.Fatalf("violation echoes the value: %s", raw)
			}
		})
	}
}

func TestLooksLikeOtherFieldParams(t *testing.T) {
	access, public := mpFields()
	access.Rules.ForbidPatterns = nil
	v := Check(access, mpPublicKey, []FieldSpec{access, public})
	if v == nil || v.Code != CodeLooksLikeOther {
		t.Fatalf("got %+v", v)
	}
	if v.Params[ParamOtherField] != "public_key" || v.Params[ParamOtherLabel] != "mercadopago.connector.field.public_key" {
		t.Fatalf("params = %+v", v.Params)
	}
	// A sibling with only a format also counts.
	id := FieldSpec{Key: "client_id", Rules: &Rules{Format: FormatUUID}}
	secret := FieldSpec{Key: "secret", Rules: &Rules{Pattern: `^sk_[a-z0-9]+$`}}
	v = Check(secret, "0a1b2c3d-4e5f-6789-abcd-ef0123456789", []FieldSpec{id, secret})
	if v == nil || v.Code != CodeLooksLikeOther || v.Params[ParamOtherField] != "client_id" {
		t.Fatalf("got %+v", v)
	}
}

func TestNormalize(t *testing.T) {
	if got := Normalize(nil, " \r\nabc\n"); got != "abc" {
		t.Fatalf("nil rules must trim, got %q", got)
	}
	f := false
	if got := Normalize(&Rules{Trim: &f}, " a "); got != " a " {
		t.Fatalf("trim:false must keep, got %q", got)
	}
}

func TestValidatePattern(t *testing.T) {
	bad := map[string]string{
		"nested plus":       `^(a+)+$`,
		"nested star":       `(x*)*`,
		"nested repeat":     `^([0-9]{2,})+$`,
		"inline flag":       `(?i)abc`,
		"named group":       `(?P<x>a)`,
		"\\A":               `\Aabc`,
		"\\z":               `abc\z`,
		"\\p":               `\p{L}+`,
		"\\Q":               `\Q.\E`,
		"posix class":       `[[:alpha:]]`,
		"x brace":           `\x{41}`,
		"does not compile":  `([a-z]`,
		"backreference":     `(a)\1`,
		"empty":             ``,
		"too long":          "^" + strings.Repeat("a", MaxPatternLen) + "$",
		"lone backslash":    `abc\`,
		"lookahead (JS-ok)": `^(?=a)a$`,
	}
	for name, p := range bad {
		if err := ValidatePattern(p); err == nil {
			t.Errorf("%s: %q accepted", name, p)
		}
	}
	good := []string{`^(?:[0-9a-f]{4}-){3}[0-9a-f]{4}$`, `\(?abc`, mpAccessTokenPattern, mpPublicKeyPattern, `^(?:sk|rk)_(?:test|live)_[0-9A-Za-z]{10,}$`, `^whsec_[A-Za-z0-9+/=]{16,}$`, `[(?]`, `^\d{4}-\d{2}-\d{2}(\.[a-z]+)?$`, `^[^\s@]+$`}
	for _, p := range good {
		if err := ValidatePattern(p); err != nil {
			t.Errorf("%q rejected: %v", p, err)
		}
	}
}

// A hostile rule that slipped past the manifest validator is IGNORED at
// runtime (reported, never hangs, never blocks the user).
func TestHostileRuleIgnoredAtRuntime(t *testing.T) {
	f := FieldSpec{Key: "token", Required: true, Rules: &Rules{
		Pattern:        `^(a+)+$`,
		ForbidPatterns: []ForbidPattern{{Pattern: `(x*)*y`, Message: "m"}},
	}}
	value := strings.Repeat("a", 4000) + "!"
	done := make(chan struct{})
	var v *Violation
	var issues []RuleIssue
	go func() {
		v, issues = CheckDetailed(f, value, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("hostile pattern hung the checker")
	}
	if v != nil {
		t.Fatalf("invalid rules must be ignored, got %+v", v)
	}
	if len(issues) != 2 {
		t.Fatalf("want 2 ignored rules, got %v", issues)
	}
	for _, is := range issues {
		if strings.Contains(is.String(), "aaaa") {
			t.Fatalf("issue echoes the value: %s", is)
		}
	}
	if err := ValidatePattern(`^(a+)+$`); err == nil {
		t.Fatal("manifest validation must reject the hostile pattern")
	}
}

func TestValidateRules(t *testing.T) {
	access, _ := mpFields()
	r := *access.Rules
	r.Examples = []string{mpAccessToken}
	if errs := Validate(&r); len(errs) != 0 {
		t.Fatalf("valid rules rejected: %v", errs)
	}
	r.Examples = []string{mpPublicKey}
	if errs := Validate(&r); len(errs) != 1 || !strings.Contains(errs[0], "examples[0]") {
		t.Fatalf("forbidden example must fail: %v", errs)
	}
	bad := &Rules{MinLength: intp(10), MaxLength: intp(5), Format: "ip", Autocomplete: "on", Mask: "partial",
		ForbidPatterns: []ForbidPattern{{Pattern: `(?i)x`, Message: ""}}}
	errs := Validate(bad)
	for _, want := range []string{"min_length", "format", "autocomplete", "mask", "forbid_patterns[0].pattern", "forbid_patterns[0].message"} {
		found := false
		for _, e := range errs {
			if strings.HasPrefix(e, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("missing %s in %v", want, errs)
		}
	}
}
