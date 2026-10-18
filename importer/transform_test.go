package importer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/modelbase"
	"github.com/asteby/metacore-kernel/netguard"
)

// Minimal real magic bytes: the engine sniffs the type instead of trusting the
// server's Content-Type.
var (
	jpegBytes = append([]byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00}, []byte("fake-jpeg-bytes")...)
	pngBytes  = append([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1A, '\n'}, []byte("png")...)
)

type memStore struct {
	files map[string][]byte
	ctype map[string]string
}

func (m *memStore) Put(_ context.Context, suggestedName string, r io.Reader, contentType string) (string, error) {
	if m.files == nil {
		m.files = map[string][]byte{}
		m.ctype = map[string]string{}
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	if len(data) > defaultMediaMaxBytes { // defensive: the engine already caps it
		return "", io.ErrUnexpectedEOF
	}
	name := suggestedName
	if name == "" {
		name = "blob"
	}
	m.files[name] = data
	m.ctype[name] = contentType
	return name, nil
}

func TestMediaURLTransformFetchesAndStores(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(jpegBytes)
	}))
	defer srv.Close()

	store := &memStore{}
	deps := &TransformDeps{HTTPClient: srv.Client(), Store: store}

	spec := modelbase.ImportSpec{
		Columns: []modelbase.ImportColumn{
			{Key: "user.name", Header: "Nombre", Required: true},
			{Key: "user.avatar", Header: "URL Foto", Transform: "media_url"},
		},
	}
	record, issues := BuildRecordWithDeps(spec, map[string]any{
		"Nombre":   "Dra. Ana",
		"URL Foto": srv.URL + "/avatar.jpg",
	}, deps, 0)
	if len(issues) > 0 {
		t.Fatalf("issues: %+v", issues)
	}
	user := record["user"].(map[string]any)
	if user["avatar"] != "avatar.jpg" {
		t.Fatalf("avatar: got %#v", user["avatar"])
	}
	if !bytes.Equal(store.files["avatar.jpg"], jpegBytes) {
		t.Fatalf("store content mismatch: %q", store.files["avatar.jpg"])
	}
}

func TestMediaURLListTransformJoinsFilenames(t *testing.T) {
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngBytes)
	}))
	defer srv.Close()

	store := &memStore{}
	deps := &TransformDeps{HTTPClient: srv.Client(), Store: store}
	spec := modelbase.ImportSpec{
		Columns: []modelbase.ImportColumn{
			{Key: "name", Header: "Nombre", Required: true},
			{Key: "_gallery", Header: "URL Galería", Transform: "media_url_list"},
		},
	}
	record, issues := BuildRecordWithDeps(spec, map[string]any{
		"Nombre":      "X",
		"URL Galería": srv.URL + "/a.png | " + srv.URL + "/b.png",
	}, deps, 0)
	if len(issues) > 0 {
		t.Fatalf("issues: %+v", issues)
	}
	got, _ := record["_gallery"].(string)
	if got != "a.png|b.png" {
		t.Fatalf("gallery list: %q", got)
	}
	if n != 2 {
		t.Fatalf("expected 2 fetches, got %d", n)
	}
}

func TestMediaURLTransformRequiresStore(t *testing.T) {
	spec := modelbase.ImportSpec{
		Columns: []modelbase.ImportColumn{
			{Key: "user.name", Header: "Nombre", Required: true},
			{Key: "user.avatar", Header: "URL Foto", Transform: "media_url"},
		},
	}
	_, issues := BuildRecordWithDeps(spec, map[string]any{
		"Nombre":   "X",
		"URL Foto": "https://example.com/a.jpg",
	}, nil, 0)
	if len(issues) != 1 {
		t.Fatalf("want 1 issue, got %+v", issues)
	}
	if !strings.Contains(issues[0].Message, "TransformDeps.Store") {
		t.Fatalf("message: %s", issues[0].Message)
	}
}

func TestUnknownTransformFailsRow(t *testing.T) {
	spec := modelbase.ImportSpec{
		Columns: []modelbase.ImportColumn{
			{Key: "name", Header: "Nombre", Required: true, Transform: "no_such_xf"},
		},
	}
	_, issues := BuildRecord(spec, map[string]any{"Nombre": "X"})
	if len(issues) != 1 || !strings.Contains(issues[0].Message, "no registrado") {
		t.Fatalf("issues: %+v", issues)
	}
}

// ---- SPEC-B: política de medios endurecida -------------------------------

// fakeSleep records the requested waits instead of sleeping.
type fakeSleep struct{ waits []time.Duration }

func (f *fakeSleep) sleep(_ context.Context, d time.Duration) error {
	f.waits = append(f.waits, d)
	return nil
}

