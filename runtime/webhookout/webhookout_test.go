package webhookout

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	v3 "github.com/asteby/metacore-kernel/manifest/v3"
	"github.com/asteby/metacore-kernel/netguard"
)

// Fixed seed: Ed25519 is deterministic, so signatures are golden-testable.
func testSigner(t testing.TB) (Signer, ed25519.PublicKey) {
	t.Helper()
	priv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))
	s, err := NewEd25519Signer(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s, priv.Public().(ed25519.PublicKey)
}

func handler(path, method string) v3.Handler {
	zero := 0
	return v3.Handler{
		Type: "webhook", Path: path, Method: method, Retries: &zero,
		Signing: &v3.HandlerSigning{Scheme: SchemeManifest},
	}
}

// captured is one request as the server saw it.
type captured struct {
	req  *http.Request
	body []byte
}

// recv starts a receiver on 127.0.0.1 that runs the reference verifier and
// answers with status/body. The client policy explicitly authorizes it.
func recv(t *testing.T, status int, resp string, seen *[]captured) (*httptest.Server, Policy) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*seen = append(*seen, captured{r.Clone(context.Background()), b})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, resp)
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return srv, Policy{AllowInternal: []string{u.Host}, AllowInsecureHTTP: true}
}

func baseReq(srvURL string) Request {
	return Request{
		Handler:  handler("/core/v1/licenses/{record.license_id}/renew", "POST"),
		BaseURL:  srvURL,
		Record:   map[string]any{"license_id": "lic_123"},
		Payload:  map[string]any{"term_days": 30, "note": "<ok> & fine"},
		Actor:    "staff@example.com",
		AddonKey: "demo", OrgID: "org1", Model: "License", Action: "renew", RecordID: "lic_123",
		InvocationID: "req-1",
	}
}

