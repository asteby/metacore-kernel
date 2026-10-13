package hub

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type fakeRevHub struct {
	mu       sync.Mutex
	priv     ed25519.PrivateKey
	body     []byte
	etag     string
	status   int
	hits     int
	notMod   int
	lastINM  string
	sortKeys bool
}

func signedBody(t *testing.T, priv ed25519.PrivateKey, generatedAt string, revoked []Revocation, sorted bool) []byte {
	t.Helper()
	l := RevocationList{GeneratedAt: generatedAt, Revoked: revoked}
	cands := signedCandidates(l)
	msg := cands[0]
	if sorted {
		msg = cands[2]
	}
	sig := ed25519.Sign(priv, msg)
	b, err := json.Marshal(revocationWire{GeneratedAt: generatedAt, Revoked: revoked, Signature: base64.StdEncoding.EncodeToString(sig)})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (f *fakeRevHub) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits++
	f.lastINM = r.Header.Get("If-None-Match")
	if f.status != 0 && f.status != 200 {
		http.Error(w, "boom", f.status)
		return
	}
	if f.etag != "" && f.lastINM == f.etag {
		f.notMod++
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("ETag", f.etag)
	w.Header().Set("Content-Type", "application/json")
	w.Write(f.body)
}

func newRevFixture(t *testing.T) (*fakeRevHub, *RevocationChecker, ed25519.PublicKey, *time.Time) {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	f := &fakeRevHub{priv: priv, etag: `"v1"`}
	f.body = signedBody(t, priv, "2026-10-01T00:00:00Z", []Revocation{
		{AddonKey: "pos", Version: "1.2.0", Reason: "cve", RevokedAt: "2026-10-01T00:00:00Z"},
		{AddonKey: "evil", Version: "*", Reason: "malware", RevokedAt: "2026-10-01T00:00:00Z"},
	}, false)
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	rc := c.NewRevocationChecker(func() []ed25519.PublicKey { return []ed25519.PublicKey{pub} }, WithRevocationTTL(time.Minute))
	rc.now = func() time.Time { return now }
	return f, rc, pub, &now
}

func TestRevocations_FetchVerifyLookup(t *testing.T) {
	_, rc, _, _ := newRevFixture(t)
	l, err := rc.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := l.Lookup("pos", "1.2.0"); !ok {
		t.Error("pos@1.2.0 must be revoked")
	}
	if _, ok := l.Lookup("pos", "v1.2.0"); !ok {
		t.Error("leading v must match")
	}
	if _, ok := l.Lookup("pos", "1.2.1"); ok {
		t.Error("pos@1.2.1 must not be revoked")
	}
	if r, ok := l.Lookup("evil", "9.9.9"); !ok || r.Reason != "malware" {
		t.Error("wildcard must revoke every version")
	}
}

func TestRevocations_SortedKeyCanonicalAccepted(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	body := signedBody(t, priv, "2026-10-01T00:00:00Z", []Revocation{{AddonKey: "a", Version: "1", Reason: "x<y&z", RevokedAt: "t"}}, true)
	l, err := VerifyRevocationList(body, []ed25519.PublicKey{pub})
	if err != nil || len(l.Revoked) != 1 {
		t.Fatalf("sorted canonical form must verify: %v", err)
	}
}

func TestRevocations_RejectsTamperedWrongKeyAndNoKeys(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	body := signedBody(t, priv, "2026-10-01T00:00:00Z", []Revocation{{AddonKey: "a", Version: "1"}}, false)
	var w revocationWire
	json.Unmarshal(body, &w)
	w.Revoked = nil // attacker strips a revocation
	tampered, _ := json.Marshal(w)

	if _, err := VerifyRevocationList(tampered, []ed25519.PublicKey{pub}); !errors.Is(err, ErrRevocationSignature) {
		t.Errorf("tampered: got %v", err)
	}
	if _, err := VerifyRevocationList(body, []ed25519.PublicKey{otherPub}); !errors.Is(err, ErrRevocationSignature) {
		t.Errorf("wrong key: got %v", err)
	}
	if _, err := VerifyRevocationList(body, nil); !errors.Is(err, ErrRevocationSignature) {
		t.Errorf("no keys must fail closed: got %v", err)
	}
	w.Signature = ""
	unsigned, _ := json.Marshal(w)
	if _, err := VerifyRevocationList(unsigned, []ed25519.PublicKey{pub}); !errors.Is(err, ErrRevocationSignature) {
		t.Errorf("unsigned: got %v", err)
	}
}

func TestRevocations_TTLAndETag(t *testing.T) {
	f, rc, _, now := newRevFixture(t)
	if _, err := rc.List(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Within TTL: served from cache, no request.
	*now = now.Add(30 * time.Second)
	rc.List(t.Context())
	if f.hits != 1 {
		t.Fatalf("hits within TTL = %d, want 1", f.hits)
	}
	// After TTL: conditional GET, 304 keeps the list.
	*now = now.Add(2 * time.Minute)
	l, err := rc.List(t.Context())
	if err != nil || len(l.Revoked) != 2 {
		t.Fatalf("after 304: %v %v", l, err)
	}
	if f.hits != 2 || f.notMod != 1 || f.lastINM != `"v1"` {
		t.Fatalf("want conditional GET answered 304, hits=%d notMod=%d inm=%q", f.hits, f.notMod, f.lastINM)
	}
}

func TestRevocations_HubDownKeepsLastValidList(t *testing.T) {
	f, rc, _, now := newRevFixture(t)
	if _, err := rc.List(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.status = 503
	f.mu.Unlock()
	*now = now.Add(10 * time.Minute)
	l, err := rc.List(t.Context())
	if err != nil {
		t.Fatalf("hub down with cache must not error: %v", err)
	}
	if _, ok := l.Lookup("pos", "1.2.0"); !ok {
		t.Fatal("known revocation forgotten while hub was down (fail-open)")
	}
	// Backoff: an immediate second call does not hit the hub again.
	hits := f.hits
	rc.List(t.Context())
	if f.hits != hits {
		t.Errorf("down hub hammered: %d -> %d", hits, f.hits)
	}
}

func TestRevocations_NoCacheAndHubDownIsUnavailable(t *testing.T) {
	f, rc, _, _ := newRevFixture(t)
	f.status = 500
	if _, err := rc.List(t.Context()); !errors.Is(err, ErrRevocationsUnavailable) {
		t.Fatalf("want ErrRevocationsUnavailable, got %v", err)
	}
}

func TestRevocations_BadSignatureAndReplayKeepCache(t *testing.T) {
	f, rc, _, now := newRevFixture(t)
	if _, err := rc.List(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Forged list without revocations, signed by an attacker key.
	_, evilPriv, _ := ed25519.GenerateKey(rand.Reader)
	f.mu.Lock()
	f.body = signedBody(t, evilPriv, "2026-10-02T00:00:00Z", nil, false)
	f.etag = `"v2"`
	f.mu.Unlock()
	*now = now.Add(2 * time.Minute)
	l, err := rc.Refresh(t.Context())
	if !errors.Is(err, ErrRevocationSignature) {
		t.Fatalf("forged list must be rejected, got %v", err)
	}
	if _, ok := l.Lookup("pos", "1.2.0"); !ok {
		t.Fatal("forged list dropped a revocation")
	}
	// Replay of an older but genuinely signed (empty) list.
	f.mu.Lock()
	f.body = signedBody(t, f.priv, "2026-09-01T00:00:00Z", nil, false)
	f.etag = `"v3"`
	f.mu.Unlock()
	l, err = rc.Refresh(t.Context())
	if err == nil {
		t.Fatal("older list must be rejected as replay")
	}
	if _, ok := l.Lookup("pos", "1.2.0"); !ok {
		t.Fatal("replayed list dropped a revocation")
	}
	// A newer valid list is accepted.
	f.mu.Lock()
	f.body = signedBody(t, f.priv, "2026-10-03T00:00:00Z", nil, false)
	f.etag = `"v4"`
	f.mu.Unlock()
	l, err = rc.Refresh(t.Context())
	if err != nil || len(l.Revoked) != 0 {
		t.Fatalf("newer list must replace cache: %v %v", l, err)
	}
}

func TestRevocations_DiskCacheSurvivesRestartWhileHubDown(t *testing.T) {
	f, rc, pub, _ := newRevFixture(t)
	path := filepath.Join(t.TempDir(), "rev.json")
	rc.cachePath = path
	if _, err := rc.List(t.Context()); err != nil {
		t.Fatal(err)
	}
	// "Restart": new checker, hub down, same trusted key.
	f.mu.Lock()
	f.status = 502
	f.mu.Unlock()
	c2, _ := NewClient(rc.client.baseURL, "")
	rc2 := c2.NewRevocationChecker(func() []ed25519.PublicKey { return []ed25519.PublicKey{pub} }, WithRevocationCacheFile(path))
	l, err := rc2.List(t.Context())
	if err != nil {
		t.Fatalf("disk cache must back the checker: %v", err)
	}
	if _, ok := l.Lookup("evil", "1"); !ok {
		t.Fatal("disk-cached revocation missing")
	}
	// A disk file signed by an untrusted key is ignored.
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	rc3 := c2.NewRevocationChecker(func() []ed25519.PublicKey { return []ed25519.PublicKey{otherPub} }, WithRevocationCacheFile(path))
	if _, err := rc3.List(t.Context()); !errors.Is(err, ErrRevocationsUnavailable) {
		t.Fatalf("untrusted disk cache must be rejected, got %v", err)
	}
}