// hashedStore is an in-memory HashedMediaStore that counts writes.
type hashedStore struct {
	memStore
	byHash  map[string]string
	puts    int
	lookups int
}

func (h *hashedStore) Lookup(_ context.Context, sum string) (string, bool, error) {
	h.lookups++
	name, ok := h.byHash[sum]
	return name, ok, nil
}

func (h *hashedStore) PutHashed(ctx context.Context, sum, suggested string, r io.Reader, ct string) (string, error) {
	h.puts++
	if h.byHash == nil {
		h.byHash = map[string]string{}
	}
	name, err := h.Put(ctx, sum[:8]+"-"+suggested, r, ct)
	if err != nil {
		return "", err
	}
	h.byHash[sum] = name
	return name, nil
}

// allowLocal is an SSRF-guarded client that also lets tests reach httptest.
func allowLocal() *http.Client {
	return netguard.NewClient(netguard.Options{AllowPrivate: true})
}

func imageServer(hits *int32, ctype string, body []byte) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		w.Header().Set("Content-Type", ctype)
		_, _ = w.Write(body)
	}))
}

func TestMediaURLRetriesOn503ThenSucceeds(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write(pngBytes)
	}))
	defer srv.Close()

	fs := &fakeSleep{}
	deps := &TransformDeps{HTTPClient: allowLocal(), Store: &memStore{}, Sleep: fs.sleep}
	name, err := fetchAndStore(deps, srv.URL+"/a.png")
	if err != nil || name != "a.png" {
		t.Fatalf("name=%q err=%v", name, err)
	}
	if hits != 3 || len(fs.waits) != 2 {
		t.Fatalf("hits=%d waits=%v, want 3 attempts and 2 waits", hits, fs.waits)
	}
	within := func(got, want time.Duration) bool {
		return got >= time.Duration(float64(want)*0.8) && got <= time.Duration(float64(want)*1.2)
	}
	if !within(fs.waits[0], 500*time.Millisecond) || !within(fs.waits[1], time.Second) {
		t.Fatalf("waits %v not ~500ms and ~1s (+-20%%)", fs.waits)
	}
}

func TestMediaURLRespectsRetryAfter(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write(pngBytes)
	}))
	defer srv.Close()

	fs := &fakeSleep{}
	deps := &TransformDeps{HTTPClient: allowLocal(), Store: &memStore{}, Sleep: fs.sleep}
	if _, err := fetchAndStore(deps, srv.URL+"/a.png"); err != nil {
		t.Fatal(err)
	}
	if len(fs.waits) != 1 || fs.waits[0] != 2*time.Second {
		t.Fatalf("waits=%v, want [2s]", fs.waits)
	}
	// A huge Retry-After is capped at 30s.
	if got := parseRetryAfter("86400"); got != 30*time.Second {
		t.Fatalf("cap: got %v", got)
	}
}

func TestMediaURLNoRetryOn404(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.NotFound(w, r)
	}))
	defer srv.Close()

	fs := &fakeSleep{}
	deps := &TransformDeps{HTTPClient: allowLocal(), Store: &memStore{}, Sleep: fs.sleep}
	_, err := fetchAndStore(deps, srv.URL+"/x.png")
	var he ErrMediaHTTP
	if !errors.As(err, &he) || he.Status != 404 {
		t.Fatalf("err=%v, want ErrMediaHTTP{404}", err)
	}
	if hits != 1 || len(fs.waits) != 0 {
		t.Fatalf("hits=%d waits=%v, want a single attempt", hits, fs.waits)
	}
}

func TestMediaURLBlocksLoopbackAndPrivate(t *testing.T) {
	var hits int32
	local := imageServer(&hits, "image/png", pngBytes)
	defer local.Close()

	fs := &fakeSleep{}
	// HTTPClient nil => default netguard client.
	deps := &TransformDeps{Store: &memStore{}, Sleep: fs.sleep}
	for _, u := range []string{
		local.URL + "/a.png", // real 127.0.0.1 listener: must never be reached
		"http://127.0.0.1:1/a.png",
		"http://127.0.0.2:1/a.png",
		"http://[::ffff:127.0.0.1]:1/a.png",
		"http://169.254.169.254/latest/meta-data",
		"http://10.0.0.1/a.png",
	} {
		_, err := fetchAndStore(deps, u)
		if !errors.Is(err, netguard.ErrBlockedEgress) {
			t.Errorf("%s: err=%v, want ErrBlockedEgress", u, err)
		}
	}
	if hits != 0 {
		t.Fatalf("loopback server reached %d times", hits)
	}
	if len(fs.waits) != 0 {
		t.Fatalf("blocked destinations must not be retried, waits=%v", fs.waits)
	}
}

