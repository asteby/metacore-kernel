package wasm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/asteby/metacore-kernel/diag"
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/security"
)

func sleb64(v int64) []byte {
	var out []byte
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if (v == 0 && b&0x40 == 0) || (v == -1 && b&0x40 != 0) {
			return append(out, b)
		}
		out = append(out, b|0x80)
	}
}

// diagGuest builds a guest importing log and http_request with exports:
//
//	pay      log, POST url (with an Authorization header), return a failure envelope
//	okafter  log, POST url, return a success envelope (the guest "handled" the error)
//	boom     log, unreachable
//	spin     infinite loop (budget timeout)
func diagGuest(url, logLine, headers, envFail, envOK string) []byte {
	off := 16
	place := map[string][2]int{}
	var data []byte
	add := func(name, s string) {
		place[name] = [2]int{off, len(s)}
		data = append(data, []byte(s)...)
		off += len(s)
	}
	// data starts at memory offset 16; keep track relative to it.
	add("log", logLine)
	add("url", url)
	add("method", "POST")
	add("hdr", headers)
	add("fail", envFail)
	add("ok", envOK)

	var buf []byte
	buf = append(buf, 0x00, 0x61, 0x73, 0x6D, 0x01, 0x00, 0x00, 0x00)
	buf = append(buf, section(0x01, []byte{
		0x04,
		0x60, 0x01, 0x7F, 0x01, 0x7F,
		0x60, 0x02, 0x7F, 0x7F, 0x01, 0x7E,
		0x60, 0x02, 0x7F, 0x7F, 0x00,
		0x60, 0x08, 0x7F, 0x7F, 0x7F, 0x7F, 0x7F, 0x7F, 0x7F, 0x7F, 0x01, 0x7E,
	})...)
	var imports []byte
	imports = append(imports, 0x02)
	imports = append(imports, encodeName("metacore_host")...)
	imports = append(imports, encodeName("log")...)
	imports = append(imports, 0x00, 0x02)
	imports = append(imports, encodeName("metacore_host")...)
	imports = append(imports, encodeName("http_request")...)
	imports = append(imports, 0x00, 0x03)
	buf = append(buf, section(0x02, imports)...)
	buf = append(buf, section(0x03, []byte{0x05, 0x00, 0x01, 0x01, 0x01, 0x01})...)
	buf = append(buf, section(0x05, []byte{0x01, 0x00, 0x01})...)
	globals := []byte{0x01, 0x7F, 0x01, 0x41}
	globals = append(globals, encodeSLEB128(8192)...)
	globals = append(globals, 0x0B)
	buf = append(buf, section(0x06, globals)...)
	var exports []byte
	exports = append(exports, 0x06)
	exports = append(exports, encodeName("memory")...)
	exports = append(exports, 0x02, 0x00)
	for i, n := range []string{"alloc", "pay", "okafter", "boom", "spin"} {
		exports = append(exports, encodeName(n)...)
		exports = append(exports, 0x00, byte(2+i))
	}
	buf = append(buf, section(0x07, exports)...)

	i32 := func(v int) []byte { return append([]byte{0x41}, encodeSLEB128(int32(v))...) }
	logCall := func() []byte {
		var b []byte
		b = append(b, i32(place["log"][0])...)
		b = append(b, i32(place["log"][1])...)
		return append(b, 0x10, 0x00)
	}
	httpCall := func() []byte {
		var b []byte
		for _, n := range []string{"url", "method", "hdr"} {
			b = append(b, i32(place[n][0])...)
			b = append(b, i32(place[n][1])...)
		}
		b = append(b, i32(0)...)
		b = append(b, i32(0)...)
		return append(b, 0x10, 0x01, 0x1a) // call http_request; drop
	}
	ret := func(n string) []byte {
		v := int64(place[n][0])<<32 | int64(place[n][1])
		return append(append([]byte{0x42}, sleb64(v)...), 0x0B)
	}
	alloc := withSize([]byte{0x01, 0x01, 0x7F, 0x23, 0x00, 0x22, 0x01, 0x20, 0x00, 0x6A, 0x24, 0x00, 0x20, 0x01, 0x0B})
	body := func(parts ...[]byte) []byte {
		b := []byte{0x00}
		for _, p := range parts {
			b = append(b, p...)
		}
		return withSize(b)
	}
	code := []byte{0x05}
	code = append(code, alloc...)
	code = append(code, body(logCall(), httpCall(), ret("fail"))...)
	code = append(code, body(logCall(), httpCall(), ret("ok"))...)
	code = append(code, body(logCall(), []byte{0x00, 0x0B})...)
	code = append(code, body([]byte{0x03, 0x40, 0x0c, 0x00, 0x0b, 0x00, 0x0B})...)
	buf = append(buf, section(0x0A, code)...)

	seg := []byte{0x01, 0x00, 0x41}
	seg = append(seg, encodeSLEB128(16)...)
	seg = append(seg, 0x0B)
	seg = append(seg, encodeULEB128(uint32(len(data)))...)
	seg = append(seg, data...)
	buf = append(buf, section(0x0B, seg)...)
	return buf
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func stubHTTP(t *testing.T, fn rtFunc) {
	t.Helper()
	prev := newHTTPClient
	newHTTPClient = func() *http.Client { return &http.Client{Transport: fn, Timeout: 5 * time.Second} }
	t.Cleanup(func() { newHTTPClient = prev })
}

func resp(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}
}

