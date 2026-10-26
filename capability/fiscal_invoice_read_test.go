package capability

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"strings"
	"testing"
	"time"
)

var updateGolden = flag.Bool("update", false, "rewrite capability/testdata golden files")

// goldenSchema is a stable, readable dump of a contract's wire shape.
func goldenSchema(c Contract) map[string]any {
	var dump func(s *Schema) any
	dump = func(s *Schema) any {
		if s == nil {
			return nil
		}
		var fs []map[string]any
		for _, f := range s.Fields {
			m := map[string]any{"name": f.Name, "type": string(f.Type)}
			if f.Required {
				m["required"] = true
			}
			if len(f.Enum) > 0 {
				m["enum"] = f.Enum
			}
			if f.Elem != "" {
				m["elem"] = string(f.Elem)
			}
			if f.Pattern != "" {
				m["pattern"] = f.Pattern
			}
			if f.Of != nil {
				m["of"] = dump(f.Of)
			}
			fs = append(fs, m)
		}
		return map[string]any{"open": s.Open, "fields": fs}
	}
	return map[string]any{
		"key": c.Key, "kind": string(c.Kind), "cardinality": string(c.Cardinality),
		"timeout_ms": c.Timeout.Milliseconds(), "idempotent": c.Idempotent, "pin_required": c.PinRequired,
		"errors": c.Errors, "input": dump(c.Input), "output": dump(c.Output),
		"max_output_bytes": c.MaxOutputBytes, "output_check": c.CheckOutput != nil,
	}
}

func TestFiscalInvoiceReadSchemaGolden(t *testing.T) {
	got, err := json.MarshalIndent([]any{
		goldenSchema(mustContract(t, FiscalInvoiceStatus)),
		goldenSchema(mustContract(t, FiscalInvoiceDocument)),
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	const path = "testdata/fiscal_invoice_read.golden.json"
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./capability -run Golden -update)", err)
	}
	if string(want) != string(got) {
		t.Fatalf("the wire contract of fiscal.invoice.status|document changed; it is additive-only. Review and rerun with -update.\n%s", got)
	}
}

func TestFiscalInvoiceReadContractShape(t *testing.T) {
	for _, key := range []string{FiscalInvoiceStatus, FiscalInvoiceDocument} {
		c := mustContract(t, key)
		if !c.IsQuery() || c.Fanout() || c.Idempotent || c.PinRequired {
			t.Errorf("%s: want a plain One query, got %+v", key, c)
		}
		if strings.Join(c.Errors, ",") != "not_found,not_ready,provider_unavailable,not_configured" {
			t.Errorf("%s errors = %v", key, c.Errors)
		}
		// The org is the invocation's, never an input field.
		for _, f := range c.Input.Fields {
			if f.Name == "org" || f.Name == "org_id" {
				t.Errorf("%s: org must be implicit", key)
			}
		}
		// No provider URL may travel in the normalized status.
		if key == FiscalInvoiceStatus {
			walkFields(c.Output, func(f Field) {
				if strings.HasSuffix(f.Name, "url") || strings.HasSuffix(f.Name, "link") {
					t.Errorf("status output must not carry %s", f.Name)
				}
			})
		}
	}
}

func walkFields(s *Schema, fn func(Field)) {
	if s == nil {
		return
	}
	for _, f := range s.Fields {
		fn(f)
		walkFields(f.Of, fn)
	}
}