func TestMediaURLBlocksRedirectToPrivate(t *testing.T) {
	var privateHits int32
	private := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&privateHits, 1)
		_, _ = w.Write(pngBytes)
	}))
	defer private.Close()
	public := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, private.URL+"/secret.png", http.StatusFound)
	}))
	defer public.Close()

	// The public listener is pretended to be a public IP; everything else
	// goes through the real rule. Same shape as netguard.NewClient.
	publicAddr := public.Listener.Addr().String()
	client := &http.Client{Transport: &http.Transport{
		Proxy: nil,
		DialContext: (&net.Dialer{Control: func(_, address string, _ syscall.RawConn) error {
			if address == publicAddr {
				return nil
			}
			host, _, _ := net.SplitHostPort(address)
			if netguard.IsBlockedIP(net.ParseIP(host)) {
				return netguard.ErrBlockedEgress
			}
			return nil
		}}).DialContext,
	}}
	fs := &fakeSleep{}
	deps := &TransformDeps{HTTPClient: client, Store: &memStore{}, Sleep: fs.sleep}
	_, err := fetchAndStore(deps, public.URL+"/a.png")
	if !errors.Is(err, netguard.ErrBlockedEgress) {
		t.Fatalf("err=%v, want ErrBlockedEgress", err)
	}
	if privateHits != 0 || len(fs.waits) != 0 {
		t.Fatalf("privateHits=%d waits=%v", privateHits, fs.waits)
	}
}

func TestMediaURLAllowHosts(t *testing.T) {
	var hits int32
	srv := imageServer(&hits, "image/png", pngBytes)
	defer srv.Close()
	// Every connection goes to the test server whatever the hostname is.
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
		},
	}}
	deps := &TransformDeps{
		HTTPClient: client,
		Store:      &memStore{},
		Policy:     MediaPolicy{AllowHosts: []string{"*.godsnutrition.com.mx"}},
	}
	if _, err := fetchAndStore(deps, "http://cdn.godsnutrition.com.mx/a.png"); err != nil {
		t.Fatalf("allowed host rejected: %v", err)
	}
	before := hits
	if _, err := fetchAndStore(deps, "http://evil.com/a.png"); !errors.Is(err, ErrMediaHost) {
		t.Fatalf("err=%v, want ErrMediaHost", err)
	}
	if hits != before {
		t.Fatal("a disallowed host must not be requested")
	}
	// Patterns that would open everything are ignored, so nothing matches.
	for _, bad := range []string{"*", "*.com"} {
		deps.Policy.AllowHosts = []string{bad}
		if _, err := fetchAndStore(deps, "http://cdn.godsnutrition.com.mx/a.png"); !errors.Is(err, ErrMediaHost) {
			t.Fatalf("pattern %q: err=%v, want ErrMediaHost", bad, err)
		}
	}
}

func TestMediaURLRejectsOversizeWithoutTruncating(t *testing.T) {
	body := append(append([]byte{}, pngBytes...), bytes.Repeat([]byte{0}, 200)...)
	pol := MediaPolicy{MaxBytes: int64(len(body)) - 1}

	// Chunked response (no Content-Length): detected while reading.
	chunked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.(http.Flusher).Flush()
		_, _ = w.Write(body)
	}))
	defer chunked.Close()
	// Declared Content-Length: aborted without reading.
	declared := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	}))
	defer declared.Close()

	for name, u := range map[string]string{"chunked": chunked.URL, "content-length": declared.URL} {
		store := &memStore{}
		deps := &TransformDeps{HTTPClient: allowLocal(), Store: store, Policy: pol, Sleep: (&fakeSleep{}).sleep}
		_, err := fetchAndStore(deps, u+"/big.png")
		if !errors.Is(err, ErrMediaTooLarge) {
			t.Fatalf("%s: err=%v, want ErrMediaTooLarge", name, err)
		}
		if len(store.files) != 0 {
			t.Fatalf("%s: store received %d files, want none", name, len(store.files))
		}
	}
	// Exactly MaxBytes is fine.
	deps := &TransformDeps{HTTPClient: allowLocal(), Store: &memStore{}, Policy: MediaPolicy{MaxBytes: int64(len(body))}}
	if _, err := fetchAndStore(deps, declared.URL+"/ok.png"); err != nil {
		t.Fatalf("file of exactly MaxBytes rejected: %v", err)
	}
}