func TestRoundTripAgainstReferenceVerifier(t *testing.T) {
	signer, pub := testSigner(t)
	var seen []captured
	srv, pol := recv(t, 200, `{"success":true,"data":{"id":"lic_123","license_key":"SECRET-KEY","nested":{"license_token":"SECRET-TOK"}},"message":"listo"}`, &seen)
	c := New(Config{Signer: signer, Policy: pol})

	res, err := c.Do(context.Background(), baseReq(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 {
		t.Fatalf("requests = %d", len(seen))
	}
	got := seen[0]
	pk, verr := refVerify(got.req, got.body, time.Now())
	if verr != nil {
		t.Fatalf("reference verifier rejected a legitimate request: %v", verr)
	}
	if pk != hex.EncodeToString(pub) {
		t.Fatal("public key mismatch")
	}
	if got.req.Method != "POST" || got.req.URL.Path != "/core/v1/licenses/lic_123/renew" {
		t.Fatalf("route = %s %s", got.req.Method, got.req.URL.Path)
	}
	if got.req.Header.Get("X-Actor") != "staff@example.com" || got.req.Header.Get("Idempotency-Key") != res.IdempotencyKey {
		t.Fatal("actor / idempotency headers")
	}
	// Flat JSON body, no HTML escaping, strict-decodable.
	if string(got.body) != `{"note":"<ok> & fine","term_days":30}` {
		t.Fatalf("body = %s", got.body)
	}
	if res.Message != "listo" || res.Status != 200 {
		t.Fatalf("result = %+v", res)
	}
	// Secrets never come back.
	if bytes.Contains(res.Body, []byte("SECRET")) || strings.Contains(strings.ToLower(string(res.Body)), "secret-") {
		t.Fatalf("secret leaked in result body: %s", res.Body)
	}
	if !bytes.Contains(res.Body, []byte(Redacted)) {
		t.Fatalf("expected redaction marker: %s", res.Body)
	}
}

func TestAdversarialMutationsAreRejectedByVerifier(t *testing.T) {
	signer, _ := testSigner(t)
	var seen []captured
	srv, pol := recv(t, 200, `{}`, &seen)
	c := New(Config{Signer: signer, Policy: pol})
	if _, err := c.Do(context.Background(), baseReq(srv.URL)); err != nil {
		t.Fatal(err)
	}
	orig := seen[0]
	now := time.Now()
	if _, err := refVerify(orig.req, orig.body, now); err != nil {
		t.Fatalf("baseline: %v", err)
	}

	mutate := func(name string, f func(r *http.Request, b []byte) []byte, want string) {
		t.Run(name, func(t *testing.T) {
			r := orig.req.Clone(context.Background())
			r.Header = orig.req.Header.Clone()
			u := *orig.req.URL
			r.URL = &u
			b := f(r, append([]byte(nil), orig.body...))
			_, err := refVerify(r, b, now)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("want %q, got %v", want, err)
			}
		})
	}
	mutate("replay on another route", func(r *http.Request, b []byte) []byte {
		r.URL.Path = "/core/v1/licenses/lic_123/revoke"
		return b
	}, "signature_mismatch")
	mutate("replay on another id", func(r *http.Request, b []byte) []byte {
		r.URL.Path = "/core/v1/licenses/lic_999/renew"
		return b
	}, "signature_mismatch")
	mutate("another method", func(r *http.Request, b []byte) []byte { r.Method = "PUT"; return b }, "signature_mismatch")
	mutate("query added", func(r *http.Request, b []byte) []byte { r.URL.RawQuery = "force=1"; return b }, "signature_mismatch")
	mutate("body altered", func(r *http.Request, b []byte) []byte { return bytes.Replace(b, []byte("30"), []byte("99"), 1) }, "signature_mismatch")
	mutate("actor altered", func(r *http.Request, b []byte) []byte {
		r.Header.Set("X-Actor", "other@example.com")
		return b
	}, "signature_mismatch")
	mutate("idempotency key altered", func(r *http.Request, b []byte) []byte {
		r.Header.Set("Idempotency-Key", "mc1-other")
		return b
	}, "signature_mismatch")
	mutate("another public key", func(r *http.Request, b []byte) []byte {
		pub, _, _ := ed25519.GenerateKey(nil)
		r.Header.Set("X-Instance-Pubkey", hex.EncodeToString(pub))
		return b
	}, "signature_mismatch")
	mutate("timestamp moved", func(r *http.Request, b []byte) []byte {
		r.Header.Set("X-Signature-Timestamp", "1")
		return b
	}, "stale_timestamp")
	mutate("scheme dropped", func(r *http.Request, b []byte) []byte {
		r.Header.Del("X-Signature-Scheme")
		return b
	}, "invalid_signature_headers")
}

func TestGoldenSignature(t *testing.T) {
	signer, _ := testSigner(t)
	body := []byte(`{"reason":"x"}`)
	ts := time.Unix(1_780_000_000, 0)
	canon := Canonical("post", "/core/v1/licenses/lic_1/revoke", "", "mc1-abc", "staff@example.com", "1780000000", body)
	sig, tsh, err := SignatureHeaders(signer, "POST", "/core/v1/licenses/lic_1/revoke", "", "mc1-abc", "staff@example.com", body, ts)
	if err != nil || tsh != "1780000000" {
		t.Fatal(err, tsh)
	}
	got := map[string]string{"canonical": string(canon), "x_signature": sig, "x_signature_timestamp": tsh,
		"idempotency_key": DeriveIdempotencyKey("demo", "org1", "License", "revoke", "lic_1", "req-1")}
	out, _ := json.MarshalIndent(got, "", "  ")
	out = append(out, '\n')
	const path = "testdata/golden_sign.json"
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, out, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.TrimSpace(want), bytes.TrimSpace(out)) {
		t.Fatalf("golden mismatch:\nwant %s\ngot  %s", want, out)
	}
	// And the reference canonical form is byte-identical.
	if !bytes.Equal(canon, refCanonical("POST", "/core/v1/licenses/lic_1/revoke", "", "mc1-abc", "staff@example.com", "1780000000", body)) {
		t.Fatal("Canonical differs from the reference transcription")
	}
}

