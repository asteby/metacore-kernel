package native

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIngressValidate(t *testing.T) {
	cases := []struct {
		name string
		in   Ingress
		ok   bool
	}{
		{"minimal", Ingress{Kind: "http"}, true},
		{"full", Ingress{Kind: "http", PathPrefix: "/shop", MaxBodyBytes: 1 << 20, Timeout: "15s"}, true},
		{"root", Ingress{Kind: "http", PathPrefix: "/"}, true},
		{"bad kind", Ingress{Kind: "tcp"}, false},
		{"empty kind", Ingress{}, false},
		{"relative", Ingress{Kind: "http", PathPrefix: "shop"}, false},
		{"trailing slash", Ingress{Kind: "http", PathPrefix: "/shop/"}, false},
		{"traversal", Ingress{Kind: "http", PathPrefix: "/a/../b"}, false},
		{"query", Ingress{Kind: "http", PathPrefix: "/a?b"}, false},
		{"encoded", Ingress{Kind: "http", PathPrefix: "/a%2f"}, false},
		{"negative body", Ingress{Kind: "http", MaxBodyBytes: -1}, false},
		{"huge body", Ingress{Kind: "http", MaxBodyBytes: MaxIngressMaxBodyBytes + 1}, false},
		{"bad timeout", Ingress{Kind: "http", Timeout: "soon"}, false},
		{"zero timeout", Ingress{Kind: "http", Timeout: "0s"}, false},
		{"long timeout", Ingress{Kind: "http", Timeout: "6m"}, false},
	}
	for _, tc := range cases {
		err := tc.in.Validate()
		if (err == nil) != tc.ok {
			t.Errorf("%s: ok=%v err=%v", tc.name, tc.ok, err)
		}
	}
}

func TestSpecValidateIngressBothScopes(t *testing.T) {
	for _, scope := range []string{ScopeInstance, ScopeInstallation} {
		s := validSpec()
		s.Scope = scope
		s.Ingress = &Ingress{Kind: IngressKindHTTP}
		if err := s.Validate(); err != nil {
			t.Fatalf("scope %s: %v", scope, err)
		}
	}
	s := validSpec()
	s.Ingress = &Ingress{Kind: "grpc"}
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "ingress.kind") {
		t.Fatalf("expected ingress.kind error, got %v", err)
	}
}

func TestIngressEffectiveAndMatches(t *testing.T) {
	e := Ingress{Kind: "http"}.Effective()
	if e.PathPrefix != "/" || e.MaxBodyBytes != DefaultIngressMaxBodyBytes || e.Timeout != DefaultIngressTimeout {
		t.Fatalf("defaults: %#v", e)
	}
	e = Ingress{Kind: "http", PathPrefix: "/shop", Timeout: "3s", MaxBodyBytes: 10}.Effective()
	if e.Timeout != 3*time.Second || e.MaxBodyBytes != 10 {
		t.Fatalf("explicit: %#v", e)
	}
	for p, want := range map[string]bool{"/shop": true, "/shop/x": true, "/shopping": false, "/": false} {
		if e.Matches(p) != want {
			t.Errorf("Matches(%q) != %v", p, want)
		}
	}
	if (Status{State: StateHealthy}).AcceptsIngress() == false || (Status{State: StateDegraded}).AcceptsIngress() {
		t.Fatal("only healthy services accept ingress")
	}
}

func TestIngressSpecJSONRoundTrip(t *testing.T) {
	s := validSpec()
	s.Ingress = &Ingress{Kind: "http", PathPrefix: "/", MaxBodyBytes: 2048, Timeout: "10s"}
	raw, _ := json.Marshal(s)
	if !strings.Contains(string(raw), `"ingress":{"kind":"http","path_prefix":"/","max_body_bytes":2048,"timeout":"10s"}`) {
		t.Fatalf("unexpected json: %s", raw)
	}
	s.Ingress = nil
	raw, _ = json.Marshal(s)
	if strings.Contains(string(raw), "ingress") {
		t.Fatalf("nil ingress must be omitted: %s", raw)
	}
}

