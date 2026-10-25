package diag

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func has(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// Vectores dorados hostiles de ExtractPaths: lo que NUNCA debe salir.
func TestExtractPaths_Hostile(t *testing.T) {
	deny := []struct{ name, text string }{
		{"email", "user john.doe@example.com is invalid"},
		{"email-local-part", "send to john.doe@corp.mx now"},
		{"url", "see https://api.provider.com/v1/payments/payer.email for details"},
		{"url-host", "GET https://api.provider.com failed"},
		{"bare-domain", "contact support.example.com or help.example.org"},
		{"file", "cannot open config.json or report.pdf"},
		{"windows-path", `C:\Users\bob\notes.txt`},
		{"percent", "q=john%2Edoe.name%40mail"},
		{"fragment", "page#payer.email"},
		{"query", "a=1&payer.email=x"},
		{"tilde", "~root.profile"},
		{"plus", "a+b.c+d"},
		{"star", "*.example"},
		{"hyphen-domain", "my-shop.payer.email"},
		{"phrase", "Invalid value for the field. Please retry later."},
		{"single-word", "email amount name password"},
		{"initials", "e.g. i.e. a.b"},
		{"pan", "card 4111 1111 1111 1111 declined"},
		{"digit-run", "order_123456789 not found"},
		{"version", "v2.0.1 deprecated 1.5"},
		{"uppercase-start", "Payer.Email rejected"},
		{"ip", "connect 10.0.0.1 failed"},
	}
	for _, c := range deny {
		if got := ExtractPaths(c.text); len(got) != 0 {
			t.Errorf("%s: %q => %v, want none", c.name, c.text, got)
		}
	}
	allow := []struct {
		text string
		want []string
	}{
		{"invalid parameter payer.identification.number.", []string{"payer.identification.number"}},
		{"bad items[0].unit_price and transaction_amount", []string{"items[0].unit_price", "transaction_amount"}},
		{"payer.firstName is required", []string{"payer.first_name"}},
		{"cannot be null: card_token_id,", []string{"card_token_id"}},
	}
	for _, c := range allow {
		got := ExtractPaths(c.text)
		for _, w := range c.want {
			if !has(got, w) {
				t.Errorf("%q => %v, missing %q", c.text, got, w)
			}
		}
	}
}

func TestExtractBody_ProviderStyle(t *testing.T) {
	body := `{"message":"invalid parameter payer.email for ana@example.com","error":"bad_request","status":400,
	"cause":[{"code":2067,"description":"Invalid user identification number 4111111111111111","field":"payer.identification.number"},
	{"code":"property_value","description":"free text with spaces"}]}`
	f := ExtractBody([]byte(body), 400)
	if !has(f.ProviderCodes, "bad_request") || !has(f.ProviderCodes, "property_value") {
		t.Fatalf("codes=%v", f.ProviderCodes)
	}
	if !has(f.Causes, "2067") || has(f.Causes, "400") {
		t.Fatalf("causes=%v", f.Causes)
	}
	if !has(f.Props, "payer.identification.number") || !has(f.Props, "payer.email") {
		t.Fatalf("props=%v", f.Props)
	}
	if f.Shape != "cause:array,error:string,message:string,status:number" {
		t.Fatalf("shape=%q", f.Shape)
	}
	for _, p := range f.Props {
		if strings.Contains(p, "example") || strings.Contains(p, "ana") {
			t.Fatalf("value leaked in prop %q", p)
		}
	}
	// Non-JSON y vacio.
	if f := ExtractBody([]byte("<html><body>502 Bad Gateway payer.email</body></html>"), 502); !has(f.ProviderCodes, "non_json_body") || len(f.Props) != 0 || f.Shape != "" {
		t.Fatalf("html=%+v", f)
	}
	if f := ExtractBody(nil, 500); !has(f.ProviderCodes, "empty_body") {
		t.Fatalf("empty=%+v", f)
	}
	// Claves hostiles en el esqueleto: ni emails ni digitos largos ni mayusculas sueltas.
	f = ExtractBody([]byte(`{"ana@example.com":1,"tok_1234567890":"x","ok_key":true,"errorCode":"E1"}`), 400)
	if f.Shape != "error_code:string,ok_key:bool" {
		t.Fatalf("shape=%q", f.Shape)
	}
}

func TestScrubText_Vectors(t *testing.T) {
	sc := NewScrubber("sk_live_TOPSECRET9", "Bearer abcDEF123456ghi")
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	in := "key=sk_live_TOPSECRET9 hdr=Bearer abcDEF123456ghi mail ana.l@example.com card 4111-1111-1111-1111 jwt " + jwt +
		" ref 98765432109 url https://api.x.com/v1/pay?token=zzz#f tok=AbCdEf0123456789AbCdEf0123456789 ok payer.email"
	out := ScrubText(in, sc)
	for _, leak := range []string{"sk_live_TOPSECRET9", "abcDEF123456ghi", "ana.l@example.com", "4111", "eyJ", "98765432109", "/v1/pay", "token=zzz", "AbCdEf0123456789"} {
		if strings.Contains(out, leak) {
			t.Errorf("leaked %q in %q", leak, out)
		}
	}
	if !strings.Contains(out, "payer.email") || !strings.Contains(out, "https://api.x.com") {
		t.Errorf("over-scrubbed: %q", out)
	}
	if ScrubText(out, sc) != out {
		t.Errorf("not idempotent: %q => %q", out, ScrubText(out, sc))
	}
}

func TestScrubber_Variants(t *testing.T) {
	sc := NewScrubber()
	sc.AddHeaders(map[string]string{"Authorization": "Basic dXNlcjpwYXNzd29yZDEyMw==", "X-Api-Key": "k-9f8e7d6c", "Content-Type": "application/json"})
	for _, s := range []string{"user:password123", "password123", "k-9f8e7d6c", "dXNlcjpwYXNzd29yZDEyMw=="} {
		if !sc.Contains("x " + s + " y") {
			t.Errorf("should know %q", s)
		}
	}
	if sc.Contains("application/json") {
		t.Error("content-type is not a secret")
	}
	var nilS *Scrubber
	if nilS.Contains("x") || nilS.Redact("x") != "x" {
		t.Error("nil scrubber must be a no-op")
	}
}

func TestSanitize_AllowlistPerField(t *testing.T) {
	sc := NewScrubber("topsecret-value")
	org := uuid.New()
	d := Diagnostic{
		Org: org, AddonKey: "Bad Key!", AddonVersion: "1.2.3", Kind: "weird", Operation: "charge payer@x.com",
		CorrelationID: "corr_1", InvocationID: "4111111111111111", Severity: "nuclear", Code: "Invalid Card Number 4111",
		HTTPStatus: 999, ProviderCode: []string{"INVALID_REQUEST", "has space", "a1234567", "topsecret-value", "ok_code"},
		Causes: []string{"2067", "12345678", "abc", "7"}, Props: []string{"payer.email", "john@x.com", "a/b", "topsecret-value", "order_123456789", "Payer.Email", "payer.email"},
		Shape: "ok:string,bad key:string,ana@x.com:number,n:weird,z_k:array,ok:number", Subject: Subject{Type: "payment", ID: "pay_42"},
		UpstreamHost: "https://user:pw@Evil.Example.com:8080/path?x=1", DiagRef: "dg_zz",
		Breadcrumbs: []string{"hi topsecret-value ana@x.com 4111111111111111", "", strings.Repeat("a b ", 100)},
	}
	o := Sanitize(d, sc)
	if o.AddonKey != "" || o.Kind != KindRuntime || o.Operation != "" || o.InvocationID != "" || o.Severity != SeverityError {
		t.Errorf("scalars: %+v", o)
	}
	if o.Code != "unknown_error" || o.HTTPStatus != 0 {
		t.Errorf("code/status: %q %d", o.Code, o.HTTPStatus)
	}
	if got := strings.Join(o.ProviderCode, ","); got != "invalid_request,ok_code" {
		t.Errorf("provider_code=%q", got)
	}
	if got := strings.Join(o.Causes, ","); got != "2067,7" {
		t.Errorf("causes=%q", got)
	}
	if got := strings.Join(o.Props, ","); got != "payer.email" {
		t.Errorf("props=%q", got)
	}
	if o.Shape != "ok:number,z_k:array" && o.Shape != "ok:string,z_k:array" {
		t.Errorf("shape=%q", o.Shape)
	}
	if o.UpstreamHost != "evil.example.com" {
		t.Errorf("host=%q", o.UpstreamHost)
	}
	if o.DiagRef != "" || o.Subject != d.Subject || o.CorrelationID != "corr_1" {
		t.Errorf("misc: %+v", o)
	}
	if len(o.Breadcrumbs) != 2 || strings.Contains(o.Breadcrumbs[0], "topsecret") || strings.Contains(o.Breadcrumbs[0], "@") || len([]rune(o.Breadcrumbs[1])) > MaxBreadcrumbLen {
		t.Errorf("breadcrumbs=%v", o.Breadcrumbs)
	}
}

func TestSanitize_BreadcrumbCapKeepsLast(t *testing.T) {
	var bc []string
	for i := 0; i < 50; i++ {
		bc = append(bc, "line "+strings.Repeat("x", i%3)+string(rune('a'+i%26)))
	}
	o := Sanitize(Diagnostic{Org: uuid.New(), AddonKey: "a", Breadcrumbs: bc}, nil)
	if len(o.Breadcrumbs) != MaxBreadcrumbs || o.Breadcrumbs[MaxBreadcrumbs-1] != bc[49] {
		t.Fatalf("got %d", len(o.Breadcrumbs))
	}
}

func TestFingerprint_StableAndSensitive(t *testing.T) {
	base := Sanitize(Diagnostic{Org: uuid.New(), AddonKey: "acme", AddonVersion: "1.2.3", Kind: KindHTTP, Operation: "pay", Code: "provider_error",
		HTTPStatus: 400, ProviderCode: []string{"b", "a"}, Props: []string{"yy.bb", "xx.aa"}}, nil)
	fp := Fingerprint(base)
	same := base
	same.AddonVersion = "1.9.0"
	same.ProviderCode = []string{"a", "b"}
	same.Props = []string{"xx.aa", "yy.bb"}
	same.Breadcrumbs = []string{"different"}
	same.At = time.Now()
	if Fingerprint(same) != fp {
		t.Error("minor version, ordering, breadcrumbs and time must not change the fingerprint")
	}
	for name, mut := range map[string]func(*Diagnostic){
		"major":  func(d *Diagnostic) { d.AddonVersion = "2.0.0" },
		"status": func(d *Diagnostic) { d.HTTPStatus = 422 },
		"props":  func(d *Diagnostic) { d.Props = []string{"xx.aa"} },
		"op":     func(d *Diagnostic) { d.Operation = "refund" },
		"code":   func(d *Diagnostic) { d.Code = "other" },
		"kind":   func(d *Diagnostic) { d.Kind = KindRuntime },
		"pcode":  func(d *Diagnostic) { d.ProviderCode = []string{"a"} },
	} {
		d := base
		mut(&d)
		if Fingerprint(d) == fp {
			t.Errorf("%s must change the fingerprint", name)
		}
	}
}

func TestFromEnvelope(t *testing.T) {
	d, ok := FromEnvelope([]byte(`{"success":false,"error":{"code":"Provider_Error","message":"payer.email is invalid for ana@x.com"},"meta":{}}`))
	if !ok || d.Code != "provider_error" || d.Kind != KindEnvelope || !has(d.Props, "payer.email") || len(d.Props) != 1 {
		t.Fatalf("%+v ok=%v", d, ok)
	}
	for _, raw := range []string{`{"success":true,"data":{}}`, `{"ok":1}`, `[1]`, ``, `{"success":false}`, `not json`, `{"success":"false","error":{}}`} {
		if _, ok := FromEnvelope([]byte(raw)); ok {
			t.Errorf("%q must not be an error envelope", raw)
		}
	}
	d, ok = FromEnvelope([]byte(`{"success":false,"error":"something went wrong with payer.email"}`))
	if !ok || d.Code != "unknown_error" {
		t.Fatalf("%+v", d)
	}
	d, _ = FromEnvelope([]byte(`{"success":false,"error":{"code":"insufficient funds please retry","message":"x"}}`))
	if d.Code != "unknown_error" {
		t.Fatalf("a phrase is not a code: %q", d.Code)
	}
}

func TestHostOf(t *testing.T) {
	for in, want := range map[string]string{
		"https://API.Example.com:8443/a/b?c=d": "api.example.com", "example.com/path": "example.com",
		"http://127.0.0.1/x": "", "http://localhost/": "", "": "", "https://a.b/": "", "ftp://h.example.org/f": "h.example.org",
		"https://12345678.example.com": "",
	} {
		if got := HostOf(in); got != want {
			t.Errorf("HostOf(%q)=%q want %q", in, got, want)
		}
	}
}

// --- Fuzz (Go native; CI runs each for a few seconds) ---

var (
	pathRe = regexp.MustCompile(`^[a-z][a-z0-9_]*(\[[0-9]{1,3}\])?(\.[a-z][a-z0-9_]*(\[[0-9]{1,3}\])?){0,8}$`)
	codeRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,47}$`)
)

func checkSanitized(t *testing.T, o Diagnostic, secret string) {
	t.Helper()
	if !codeRe.MatchString(o.Code) {
		t.Fatalf("code %q", o.Code)
	}
	for _, c := range o.ProviderCode {
		if !codeRe.MatchString(c) || longDigitRun(c, 5) {
			t.Fatalf("provider_code %q", c)
		}
	}
	for _, c := range o.Causes {
		if !reCause.MatchString(c) {
			t.Fatalf("cause %q", c)
		}
	}
	if len(o.Props) > maxProps || len(o.ProviderCode) > maxProviderCds || len(o.Causes) > maxCauses || len(o.Breadcrumbs) > MaxBreadcrumbs {
		t.Fatal("limits")
	}
	for _, p := range o.Props {
		if len(p) > 80 || !pathRe.MatchString(p) || longDigitRun(p, 5) {
			t.Fatalf("prop %q", p)
		}
	}
	if o.Shape != "" {
		parts := strings.Split(o.Shape, ",")
		if len(parts) > maxShapeKeys {
			t.Fatalf("shape %q", o.Shape)
		}
		for _, p := range parts {
			if !reShape.MatchString(p) {
				t.Fatalf("shape part %q", p)
			}
		}
	}
	if o.UpstreamHost != "" && !reHost.MatchString(o.UpstreamHost) {
		t.Fatalf("host %q", o.UpstreamHost)
	}
	blob, _ := json.Marshal(o)
	if secret != "" && strings.Contains(string(blob), secret) {
		t.Fatalf("secret leaked: %s", blob)
	}
	for _, b := range o.Breadcrumbs {
		if reEmail.MatchString(b) || len([]rune(b)) > MaxBreadcrumbLen {
			t.Fatalf("breadcrumb %q", b)
		}
		for _, m := range rePAN.FindAllString(b, -1) {
			if luhnValid(digitsOnly(m)) {
				t.Fatalf("PAN in breadcrumb %q", b)
			}
		}
		if reDigits.MatchString(b) || reJWT.MatchString(b) {
			t.Fatalf("digits/jwt in breadcrumb %q", b)
		}
	}
}

func FuzzSanitize(f *testing.F) {
	f.Add("charge", "invalid_request", "payer.email", "2067", "ok:string", "https://a.example.com/x", "hi ana@x.com 4111111111111111")
	f.Add("x y", "A", "a/b", "99999999", "k:bad", "user@host", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.sig")
	f.Add("", "", "", "", "", "", "tok=AbCdEf0123456789AbCdEf0123456789")
	f.Fuzz(func(t *testing.T, op, code, prop, cause, shape, host, crumb string) {
		sc := NewScrubber("fuzz-secret-1")
		d := Diagnostic{Org: uuid.New(), AddonKey: "acme", Operation: op, Code: code, ProviderCode: []string{code},
			Props: []string{prop, crumb}, Causes: []string{cause}, Shape: shape, UpstreamHost: host,
			Breadcrumbs: []string{crumb, crumb + " fuzz-secret-1"}}
		o := Sanitize(d, sc)
		checkSanitized(t, o, "fuzz-secret-1")
		again := Sanitize(o, sc)
		a, _ := json.Marshal(o)
		b, _ := json.Marshal(again)
		if string(a) != string(b) {
			t.Fatalf("not idempotent:\n%s\n%s", a, b)
		}
	})
}

func FuzzExtractBody(f *testing.F) {
	f.Add(`{"message":"bad payer.email ana@x.com","cause":[{"code":2067}],"error":"bad_request"}`, 400)
	f.Add(`<html>payer.email</html>`, 502)
	f.Add(`{"a":{"b":{"c":{"d":{"e":{"f":{"g":"h.ii"}}}}}}}`, 400)
	f.Fuzz(func(t *testing.T, body string, status int) {
		fc := ExtractBody([]byte(body), status)
		o := Sanitize(Diagnostic{Org: uuid.New(), AddonKey: "acme", ProviderCode: fc.ProviderCodes, Causes: fc.Causes, Props: fc.Props, Shape: fc.Shape}, nil)
		if len(o.Props) != len(fc.Props) && len(fc.Props) <= maxProps {
			// ExtractBody ya debe entregar solo lo que Sanitize acepta.
			t.Fatalf("extract produced rejectable props: %v -> %v", fc.Props, o.Props)
		}
		checkSanitized(t, o, "")
		for _, p := range fc.Props {
			if strings.Contains(p, "@") {
				t.Fatalf("prop with @: %q", p)
			}
		}
	})
}

func FuzzScrubText(f *testing.F) {
	f.Add("key=fuzz-secret-1 4111-1111-1111-1111 a@b.co")
	f.Add("Bearer abcdefgh https://u:p@h.example.com/x?y=1")
	f.Fuzz(func(t *testing.T, s string) {
		sc := NewScrubber("fuzz-secret-1")
		out := ScrubText(s, sc)
		if strings.Contains(out, "fuzz-secret-1") {
			t.Fatalf("secret survived: %q", out)
		}
		if again := ScrubText(out, sc); again != out {
			t.Fatalf("not idempotent: %q -> %q", out, again)
		}
	})
}