func TestIdempotencyKeyStableAndDistinct(t *testing.T) {
	a := DeriveIdempotencyKey("a", "o", "M", "x", "r1", "req-1")
	if a != DeriveIdempotencyKey("a", "o", "M", "x", "r1", "req-1") {
		t.Fatal("not stable")
	}
	for _, other := range []string{
		DeriveIdempotencyKey("a", "o", "M", "x", "r1", "req-2"),
		DeriveIdempotencyKey("a", "o", "M", "x", "r2", "req-1"),
		DeriveIdempotencyKey("a", "o", "M", "y", "r1", "req-1"),
		DeriveIdempotencyKey("a", "o", "Mx", "", "r1", "req-1"), // no concatenation ambiguity
	} {
		if other == a {
			t.Fatal("collision")
		}
	}
	if !refIdemKey.MatchString(a) {
		t.Fatalf("key %q outside the receiver grammar", a)
	}
}

func TestSameInvocationSameKeyAcrossRetries(t *testing.T) {
	signer, _ := testSigner(t)
	var n int32
	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		if atomic.AddInt32(&n, 1) < 3 {
			w.WriteHeader(503)
			return
		}
		_, _ = io.WriteString(w, `{"success":true}`)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	c := New(Config{Signer: signer, Policy: Policy{AllowInternal: []string{u.Host}, AllowInsecureHTTP: true}})
	req := baseReq(srv.URL)
	two := 2
	req.Handler.Retries = &two
	res, err := c.Do(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Attempts != 3 || len(keys) != 3 || keys[0] != keys[1] || keys[1] != keys[2] || keys[0] != res.IdempotencyKey {
		t.Fatalf("attempts=%d keys=%v", res.Attempts, keys)
	}
}

func TestFailClosed(t *testing.T) {
	signer, _ := testSigner(t)
	cases := []struct {
		name   string
		status int
		body   string
		code   string
		http   int
	}{
		{"400", 400, `{"error":{"code":"x","message":"campo invalido"}}`, CodeRejected, 422},
		{"404", 404, `nope`, CodeRejected, 422},
		{"409", 409, `{"error":{"message":"conflict"}}`, CodeRejected, 422},
		{"redirect", 302, ``, CodeRejected, 422},
		{"429", 429, ``, CodeUnavailable, 502},
		{"500", 500, `{"license_key":"LEAK-ME"}`, CodeUnavailable, 502},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seen []captured
			srv, pol := recv(t, tc.status, tc.body, &seen)
			c := New(Config{Signer: signer, Policy: pol})
			res, err := c.Do(context.Background(), baseReq(srv.URL))
			if err == nil || res != nil {
				t.Fatalf("non-2xx must be an error, got res=%v err=%v", res, err)
			}
			var ae *ActionError
			if !errors.As(err, &ae) || ae.Code != tc.code || ae.HTTPStatus() != tc.http || ae.Status != tc.status {
				t.Fatalf("err = %#v", err)
			}
			if strings.Contains(err.Error(), "LEAK-ME") {
				t.Fatal("secret in error")
			}
			if tc.status == 400 && ae.Message != "campo invalido" {
				t.Fatalf("message = %q", ae.Message)
			}
		})
	}
}

func TestRedirectNotFollowed(t *testing.T) {
	signer, _ := testSigner(t)
	var hits int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { atomic.AddInt32(&hits, 1) }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/x", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	u1, _ := url.Parse(srv.URL)
	u2, _ := url.Parse(target.URL)
	c := New(Config{Signer: signer, Policy: Policy{AllowInternal: []string{u1.Host, u2.Host}, AllowInsecureHTTP: true}})
	if _, err := c.Do(context.Background(), baseReq(srv.URL)); err == nil {
		t.Fatal("3xx must fail")
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Fatal("redirect was followed")
	}
}

func TestRetryOn5xxNotOn4xx(t *testing.T) {
	signer, _ := testSigner(t)
	for _, tc := range []struct{ status, want int }{{503, 3}, {400, 1}} {
		var n int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&n, 1)
			w.WriteHeader(tc.status)
		}))
		u, _ := url.Parse(srv.URL)
		c := New(Config{Signer: signer, Policy: Policy{AllowInternal: []string{u.Host}, AllowInsecureHTTP: true}})
		req := baseReq(srv.URL)
		two := 2
		req.Handler.Retries = &two
		_, err := c.Do(context.Background(), req)
		srv.Close()
		if err == nil || int(atomic.LoadInt32(&n)) != tc.want {
			t.Fatalf("status %d: attempts=%d err=%v", tc.status, n, err)
		}
	}
}