type diagFixture struct {
	h   *Host
	rec *diag.Recorder
	st  *diag.Store
	org uuid.UUID
}

const (
	diagSecret = "sk_live_TOPSECRET9"
	diagBearer = "tok_live_ABCDEF123456"
)

func newDiagFixture(t *testing.T, timeoutMs int) *diagFixture {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:wasmdiag_%s?mode=memory&cache=shared&_busy_timeout=5000", uuid.NewString())),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sql, _ := db.DB()
	sql.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sql.Close() })
	if err := diag.Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	st := diag.NewStore(db)
	on := true
	rec := diag.NewRecorder(st, diag.Options{Enabled: &on, FlushEvery: time.Hour, AddonVersion: func(string) string { return "1.4.2" }})
	ctx := context.Background()
	caps := security.Compile("diagaddon", []manifest.Capability{{Kind: "http:fetch", Target: "api.provider.example"}})
	h, err := NewHost(ctx, caps, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close(ctx) })
	h.WithDiagnostics(rec)
	wasmBytes := diagGuest("https://api.provider.example/v1/payments?token=abc", "charging key="+diagSecret+" email=ana@example.com",
		`{"Authorization":"Bearer `+diagBearer+`"}`,
		`{"success":false,"error":{"code":"provider_error","message":"payer.email is invalid"}}`,
		`{"success":true,"data":{}}`)
	spec := &manifest.BackendSpec{Runtime: "wasm", Entry: "backend.wasm", Exports: []string{"pay", "okafter", "boom", "spin"}, TimeoutMs: timeoutMs}
	if err := h.Load(ctx, "diagaddon", wasmBytes, spec); err != nil {
		t.Fatal(err)
	}
	return &diagFixture{h: h, rec: rec, st: st, org: uuid.New()}
}

func (f *diagFixture) invoke(t *testing.T, fn string) ([]byte, error) {
	t.Helper()
	ctx := diag.WithCorrelation(context.Background(), "corr-123")
	ctx = diag.WithSubject(ctx, "payment", "pay_42")
	return f.h.InvokeFor(ctx, f.org, uuid.New(), "diagaddon", fn, []byte(`{}`), map[string]string{"api_key": diagSecret})
}

func (f *diagFixture) groups(t *testing.T) []diag.Group {
	t.Helper()
	f.rec.Flush(context.Background())
	gs, _, err := f.st.List(context.Background(), diag.Filter{Org: f.org})
	if err != nil {
		t.Fatal(err)
	}
	return gs
}

