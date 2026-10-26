package capability

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Adversarial review of fiscal.invoice.status|document (PR #556): a provider
// is untrusted code; nothing it answers may hand the customer's browser an
// absolute / provider URL, and the host (Executor) must enforce what the
// schema cannot say, not leave it to every consumer.

var advNow = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

func docURL(u string) string {
	b, _ := json.Marshal(map[string]any{
		"kind": "xml", "filename": "F-10.xml", "content_type": "application/xml", "size_bytes": 900,
		"url": u, "expires_at": "2026-10-26T12:10:00Z",
	})
	return string(b)
}

// rejected reports whether the output fails the schema or the contract check.
func docRejected(raw string) bool {
	c := Contracts[FiscalInvoiceDocument]
	if len(c.ValidateOutput([]byte(raw))) > 0 {
		return true
	}
	return len(CheckInvoiceDocument([]byte(raw), advNow)) > 0
}

func TestFiscalInvoiceDocumentURLCannotEscapeTheHost(t *testing.T) {
	for name, u := range map[string]string{
		"absolute":              "https://api.factura.com/f.pdf",
		"scheme":                "javascript:alert(1)",
		"protocol-relative":     "//pac.example/f.pdf",
		"backslash 2nd":         `/\pac.example`,
		"backslash later":       `/a\..\..\/pac.example`,
		"dot segments":          "/a/../..//pac.example/f.pdf",
		"double slash inside":   "/a//pac.example/f.pdf",
		"dot segment end":       "/a/..",
		"single dot":            "/./x",
		"encoded slashes":       "/%2F%2Fpac.example/f.pdf",
		"encoded slashes lower": "/%2f%2fpac.example",
		"encoded backslash":     "/%5C%5Cpac.example",
		"encoded dots":          "/%2e%2e/admin",
		"encoded control":       "/x%0d%0aSet-Cookie:a",
		"nul":                   "/x\x00y",
		"vertical tab":          "/x\x0by",
		"del":                   "/x\x7fy",
		"bidi override":         "/x‮y",
		"non ascii":             "/fáctura",
		"fragment":              "/x#y",
		"absolute in query":     "/x?next=https://pac.example",
		"too long":              "/" + strings.Repeat("a", 4096),
		"space":                 "/x y",
	} {
		if !docRejected(docURL(u)) {
			t.Errorf("%s: url %q accepted", name, u)
		}
	}
	for name, u := range map[string]string{
		"store path":        "/storefront/customer/orders/SO-1/invoices/d1/xml?exp=1793000000&sig=ab12",
		"escaped id":        "/addons/fiscal_mexico/download/F%2D10/xml?exp=1793000000&sig=ab12",
		"base64url sig":     "/f/d1/xml?sig=AbC-_d~e.f",
		"at sign in a path": "/f/@d1",
	} {
		if docRejected(docURL(u)) {
			t.Errorf("%s: legit url %q rejected: %v %v", name, u,
				Contracts[FiscalInvoiceDocument].ValidateOutput([]byte(docURL(u))), CheckInvoiceDocument([]byte(docURL(u)), advNow))
		}
	}
}

func TestFiscalInvoiceDocumentFilenameAndExtensions(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString([]byte("%PDF-1"))
	inline := func(extra string) string {
		return `{"kind":"pdf","filename":"F-10.pdf","content_type":"application/pdf","size_bytes":6,"content_base64":"` + b64 + `"` + extra + `}`
	}
	if docRejected(inline("")) {
		t.Fatal("baseline rejected")
	}
	for name, raw := range map[string]string{
		"path traversal name": strings.Replace(inline(""), "F-10.pdf", "../../etc/passwd", 1),
		"slash in name":       strings.Replace(inline(""), "F-10.pdf", "a/b.pdf", 1),
		"backslash in name":   strings.Replace(inline(""), "F-10.pdf", `a\\b.pdf`, 1),
		"header injection":    strings.Replace(inline(""), "F-10.pdf", `a\r\nSet-Cookie: x=1`, 1),
		"quote in name":       strings.Replace(inline(""), "F-10.pdf", `a\".pdf`, 1),
		"empty name":          strings.Replace(inline(""), "F-10.pdf", "", 1),
		"dotdot name":         strings.Replace(inline(""), "F-10.pdf", "..", 1),
		"long name":           strings.Replace(inline(""), "F-10.pdf", strings.Repeat("a", 300), 1),
		"x_ absolute url":     inline(`,"x_pac_url":"https://api.factura.com/f.pdf"`),
		"x_ nested url":       inline(`,"x_pac":{"links":["//pac.example/a"]}`),
		"duplicate url key":   `{"kind":"xml","filename":"a.xml","content_type":"application/xml","size_bytes":1,"url":"https://pac.example/a","url":"/ok","expires_at":"2026-10-26T12:05:00Z"}`,
		"escaped dup key":     `{"kind":"xml","filename":"a.xml","content_type":"application/xml","size_bytes":1,"url":"https://pac.example/a","\u0075rl":"/ok","expires_at":"2026-10-26T12:05:00Z"}`,
		"invalid utf8":        inline(`,"x_note":"` + "\xff" + `"`),
	} {
		if !docRejected(raw) {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestFiscalInvoiceStatusCannotCarryLinksOrIncoherentFlags(t *testing.T) {
	c := Contracts[FiscalInvoiceStatus]
	rejected := func(raw string) bool {
		return len(c.ValidateOutput([]byte(raw))) > 0 || len(CheckInvoiceStatus([]byte(raw), advNow)) > 0
	}
	good := `{"documents":[{"document_id":"d1","status":"issued","kind":"invoice","available_files":["pdf","xml"],"test_mode":true,"environment":"sandbox","x_pac":"factura_com"},
	{"document_id":"d2","status":"cancelled","kind":"credit_note","related_to":"d1","cancelled_at":"2026-10-21T10:00:00Z"}]}`
	if rejected(good) {
		t.Fatalf("valid status rejected: %v %v", c.ValidateOutput([]byte(good)), CheckInvoiceStatus([]byte(good), advNow))
	}
	for name, raw := range map[string]string{
		"x_ link":             `{"documents":[{"document_id":"d","status":"issued","kind":"invoice","x_pdf":"https://pac.example/a.pdf"}]}`,
		"x_ protocol rel":     `{"documents":[{"document_id":"d","status":"issued","kind":"invoice","x_pdf":"//pac.example/a.pdf"}],"x_meta":{"u":"//pac"}}`,
		"url in message":      `{"documents":[{"document_id":"d","status":"error","kind":"invoice","error_reason":"unknown","error_message":"see https://api.factura.com/v4/x?token=abc"}]}`,
		"unknown file kind":   `{"documents":[{"document_id":"d","status":"issued","kind":"invoice","available_files":["pdf","zip"]}]}`,
		"test flag mismatch":  `{"documents":[{"document_id":"d","status":"issued","kind":"invoice","test_mode":false,"environment":"sandbox"}]}`,
		"duplicate id":        `{"documents":[{"document_id":"d","status":"issued","kind":"invoice"},{"document_id":"d","status":"pending","kind":"invoice"}]}`,
		"invoice related_to":  `{"documents":[{"document_id":"d","status":"issued","kind":"invoice","related_to":"x"}]}`,
		"reason without err":  `{"documents":[{"document_id":"d","status":"issued","kind":"invoice","error_reason":"unknown"}]}`,
		"duplicate key":       `{"documents":[{"document_id":"d","status":"issued","kind":"invoice","test_mode":true,"test_mode":false,"environment":"production"}]}`,
		"bad cancelled_at":    `{"documents":[{"document_id":"d","status":"cancelled","kind":"invoice","cancelled_at":"ayer"}]}`,
		"bad emitted_at":      `{"documents":[{"document_id":"d","status":"issued","kind":"invoice","emitted_at":"2026-13-01"}]}`,
		"files while pending": `{"documents":[{"document_id":"d","status":"pending","kind":"invoice","available_files":["pdf"]}]}`,
	} {
		if !rejected(raw) {
			t.Errorf("%s accepted", name)
		}
	}
}

// The host applies the contract check: a consumer never sees an output that
// CheckInvoiceDocument / CheckInvoiceStatus reject, nor an oversized one.
func TestFiscalInvoiceReadHostEnforcesContractCheck(t *testing.T) {
	mx := Market{Code: "mx", Countries: []string{"MX"}}
	var answer json.RawMessage
	g := &Engine{
		Source:   fiscalSource(Provider{Addon: "fiscal_mexico", Countries: []string{"MX"}}),
		Invoke:   func(context.Context, Call) (json.RawMessage, error) { return answer, nil },
		Executor: Executor{Now: func() time.Time { return advNow }},
	}
	q := func(key string, in map[string]any) Result {
		t.Helper()
		res, err := g.Query(context.Background(), Query{Org: org, Market: mx, Capability: key, Input: in})
		if err != nil {
			t.Fatal(err)
		}
		return res.Results[0]
	}
	docIn := map[string]any{"order_ref": "SO-1", "document_id": "d1", "kind": "pdf"}
	b64 := base64.StdEncoding.EncodeToString([]byte("%PDF-1"))
	for name, raw := range map[string]string{
		"both":        `{"kind":"pdf","filename":"a.pdf","content_type":"application/pdf","size_bytes":6,"content_base64":"` + b64 + `","url":"/x","expires_at":"2026-10-26T12:05:00Z"}`,
		"size lies":   `{"kind":"pdf","filename":"a.pdf","content_type":"application/pdf","size_bytes":1,"content_base64":"` + b64 + `"}`,
		"expired url": `{"kind":"pdf","filename":"a.pdf","content_type":"application/pdf","size_bytes":6,"url":"/x","expires_at":"2026-10-26T11:00:00Z"}`,
		"x_ leak":     `{"kind":"pdf","filename":"a.pdf","content_type":"application/pdf","size_bytes":6,"content_base64":"` + b64 + `","x_src":"https://pac.example/a"}`,
	} {
		answer = json.RawMessage(raw)
		if r := q(FiscalInvoiceDocument, docIn); r.ErrorCode() != "invalid_output" {
			t.Errorf("%s: host let it through (code %q)", name, r.ErrorCode())
		}
	}
	answer = json.RawMessage(`{"kind":"pdf","filename":"a.pdf","content_type":"application/pdf","size_bytes":6,"content_base64":"` + b64 + `"}`)
	if r := q(FiscalInvoiceDocument, docIn); !r.OK() {
		t.Fatalf("valid document rejected: %v", r.Err)
	}
	answer = json.RawMessage(`{"documents":[{"document_id":"d","status":"issued","kind":"invoice","x_pdf":"https://pac.example/a.pdf"}]}`)
	if r := q(FiscalInvoiceStatus, map[string]any{"order_ref": "SO-1"}); r.ErrorCode() != "invalid_output" {
		t.Errorf("status x_ link: host let it through (code %q)", r.ErrorCode())
	}
	// Oversized output: refused before it is decoded, whatever it holds.
	huge := `{"kind":"pdf","filename":"a.pdf","content_type":"application/pdf","size_bytes":6,"content_base64":"` + b64 + `","x_pad":"` + strings.Repeat("A", 12<<20) + `"}`
	answer = json.RawMessage(huge)
	r := q(FiscalInvoiceDocument, docIn)
	if r.ErrorCode() != "invalid_output" || len(r.Err.Error()) > 4096 {
		t.Errorf("oversized output: code %q, message %d bytes", r.ErrorCode(), len(r.Err.Error()))
	}
	answer = json.RawMessage(`{"documents":[],"x_pad":"` + strings.Repeat("A", 2<<20) + `"}`)
	if r := q(FiscalInvoiceStatus, map[string]any{"order_ref": "SO-1"}); r.ErrorCode() != "invalid_output" {
		t.Errorf("oversized status: code %q", r.ErrorCode())
	}
	// The biggest legitimate file still fits under the cap.
	max := base64.StdEncoding.EncodeToString(make([]byte, InvoiceDocumentMaxBytes))
	answer = json.RawMessage(`{"kind":"pdf","filename":"a.pdf","content_type":"application/pdf","size_bytes":5242880,"content_base64":"` + max + `"}`)
	if r := q(FiscalInvoiceDocument, docIn); !r.OK() {
		t.Fatalf("5 MiB document rejected: %v", r.Err)
	}
}

// Without PinRequired a tenant may choose among ITS OWN installed providers,
// never reach an addon its org does not have installed and enabled.
func TestFiscalInvoiceReadCannotPinForeignProvider(t *testing.T) {
	other := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	var hit []string
	g := &Engine{
		Source: ProviderSourceFunc(func(_ context.Context, o uuid.UUID, _ string) ([]Provider, error) {
			if o == other {
				return []Provider{{Addon: "pac_of_other_org", Countries: []string{"MX"}}}, nil
			}
			return []Provider{{Addon: "fiscal_mexico", Countries: []string{"MX"}, Priority: 1}, {Addon: "pac_b", Countries: []string{"MX"}, Priority: 2}}, nil
		}),
		Invoke: func(_ context.Context, call Call) (json.RawMessage, error) {
			hit = append(hit, call.Provider.Addon+"@"+call.Org.String())
			return json.RawMessage(`{"documents":[]}`), nil
		},
	}
	mx := Market{Code: "mx", Countries: []string{"MX"}}
	in := map[string]any{"order_ref": "SO-1"}
	if _, err := g.Query(context.Background(), Query{Org: org, Market: mx, Capability: FiscalInvoiceStatus, Input: in, Provider: "pac_of_other_org"}); !errors.Is(err, ErrNoProvider) {
		t.Fatalf("foreign provider pin: %v", err)
	}
	if _, err := g.Query(context.Background(), Query{Org: org, Market: mx, Capability: FiscalInvoiceStatus, Input: in, Provider: "pac_b"}); err != nil {
		t.Fatalf("own provider pin: %v", err)
	}
	if _, err := g.Query(context.Background(), Query{Org: org, Market: mx, Capability: FiscalInvoiceStatus, Input: in}); err != nil {
		t.Fatal(err)
	}
	want := "pac_b@" + org.String() + ",fiscal_mexico@" + org.String()
	if strings.Join(hit, ",") != want {
		t.Fatalf("hits = %v, want %s", hit, want)
	}
	// An org-ish input field never changes the org the provider runs under.
	hit = nil
	in = map[string]any{"order_ref": "SO-1", "org_id": other.String()}
	if _, err := g.Query(context.Background(), Query{Org: org, Market: mx, Capability: FiscalInvoiceStatus, Input: in}); err != nil {
		t.Fatal(err)
	}
	if len(hit) != 1 || !strings.HasSuffix(hit[0], org.String()) {
		t.Fatalf("org taken from input: %v", hit)
	}
}