func TestFiscalInvoiceStatusValidation(t *testing.T) {
	c := mustContract(t, FiscalInvoiceStatus)
	if errs := c.CheckInput(map[string]any{}); len(errs) == 0 {
		t.Fatal("order_ref is required")
	}
	if errs := c.CheckInput(map[string]any{"order_ref": "SO-1"}); len(errs) != 0 {
		t.Fatalf("minimal input rejected: %v", errs)
	}
	good := `{"documents":[
	 {"document_id":"d1","status":"issued","kind":"invoice","fiscal_uuid":"U-1","series":"A","folio":"10","total":"1160.00","currency":"MXN","emitted_at":"2026-10-20T10:00:00Z","available_files":["pdf","xml"],"test_mode":true,"environment":"sandbox"},
	 {"document_id":"d2","status":"issued","kind":"credit_note","related_to":"d1","total":"-100.00","currency":"MXN"},
	 {"document_id":"d3","status":"error","kind":"invoice","error_reason":"rejected_by_authority","error_message":"RFC no encontrado","x_pac":"factura_com"}]}`
	if p := c.ValidateOutput([]byte(good)); len(p) != 0 {
		t.Fatalf("valid output rejected: %v", p)
	}
	if p := c.ValidateOutput([]byte(`{"documents":[]}`)); len(p) != 0 {
		t.Fatalf("empty list is a valid answer: %v", p)
	}
	for name, bad := range map[string]string{
		"no documents":   `{}`,
		"bad status":     `{"documents":[{"document_id":"d","status":"failed","kind":"invoice"}]}`,
		"bad kind":       `{"documents":[{"document_id":"d","status":"issued","kind":"receipt"}]}`,
		"no document_id": `{"documents":[{"status":"issued","kind":"invoice"}]}`,
		"float total":    `{"documents":[{"document_id":"d","status":"issued","kind":"invoice","total":1160}]}`,
		"bad env":        `{"documents":[{"document_id":"d","status":"issued","kind":"invoice","environment":"staging"}]}`,
		"bad reason":     `{"documents":[{"document_id":"d","status":"error","kind":"invoice","error_reason":"HTTP 500"}]}`,
		"pac url":        `{"documents":[{"document_id":"d","status":"issued","kind":"invoice","pdf_url":"https://pac.example/x.pdf"}]}`,
		"bad test_mode":  `{"documents":[{"document_id":"d","status":"issued","kind":"invoice","test_mode":"yes"}]}`,
	} {
		if p := c.ValidateOutput([]byte(bad)); len(p) == 0 {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestFiscalInvoiceDocumentValidation(t *testing.T) {
	c := mustContract(t, FiscalInvoiceDocument)
	for name, in := range map[string]map[string]any{
		"no order":    {"document_id": "d", "kind": "pdf"},
		"no document": {"order_ref": "SO-1", "kind": "pdf"},
		"no kind":     {"order_ref": "SO-1", "document_id": "d"},
		"bad kind":    {"order_ref": "SO-1", "document_id": "d", "kind": "zip"},
	} {
		if errs := c.CheckInput(in); len(errs) == 0 {
			t.Errorf("%s accepted", name)
		}
	}
	if errs := c.CheckInput(map[string]any{"order_ref": "SO-1", "document_id": "d", "kind": "xml"}); len(errs) != 0 {
		t.Fatalf("valid input rejected: %v", errs)
	}

	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	body := []byte("%PDF-1.7 hello")
	b64 := base64.StdEncoding.EncodeToString(body)
	inline := `{"kind":"pdf","filename":"F-10.pdf","content_type":"application/pdf","size_bytes":14,"content_base64":"` + b64 + `"}`
	signed := func(url, exp string) string {
		return `{"kind":"xml","filename":"F-10.xml","content_type":"application/xml","size_bytes":900,"url":"` + url + `","expires_at":"` + exp + `"}`
	}
	ok := map[string]string{
		"inline bytes":  inline,
		"relative url":  signed("/storefront/invoices/d1/file?sig=abc", "2026-10-26T12:10:00Z"),
		"test mode out": strings.Replace(inline, `"size_bytes"`, `"test_mode":true,"size_bytes"`, 1),
	}
	for name, raw := range ok {
		if p := c.ValidateOutput([]byte(raw)); len(p) != 0 {
			t.Errorf("%s: schema: %v", name, p)
		}
		if p := CheckInvoiceDocument([]byte(raw), now); len(p) != 0 {
			t.Errorf("%s: limits: %v", name, p)
		}
	}
	// Schema-level rejections.
	for name, raw := range map[string]string{
		"absolute pac url":  signed("https://api.factura.com/f.pdf", "2026-10-26T12:10:00Z"),
		"protocol-relative": signed("//pac.example/f.pdf", "2026-10-26T12:10:00Z"),
		"backslash":         signed(`/\\pac`, "2026-10-26T12:10:00Z"),
		"bad content_type":  strings.Replace(inline, "application/pdf", "text/html", 1),
		"no size":           `{"kind":"pdf","filename":"a","content_type":"application/pdf","content_base64":"` + b64 + `"}`,
	} {
		if p := c.ValidateOutput([]byte(raw)); len(p) == 0 {
			t.Errorf("%s passed the schema", name)
		}
	}
	// Limits the schema cannot express.
	big := base64.StdEncoding.EncodeToString(make([]byte, InvoiceDocumentMaxBytes+1))
	for name, raw := range map[string]string{
		"neither":         `{"kind":"pdf","filename":"a","content_type":"application/pdf","size_bytes":1}`,
		"both":            strings.Replace(inline, `"content_base64"`, `"url":"/x","expires_at":"2026-10-26T12:05:00Z","content_base64"`, 1),
		"kind mismatch":   strings.Replace(inline, `"kind":"pdf"`, `"kind":"xml"`, 1),
		"size lies":       strings.Replace(inline, `"size_bytes":14`, `"size_bytes":15`, 1),
		"too large":       `{"kind":"pdf","filename":"a","content_type":"application/pdf","size_bytes":5242881,"content_base64":"` + big + `"}`,
		"size over cap":   `{"kind":"pdf","filename":"a","content_type":"application/pdf","size_bytes":9999999,"url":"/x","expires_at":"2026-10-26T12:05:00Z"}`,
		"url no expiry":   `{"kind":"pdf","filename":"a","content_type":"application/pdf","size_bytes":1,"url":"/x"}`,
		"expired":         signed("/x", "2026-10-26T11:59:00Z"),
		"expiry too long": signed("/x", "2026-10-26T12:16:00Z"),
		"bad base64":      strings.Replace(inline, b64, "!!!", 1),
	} {
		if p := CheckInvoiceDocument([]byte(raw), now); len(p) == 0 {
			t.Errorf("%s accepted by the limits check", name)
		}
	}
}

func fiscalSource(ps ...Provider) ProviderSource { return pinSource(ps...) }

func TestFiscalInvoiceStatusRoutingByCountryAndPriority(t *testing.T) {
	c := mustContract(t, FiscalInvoiceStatus)
	mx := Market{Code: "mx", Countries: []string{"MX"}}
	us := Market{Code: "us", Countries: []string{"US"}}
	provs := []Provider{
		{Addon: "fiscal_mexico", Countries: []string{"MX"}, Priority: 2},
		{Addon: "pac_b", Countries: []string{"MX"}, Priority: 1},
	}
	got, err := Select(c, provs, mx)
	if err != nil || strings.Join(addons(got), ",") != "pac_b" {
		t.Fatalf("priority: %v %v", addons(got), err)
	}
	// Same priority = ambiguous (fail closed), the org must choose.
	provs[0].Priority = 1
	if _, err := Select(c, provs, mx); !errors.Is(err, ErrAmbiguousProvider) {
		t.Fatalf("tie: %v", err)
	}
	// US: nobody serves it = "invoicing not available".
	if _, err := Select(c, provs, us); !errors.Is(err, ErrNoProvider) {
		t.Fatalf("us: %v", err)
	}
	// The preferred provider has no credentials: the backup serves.
	provs[0].Priority, provs[1].Priority = 1, 2
	provs[0].Status = StatusNotConfigured
	got, err = Select(c, provs, mx)
	if err != nil || strings.Join(addons(got), ",") != "pac_b" {
		t.Fatalf("not_configured fallback: %v %v", addons(got), err)
	}
	// Every candidate unconfigured: not_configured, not no_provider.
	provs[1].Status = StatusNotConfigured
	if _, err := Select(c, provs, mx); !errors.Is(err, ErrNotConfigured) || errors.Is(err, ErrNoProvider) {
		t.Fatalf("all not_configured: %v", err)
	}
}

func TestFiscalInvoiceReadEngineEndToEnd(t *testing.T) {
	var gotOrg, gotCaller string
	g := &Engine{
		Source: fiscalSource(Provider{Addon: "fiscal_mexico", Countries: []string{"MX"}}),
		Invoke: func(_ context.Context, call Call) (json.RawMessage, error) {
			gotOrg = call.Org.String()
			var in map[string]any
			_ = json.Unmarshal(call.Payload, &in)
			switch in["order_ref"] {
			case "SO-OTHER":
				return nil, NewCodedError("not_found", "document not found")
			case "SO-WAIT":
				return nil, NewCodedError("not_ready", "pac still stamping")
			case "SO-DOWN":
				return nil, NewCodedError("provider_unavailable", "")
			case "SO-LEAK":
				return json.RawMessage(`{"documents":[{"document_id":"d","status":"issued","kind":"invoice","pdf_url":"https://pac/x"}]}`), nil
			}
			return json.RawMessage(`{"documents":[{"document_id":"d1","status":"pending","kind":"invoice"}]}`), nil
		},
	}
	_ = gotCaller
	mx := Market{Code: "mx", Countries: []string{"MX"}}
	run := func(ref string) QueryResult {
		t.Helper()
		res, err := g.Query(context.Background(), Query{Org: org, Market: mx, Capability: FiscalInvoiceStatus, Input: map[string]any{"order_ref": ref}})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := run("SO-1"); len(res.Results) != 1 || !res.Results[0].OK() || gotOrg != org.String() {
		t.Fatalf("ok path: %+v org=%s", res, gotOrg)
	}
	for ref, code := range map[string]string{"SO-OTHER": "not_found", "SO-WAIT": "not_ready", "SO-DOWN": "provider_unavailable", "SO-LEAK": "invalid_output"} {
		res := run(ref)
		if len(res.Results) != 1 || res.Results[0].ErrorCode() != code {
			t.Errorf("%s: want %s, got %+v", ref, code, res.Results)
		}
	}
	// No credentials anywhere.
	g.Source = fiscalSource(Provider{Addon: "fiscal_mexico", Countries: []string{"MX"}, Status: StatusNotConfigured})
	if _, err := g.Query(context.Background(), Query{Org: org, Market: mx, Capability: FiscalInvoiceStatus, Input: map[string]any{"order_ref": "SO-1"}}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("not_configured: %v", err)
	}
	// Invalid input never reaches the provider.
	if _, err := g.Query(context.Background(), Query{Org: org, Market: mx, Capability: FiscalInvoiceDocument, Input: map[string]any{"order_ref": "SO-1", "document_id": "d"}}); err == nil {
		t.Fatal("missing kind must be rejected")
	}
}