func TestTimeoutBudget(t *testing.T) {
	signer, _ := testSigner(t)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	u, _ := url.Parse(srv.URL)
	c := New(Config{Signer: signer, Policy: Policy{AllowInternal: []string{u.Host}, AllowInsecureHTTP: true}})
	req := baseReq(srv.URL)
	req.Handler.TimeoutMS = 1000
	start := time.Now()
	_, err := c.Do(context.Background(), req)
	var ae *ActionError
	if !errors.As(err, &ae) || ae.Code != CodeTimeout || ae.HTTPStatus() != 504 {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 4*time.Second {
		t.Fatal("budget not respected")
	}
}

func TestResponseBodyLimit(t *testing.T) {
	signer, _ := testSigner(t)
	big := `{"data":"` + strings.Repeat("a", 5000) + `"}`
	var seen []captured
	srv, pol := recv(t, 200, big, &seen)
	c := New(Config{Signer: signer, Policy: pol, MaxResponseBytes: 1024})
	res, err := c.Do(context.Background(), baseReq(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || res.Body != nil || res.Data != nil {
		t.Fatalf("expected truncated result without body: %+v", res)
	}
}

func TestSSRFDefaultsDeny(t *testing.T) {
	signer, _ := testSigner(t)
	var seen []captured
	srv, _ := recv(t, 200, `{}`, &seen)
	c := New(Config{Signer: signer}) // zero policy
	for _, base := range []string{
		srv.URL,                      // loopback http
		"https://127.0.0.1",          // loopback literal
		"https://10.1.2.3",           // private
		"https://192.168.0.5",        // private
		"https://169.254.169.254",    // cloud metadata
		"https://[::1]",              // v6 loopback
		"https://[::ffff:127.0.0.1]", // mapped
		"https://localhost",          // name
		"https://core.internal",      // forbidden suffix
		"https://intranet",           // single label
		"http://example.com",         // cleartext public
		"https://example.com:8443",   // odd port, no allowlist
		"ftp://example.com",
	} {
		_, err := c.Do(context.Background(), baseReq(base))
		var ae *ActionError
		if !errors.As(err, &ae) || ae.Code != CodeMisconfigured || !errors.Is(err, ErrDestinationBlocked) {
			t.Errorf("%s: want blocked, got %v", base, err)
		}
	}
	if len(seen) != 0 {
		t.Fatal("a blocked destination received a request")
	}
}

func TestSSRFExplicitInternalOnlyForThatDestination(t *testing.T) {
	signer, _ := testSigner(t)
	var seen []captured
	srv, pol := recv(t, 200, `{}`, &seen)
	c := New(Config{Signer: signer, Policy: pol})
	if _, err := c.Do(context.Background(), baseReq(srv.URL)); err != nil {
		t.Fatalf("authorized internal host rejected: %v", err)
	}
	// Another loopback port is NOT authorized by an entry carrying a port.
	u, _ := url.Parse(srv.URL)
	other := "http://127.0.0.1:1"
	if u.Port() == "1" {
		other = "http://127.0.0.1:2"
	}
	if _, err := c.Do(context.Background(), baseReq(other)); !errors.Is(err, ErrDestinationBlocked) {
		t.Fatalf("unauthorized loopback port reached: %v", err)
	}
	// Cleartext needs AllowInsecureHTTP even for an authorized host.
	c2 := New(Config{Signer: signer, Policy: Policy{AllowInternal: pol.AllowInternal}})
	if _, err := c2.Do(context.Background(), baseReq(srv.URL)); !errors.Is(err, ErrDestinationBlocked) {
		t.Fatalf("http without AllowInsecureHTTP: %v", err)
	}
}

func TestDialTimeGuardBlocksResolvedPrivateIP(t *testing.T) {
	// The pre-check passes a public-looking name; the connect-time guard must
	// still refuse an address that resolves to a blocked IP (DNS rebinding).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	defer ln.Close()
	tr := (Policy{}).newHTTPClient().Transport.(*http.Transport)
	_, err = tr.DialContext(context.Background(), "tcp", ln.Addr().String())
	if !errors.Is(err, netguard.ErrBlockedEgress) {
		t.Fatalf("dial to loopback must be blocked, got %v", err)
	}
	// Authorized by name: dial goes through.
	pol := Policy{AllowInternal: []string{ln.Addr().String()}}
	tr = pol.newHTTPClient().Transport.(*http.Transport)
	conn, err := tr.DialContext(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("authorized dial failed: %v", err)
	}
	conn.Close()
}

func TestPublicHostAllowList(t *testing.T) {
	p := Policy{AllowHosts: []string{"core.example.com"}}
	for host, ok := range map[string]bool{
		"https://core.example.com":      true,
		"https://other.example.com":     false,
		"https://core.example.com:8443": false,
	} {
		u, _ := url.Parse(host + "/x")
		if got := p.check(u) == nil; got != ok {
			t.Errorf("%s: allowed=%v want %v", host, got, ok)
		}
	}
}

func TestNeverUnsigned(t *testing.T) {
	var seen []captured
	srv, pol := recv(t, 200, `{}`, &seen)
	signer, _ := testSigner(t)

	if _, err := New(Config{Policy: pol}).Do(context.Background(), baseReq(srv.URL)); !errors.Is(err, ErrNoSigner) {
		t.Fatalf("no signer: %v", err)
	}
	for name, mut := range map[string]func(*Request){
		"no signing block": func(r *Request) { r.Handler.Signing = nil },
		"hmac scheme":      func(r *Request) { r.Handler.Signing = &v3.HandlerSigning{Scheme: "metacore-hmac-v1", SecretRef: "a.b"} },
		"bad actor":        func(r *Request) { r.Actor = "not-an-email" },
		"empty actor":      func(r *Request) { r.Actor = "" },
		"wrong type":       func(r *Request) { r.Handler.Type = "wasm" },
		"bad method":       func(r *Request) { r.Handler.Method = "GET" },
		"timeout range":    func(r *Request) { r.Handler.TimeoutMS = 5 },
		"retries range":    func(r *Request) { n := 9; r.Handler.Retries = &n },
		"no path":          func(r *Request) { r.Handler.Path = "" },
		"base with path":   func(r *Request) { r.BaseURL += "/api" },
		"base with creds":  func(r *Request) { r.BaseURL = strings.Replace(r.BaseURL, "http://", "http://u:p@", 1) },
		"missing record":   func(r *Request) { r.Record = nil },
	} {
		r := baseReq(srv.URL)
		mut(&r)
		if _, err := New(Config{Signer: signer, Policy: pol}).Do(context.Background(), r); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	if len(seen) != 0 {
		t.Fatalf("%d requests escaped despite misconfiguration", len(seen))
	}
}

func TestExpandPath(t *testing.T) {
	rec := map[string]any{"v": "ops", "ver": "1.2.3", "n": float64(42), "empty": "", "slash": "a/b", "dots": "..", "obj": map[string]any{}}
	ok := map[string]string{
		"/core/v1/rollouts/{record.v}/{record.ver}/promote": "/core/v1/rollouts/ops/1.2.3/promote",
		"/x/{record.n}":   "/x/42",
		"/x/{payload.id}": "/x/p%201",
		"/plain":          "/plain",
	}
	for in, want := range ok {
		got, err := ExpandPath(in, rec, map[string]any{"id": "p 1"})
		if err != nil || got != want {
			t.Errorf("%s -> %q, %v (want %q)", in, got, err, want)
		}
	}
	for _, bad := range []string{
		"x", "//x", "/x/{record.empty}", "/x/{record.slash}", "/x/{record.dots}", "/x/{record.nope}",
		"/x/{record.obj}", "/x/{other.v}", "/x/{record.v", "/x/}", "/x?a=1", "/x#f", "/a/../b", "/a/./b", `/a\b`,
	} {
		if got, err := ExpandPath(bad, rec, nil); err == nil {
			t.Errorf("%q must be rejected, got %q", bad, got)
		}
	}
}

func TestConfirmTyped(t *testing.T) {
	ct := &v3.ConfirmTyped{Field: "customer_email"}
	rec := map[string]any{"customer_email": "a@example.com"}
	if err := VerifyConfirmTyped(nil, rec, nil); err != nil {
		t.Fatal(err)
	}
	if err := VerifyConfirmTyped(ct, rec, map[string]any{"confirm_text": "  a@example.com "}); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]map[string]any{
		"missing": {}, "wrong": {"confirm_text": "b@example.com"}, "case": {"confirm_text": "A@example.com"},
		"empty": {"confirm_text": ""}, "non-string": {"confirm_text": []any{"a@example.com"}},
	} {
		if err := VerifyConfirmTyped(ct, rec, p); !errors.Is(err, ErrConfirmMismatch) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A record with no value to confirm can never be confirmed with "".
	if err := VerifyConfirmTyped(ct, map[string]any{}, map[string]any{"confirm_text": ""}); !errors.Is(err, ErrConfirmMismatch) {
		t.Fatal("empty expected value accepted")
	}
}

func TestDoEnforcesConfirmTypedAndStripsConfirmText(t *testing.T) {
	signer, _ := testSigner(t)
	var seen []captured
	srv, pol := recv(t, 200, `{}`, &seen)
	c := New(Config{Signer: signer, Policy: pol})
	req := baseReq(srv.URL)
	req.ConfirmTyped = &v3.ConfirmTyped{Field: "license_id"}

	_, err := c.Do(context.Background(), req)
	var ae *ActionError
	if !errors.As(err, &ae) || ae.Code != CodeConfirmMismatch || len(seen) != 0 {
		t.Fatalf("err=%v seen=%d", err, len(seen))
	}
	req.Payload = map[string]any{"term_days": 30, ConfirmTextKey: "lic_123"}
	if _, err := c.Do(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(seen[0].body), ConfirmTextKey) {
		t.Fatalf("confirm_text travelled to the destination: %s", seen[0].body)
	}
	if _, err := refVerify(seen[0].req, seen[0].body, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestEmptyPayloadSendsEmptyObject(t *testing.T) {
	signer, _ := testSigner(t)
	var seen []captured
	srv, pol := recv(t, 200, `{}`, &seen)
	req := baseReq(srv.URL)
	req.Payload = nil
	if _, err := New(Config{Signer: signer, Policy: pol}).Do(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if string(seen[0].body) != `{}` {
		t.Fatalf("body = %q", seen[0].body)
	}
	if _, err := refVerify(seen[0].req, seen[0].body, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestMethodsRoundTrip(t *testing.T) {
	signer, _ := testSigner(t)
	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		var seen []captured
		srv, pol := recv(t, 200, `{}`, &seen)
		req := baseReq(srv.URL)
		req.Handler.Method = strings.ToLower(m)
		if _, err := New(Config{Signer: signer, Policy: pol}).Do(context.Background(), req); err != nil {
			t.Fatalf("%s: %v", m, err)
		}
		if seen[0].req.Method != m {
			t.Fatalf("method %s sent as %s", m, seen[0].req.Method)
		}
		if _, err := refVerify(seen[0].req, seen[0].body, time.Now()); err != nil {
			t.Fatalf("%s: %v", m, err)
		}
	}
}

func TestSignatureHeadersRejectsBadSigner(t *testing.T) {
	if _, err := NewEd25519Signer(ed25519.PrivateKey("short")); err == nil {
		t.Fatal("short key accepted")
	}
	if _, _, err := SignatureHeaders(badSigner{}, "POST", "/x", "", "k", "a@b.co", nil, time.Now()); err == nil {
		t.Fatal("malformed signature accepted")
	}
}

type badSigner struct{}

func (badSigner) PublicKeyHex() string        { return strings.Repeat("0", 64) }
func (badSigner) Sign([]byte) ([]byte, error) { return []byte("short"), nil }

func TestErrorsNeverCarryKeysOrSignatures(t *testing.T) {
	signer, _ := testSigner(t)
	var seen []captured
	srv, pol := recv(t, 500, `{"error":{"message":"boom"}}`, &seen)
	_, err := New(Config{Signer: signer, Policy: pol}).Do(context.Background(), baseReq(srv.URL))
	if err == nil {
		t.Fatal("expected error")
	}
	sig := seen[0].req.Header.Get("X-Signature")
	for _, leak := range []string{sig, strings.TrimPrefix(sig, "ed25519:"), strings.Repeat("07", 32)} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("error leaks %q", leak)
		}
	}
}