// startUnixUpstream serves h on a fresh unix socket, like a sidecar would on
// EnvIngressSocket.
func startUnixUpstream(t *testing.T, h http.Handler) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ingress")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "ingress.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return sock
}

func newTestProxy(t *testing.T, sock string, ing Ingress, opts IngressProxyOptions) *httptest.Server {
	t.Helper()
	p, err := NewIngressProxy(sock, ing, opts)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(p)
	t.Cleanup(func() { front.Close(); p.CloseIdleConnections() })
	return front
}

func TestIngressProxyHeaders(t *testing.T) {
	got := make(chan *http.Request, 1)
	sock := startUnixUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Clone(r.Context())
		w.Header().Set("Connection", "X-Upstream-Hop")
		w.Header().Set("X-Upstream-Hop", "leak")
		w.Header().Set("X-Request-Id", "upstream-forged")
		w.Header().Set("X-App", "ok")
		w.WriteHeader(http.StatusTeapot)
		_, _ = io.WriteString(w, "hello "+r.URL.RequestURI())
	}))
	front := newTestProxy(t, sock, Ingress{Kind: "http"}, IngressProxyOptions{
		TrustedHeaders: []string{"x-storefront-domain"},
		SetTrustedHeaders: func(in *http.Request, out http.Header) {
			out.Set("X-Storefront-Domain", strings.ToLower(in.Host))
		},
		RequestID: func(*http.Request) string { return "req-1" },
	})

	req, _ := http.NewRequest(http.MethodGet, front.URL+"/p/llanta?x=1", nil)
	req.Host = "MiTienda.example.com"
	req.Header.Set("X-Forwarded-For", "6.6.6.6")
	req.Header.Set("X-Forwarded-Host", "evil.example")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("Forwarded", "for=6.6.6.6")
	req.Header.Set("X-Real-Ip", "6.6.6.6")
	req.Header.Set("X-Request-Id", "client-forged")
	req.Header.Set("X-Storefront-Domain", "victim.example")
	req.Header.Set("X-Metacore-Org", "victim")
	req.Header.Set("Connection", "X-Client-Hop")
	req.Header.Set("X-Client-Hop", "leak")
	req.Header.Set("Cookie", "cart=abc")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusTeapot || string(body) != "hello /p/llanta?x=1" {
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
	if resp.Header.Get("X-Request-Id") != "req-1" || len(resp.Header.Values("X-Request-Id")) != 1 {
		t.Fatalf("response request id: %v", resp.Header.Values("X-Request-Id"))
	}
	if resp.Header.Get("X-Upstream-Hop") != "" || resp.Header.Get("X-App") != "ok" {
		t.Fatalf("response hop-by-hop not stripped: %v", resp.Header)
	}

	up := <-got
	h := up.Header
	checks := map[string]string{
		"X-Forwarded-For":     "127.0.0.1",
		"X-Forwarded-Host":    "MiTienda.example.com",
		"X-Forwarded-Proto":   "http",
		"X-Request-Id":        "req-1",
		"X-Storefront-Domain": "mitienda.example.com",
		"Cookie":              "cart=abc",
		"Forwarded":           "",
		"X-Real-Ip":           "",
		"X-Metacore-Org":      "",
		"X-Client-Hop":        "",
	}
	for k, want := range checks {
		if v := strings.Join(h.Values(k), ","); v != want {
			t.Errorf("upstream %s = %q, want %q", k, v, want)
		}
	}
	if up.Host != "MiTienda.example.com" {
		t.Errorf("upstream Host = %q", up.Host)
	}
}

func TestIngressProxyPathPrefixAndUpgrade(t *testing.T) {
	sock := startUnixUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.URL.Path)
	}))
	front := newTestProxy(t, sock, Ingress{Kind: "http", PathPrefix: "/shop"}, IngressProxyOptions{})
	for p, code := range map[string]int{"/shop": 200, "/shop/a": 200, "/shopping": 404, "/admin": 404} {
		resp, err := http.Get(front.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != code {
			t.Errorf("%s: %d want %d", p, resp.StatusCode, code)
		}
		if resp.Header.Get("X-Request-Id") == "" {
			t.Errorf("%s: missing request id", p)
		}
	}
	req, _ := http.NewRequest(http.MethodGet, front.URL+"/shop/ws", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("upgrade: %d", resp.StatusCode)
	}
}