func TestMediaURLMagicBytesOverrideContentType(t *testing.T) {
	var hits int32
	cases := []struct {
		name  string
		ctype string
		body  []byte
		okAs  string // expected stored content type, "" => ErrMediaType
	}{
		{"png-as-text", "text/plain", pngBytes, "image/png"},
		{"html-as-jpeg", "image/jpeg", []byte("<!DOCTYPE html><html><script>alert(1)</script></html>"), ""},
		{"svg", "image/svg+xml", []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"><script/></svg>`), ""},
		{"bare-svg", "image/png", []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`), ""},
		{"webp", "application/octet-stream", append([]byte("RIFF\x10\x00\x00\x00WEBPVP8 "), 0, 0, 0, 0), "image/webp"},
		{"avif", "application/octet-stream", []byte("\x00\x00\x00\x18ftypavif\x00\x00\x00\x00avifmif1"), "image/avif"},
	}
	for _, tc := range cases {
		srv := imageServer(&hits, tc.ctype, tc.body)
		store := &memStore{}
		deps := &TransformDeps{HTTPClient: allowLocal(), Store: store}
		name, err := fetchAndStore(deps, srv.URL+"/pic.jpg")
		srv.Close()
		if tc.okAs == "" {
			if !errors.Is(err, ErrMediaType) {
				t.Errorf("%s: err=%v, want ErrMediaType", tc.name, err)
			}
			if len(store.files) != 0 {
				t.Errorf("%s: a disallowed type was stored", tc.name)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if store.ctype[name] != tc.okAs {
			t.Errorf("%s: stored as %q, want %q", tc.name, store.ctype[name], tc.okAs)
		}
		// The extension comes from the detected type, not from the URL.
		wantExt := map[string]string{"image/png": ".png", "image/webp": ".webp", "image/avif": ".avif"}[tc.okAs]
		if !strings.HasSuffix(name, wantExt) {
			t.Errorf("%s: name %q, want extension %s", tc.name, name, wantExt)
		}
	}
}

func TestMediaURLDedupeByHash(t *testing.T) {
	var hits int32
	srv := imageServer(&hits, "image/png", pngBytes)
	defer srv.Close()

	store := &hashedStore{}
	deps := &TransformDeps{HTTPClient: allowLocal(), Store: store}
	n1, err := fetchAndStore(deps, srv.URL+"/one.png")
	if err != nil {
		t.Fatal(err)
	}
	n2, err := fetchAndStore(deps, srv.URL+"/two.png")
	if err != nil {
		t.Fatal(err)
	}
	if store.puts != 1 || n1 != n2 {
		t.Fatalf("puts=%d n1=%q n2=%q, want one PutHashed and the same name", store.puts, n1, n2)
	}
}

func TestMediaURLListDedupesSameURL(t *testing.T) {
	var hits int32
	srv := imageServer(&hits, "image/png", pngBytes)
	defer srv.Close()

	deps := &TransformDeps{HTTPClient: allowLocal(), Store: &memStore{}}
	got, err := transformMediaURLList(TransformContext{Deps: deps}, srv.URL+"/a.png|"+srv.URL+"/a.png|"+srv.URL+"/b.png")
	if err != nil {
		t.Fatal(err)
	}
	if got != "a.png|b.png" || hits != 2 {
		t.Fatalf("got %q with %d downloads, want a.png|b.png with 2", got, hits)
	}
}

func TestMediaURLContextCancelStopsRetries(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deps := &TransformDeps{
		HTTPClient: allowLocal(),
		Store:      &memStore{},
		Context:    ctx,
		Sleep: func(ctx context.Context, _ time.Duration) error {
			cancel() // the request ends while waiting for the backoff
			return ctx.Err()
		},
	}
	_, err := fetchAndStore(deps, srv.URL+"/a.png")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
	if hits != 1 {
		t.Fatalf("hits=%d, want 1 (no attempts after cancel)", hits)
	}
}

func TestFetchMedia_ReturnsValidatedBytesWithoutTouchingTheStore(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(jpegBytes)
	}))
	defer srv.Close()
	f, err := FetchMedia(&TransformDeps{HTTPClient: srv.Client()}, srv.URL+"/foto.png")
	if err != nil {
		t.Fatal(err)
	}
	if f.Type != "image/jpeg" || f.SuggestedName != "foto.jpg" || !bytes.Equal(f.Data, jpegBytes) {
		t.Fatalf("got %+v", f)
	}
}

func TestFetchMedia_RejectsNonImageAndBadScheme(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("<html>")) }))
	defer srv.Close()
	if _, err := FetchMedia(&TransformDeps{HTTPClient: srv.Client()}, srv.URL); !errors.Is(err, ErrMediaType) {
		t.Fatalf("got %v", err)
	}
	if _, err := FetchMedia(nil, "ftp://x/y.jpg"); err == nil {
		t.Fatal("esquema inválido aceptado")
	}
}