// The real case: the provider answers 400 and the guest returns a failure
// envelope. Nobody changed the addon; the diagnostic must show the 400, the
// field, the host and the breadcrumbs, with every secret scrubbed.
func TestDiag_ProviderRejectionVisibleWithoutChangingTheAddon(t *testing.T) {
	f := newDiagFixture(t, 5000)
	stubHTTP(t, func(r *http.Request) (*http.Response, error) {
		return resp(400, `{"message":"invalid parameter payer.identification.number for sk_live_TOPSECRET9 ana@example.com","error":"bad_request","status":400,"cause":[{"code":2067,"description":"Invalid user identification number 4111111111111111"}]}`), nil
	})
	out, err := f.invoke(t, "pay")
	if err != nil || !bytes.Contains(out, []byte(`"success":false`)) {
		t.Fatalf("out=%s err=%v", out, err)
	}
	gs := f.groups(t)
	if len(gs) != 1 {
		t.Fatalf("groups=%d", len(gs))
	}
	g := gs[0]
	if g.Kind != "http" || g.HTTPStatus != 400 || g.Code != "provider_error" || g.AddonKey != "diagaddon" || g.AddonVersion != "1.4.2" {
		t.Fatalf("group=%+v", g)
	}
	_, evs, err := f.st.Get(context.Background(), f.org, g.ID)
	if err != nil || len(evs) != 1 {
		t.Fatalf("events=%d err=%v", len(evs), err)
	}
	d := evs[0].Diagnostic
	if d.UpstreamHost != "api.provider.example" || d.Operation != "pay" || d.CorrelationID != "corr-123" ||
		d.Subject != (diag.Subject{Type: "payment", ID: "pay_42"}) {
		t.Fatalf("diag=%+v", d)
	}
	if !contains(d.ProviderCode, "bad_request") || !contains(d.Causes, "2067") || !contains(d.Props, "payer.identification.number") {
		t.Fatalf("facts missing: %+v", d)
	}
	if len(d.Breadcrumbs) != 1 {
		t.Fatalf("breadcrumbs=%v", d.Breadcrumbs)
	}
	if d.DiagRef == "" || d.DiagRef != evs[0].DiagRef {
		t.Fatalf("diag_ref=%q", d.DiagRef)
	}
	blob := fmt.Sprintf("%+v", evs)
	for _, leak := range []string{diagSecret, diagBearer, "ana@example.com", "4111111111111111", "token=abc", "/v1/payments"} {
		if strings.Contains(blob, leak) {
			t.Fatalf("leaked %q in %s", leak, blob)
		}
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// A 400 the guest handled (success envelope) leaves nothing: breadcrumbs and
// HTTP failures persist only if the invocation ends in failure.
func TestDiag_HandledHTTPErrorLeavesNoRecord(t *testing.T) {
	f := newDiagFixture(t, 5000)
	stubHTTP(t, func(*http.Request) (*http.Response, error) { return resp(404, `{"error":"not_found"}`), nil })
	if _, err := f.invoke(t, "okafter"); err != nil {
		t.Fatal(err)
	}
	if gs := f.groups(t); len(gs) != 0 {
		t.Fatalf("groups=%v", gs)
	}
}

func TestDiag_TransportErrorIsRetryableAndIndeterminateOnRuntimeFailure(t *testing.T) {
	f := newDiagFixture(t, 5000)
	stubHTTP(t, func(*http.Request) (*http.Response, error) { return nil, errors.New("dial tcp: connection refused") })
	if _, err := f.invoke(t, "pay"); err != nil {
		t.Fatal(err)
	}
	gs := f.groups(t)
	if len(gs) != 1 {
		t.Fatalf("groups=%d", len(gs))
	}
	_, evs, _ := f.st.Get(context.Background(), f.org, gs[0].ID)
	d := evs[0].Diagnostic
	if !d.Retryable || !d.Indeterminate || !contains(d.ProviderCode, "upstream_unreachable") || d.Kind != diag.KindHTTP {
		t.Fatalf("diag=%+v", d)
	}
}

func TestDiag_TrapIsRecordedAsRuntime(t *testing.T) {
	f := newDiagFixture(t, 5000)
	if _, err := f.invoke(t, "boom"); err == nil {
		t.Fatal("expected trap")
	}
	gs := f.groups(t)
	if len(gs) != 1 || gs[0].Kind != "runtime" || gs[0].Code != "trap" {
		t.Fatalf("groups=%+v", gs)
	}
	_, evs, _ := f.st.Get(context.Background(), f.org, gs[0].ID)
	if len(evs[0].Diagnostic.Breadcrumbs) != 1 {
		t.Fatalf("breadcrumbs lost: %+v", evs[0].Diagnostic)
	}
}

func TestDiag_BudgetTimeoutIsRecorded(t *testing.T) {
	f := newDiagFixture(t, 150)
	if _, err := f.invoke(t, "spin"); err == nil {
		t.Fatal("expected timeout")
	}
	gs := f.groups(t)
	if len(gs) != 1 || gs[0].Code != "timeout" {
		t.Fatalf("groups=%+v", gs)
	}
}

func TestDiag_EgressDeniedIsDiagnosed(t *testing.T) {
	f := newDiagFixture(t, 5000)
	stubHTTP(t, func(*http.Request) (*http.Response, error) { t.Fatal("must not be called"); return nil, nil })
	// The guest URL host is api.provider.example; rebuild the host without the grant.
	f.h.caps = security.Compile("diagaddon", nil)
	f.h.addonCaps = nil
	if _, err := f.invoke(t, "pay"); err != nil {
		t.Fatal(err)
	}
	gs := f.groups(t)
	if len(gs) != 1 {
		t.Fatalf("groups=%d", len(gs))
	}
	_, evs, _ := f.st.Get(context.Background(), f.org, gs[0].ID)
	if !contains(evs[0].Diagnostic.ProviderCode, "egress_denied") || evs[0].Diagnostic.UpstreamHost != "api.provider.example" {
		t.Fatalf("diag=%+v", evs[0].Diagnostic)
	}
}

func TestDiag_OffCapturesNothing(t *testing.T) {
	f := newDiagFixture(t, 5000)
	off := false
	f.h.WithDiagnostics(diag.NewRecorder(f.st, diag.Options{Enabled: &off}))
	if _, err := f.invoke(t, "boom"); err == nil {
		t.Fatal("expected trap")
	}
	if gs := f.groups(t); len(gs) != 0 {
		t.Fatalf("groups=%v", gs)
	}
	f.h.WithDiagnostics(nil)
	if _, err := f.invoke(t, "boom"); err == nil {
		t.Fatal("expected trap")
	}
}

func TestDiag_ClassifyRuntime(t *testing.T) {
	cases := []struct {
		err  error
		code string
	}{
		{&AddonNotLoadedError{AddonKey: "x"}, "addon_not_loaded"},
		{&faultError{code: "pool_busy", err: errors.New("busy")}, "pool_busy"},
		{&faultError{code: "memory_limit", err: errors.New("mem")}, "memory_limit"},
		{&faultError{code: "canceled", err: context.Canceled}, "canceled"},
		{errors.New(`wasm: function "x" not in backend.exports`), "export_missing"},
		{context.DeadlineExceeded, "timeout"},
		{errors.New("boom"), "runtime_error"},
	}
	for _, c := range cases {
		if got, _ := classifyRuntime(c.err); got != c.code {
			t.Errorf("%v => %s, want %s", c.err, got, c.code)
		}
	}
	// transparent wrapper
	fe := &faultError{code: "trap", err: fmt.Errorf("wasm: call x: %w", context.Canceled)}
	if !errors.Is(fe, context.Canceled) || fe.Error() != "wasm: call x: context canceled" {
		t.Fatal("faultError must be transparent")
	}
}