func TestIngressProxyStreamsResponse(t *testing.T) {
	release := make(chan struct{})
	sock := startUnixUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "chunk-1\n")
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "chunk-2\n")
	}))
	// Timeout covers response headers only; the stream may outlive it.
	front := newTestProxy(t, sock, Ingress{Kind: "http", Timeout: "200ms"}, IngressProxyOptions{})
	resp, err := http.Get(front.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	line, err := br.ReadString('\n')
	if err != nil || line != "chunk-1\n" {
		t.Fatalf("first chunk not streamed: %q %v", line, err)
	}
	time.Sleep(300 * time.Millisecond)
	close(release)
	line, err = br.ReadString('\n')
	if err != nil || line != "chunk-2\n" {
		t.Fatalf("second chunk: %q %v", line, err)
	}
}

func TestIngressProxyStreamsRequestBody(t *testing.T) {
	sock := startUnixUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_, _ = io.WriteString(w, string(b))
	}))
	front := newTestProxy(t, sock, Ingress{Kind: "http", MaxBodyBytes: 1024}, IngressProxyOptions{})
	pr, pw := io.Pipe()
	go func() {
		for i := 0; i < 4; i++ {
			_, _ = pw.Write([]byte("abcd"))
		}
		pw.Close()
	}()
	resp, err := http.Post(front.URL+"/cart", "text/plain", pr) // chunked, unknown length
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != strings.Repeat("abcd", 4) {
		t.Fatalf("status=%d body=%q", resp.StatusCode, b)
	}
}

func TestIngressProxyTimeout504(t *testing.T) {
	sock := startUnixUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	front := newTestProxy(t, sock, Ingress{Kind: "http", Timeout: "100ms"}, IngressProxyOptions{})
	start := time.Now()
	resp, err := http.Get(front.URL + "/slow")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("timeout not enforced: %s", time.Since(start))
	}
}

func TestIngressProxyUpstreamDown502(t *testing.T) {
	dir, err := os.MkdirTemp("", "ingress")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	front := newTestProxy(t, filepath.Join(dir, "missing.sock"), Ingress{Kind: "http"}, IngressProxyOptions{})
	resp, err := http.Get(front.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || resp.Header.Get("X-Request-Id") == "" {
		t.Fatalf("status %d headers %v", resp.StatusCode, resp.Header)
	}
}

func TestIngressProxyBodyCap413(t *testing.T) {
	sock := startUnixUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	front := newTestProxy(t, sock, Ingress{Kind: "http", MaxBodyBytes: 8}, IngressProxyOptions{})

	// Declared length over the cap: rejected before dialing.
	resp, err := http.Post(front.URL+"/", "text/plain", strings.NewReader("0123456789"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("content-length over cap: %d", resp.StatusCode)
	}

	// Chunked body that grows past the cap mid-stream.
	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write([]byte("0123456789abcdef"))
		pw.Close()
	}()
	resp, err = http.Post(front.URL+"/", "text/plain", pr)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("chunked over cap: %d", resp.StatusCode)
	}

	// Within the cap passes.
	resp, err = http.Post(front.URL+"/", "text/plain", strings.NewReader("small"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("within cap: %d", resp.StatusCode)
	}
}

func TestNewIngressProxyRejectsInvalid(t *testing.T) {
	if _, err := NewIngressProxy("/run/x.sock", Ingress{Kind: "tcp"}, IngressProxyOptions{}); err == nil {
		t.Fatal("invalid ingress must be rejected")
	}
	if _, err := NewIngressProxy("", Ingress{Kind: "http"}, IngressProxyOptions{}); err == nil {
		t.Fatal("missing socket must be rejected")
	}
}
