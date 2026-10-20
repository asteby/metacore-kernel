package licensing

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeHub answers POST /v1/licenses/renew with a scripted response.
type fakeHub struct {
	mu     sync.Mutex
	status int
	body   string
	token  string
	hits   int
}

func (h *fakeHub) set(status int, body, token string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.status, h.body, h.token = status, body, token
}

func (h *fakeHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hits++
	w.Header().Set("Content-Type", "application/json")
	if h.status != http.StatusOK {
		w.WriteHeader(h.status)
		_, _ = w.Write([]byte(h.body))
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"license_token": h.token})
}

type revokeHarness struct {
	svc   *Service
	hub   *fakeHub
	store *MemoryStore
	clock *time.Time
	priv  ed25519.PrivateKey
	pub   string
	srv   *httptest.Server
}

func (h *revokeHarness) newService(t *testing.T, enforce, revokeEnforce bool, st *MemoryStore) *Service {
	t.Helper()
	_, ipriv, _ := ed25519.GenerateKey(nil)
	return New(Config{
		Enforce:       enforce,
		RevokeEnforce: revokeEnforce,
		HubBaseURL:    h.srv.URL,
		PubKeyHex:     h.pub,
		InstanceID:    uuid.New(),
		InstancePriv:  ipriv,
		Store:         st,
		Now:           func() time.Time { return *h.clock },
	})
}

func newRevokeHarness(t *testing.T, enforce, revokeEnforce bool) *revokeHarness {
	t.Helper()
	priv, pub := testKeypair(t)
	now := time.Now().UTC()
	h := &revokeHarness{hub: &fakeHub{}, store: NewMemoryStore(), clock: &now, priv: priv, pub: pub}
	h.srv = httptest.NewServer(h.hub)
	t.Cleanup(h.srv.Close)
	// A leased token (so it renews every cycle) that is valid for a year.
	c := baseClaims()
	c.MaxOfflineHours = 24 * 14
	c.IssuedAt = now
	exp := c.ExpiresAt
	if err := h.store.Save(context.Background(), &Record{Token: mintToken(t, priv, c), ExpiresAt: &exp}); err != nil {
		t.Fatal(err)
	}
	h.svc = h.newService(t, enforce, revokeEnforce, h.store)
	return h
}

const revokedBody = `{"error":"license revoked; renewal refused","code":"license_revoked"}`

// A revocation puts the instance read-only after the grace (72h default), with
// a visible warning window before.
func TestRevoke_ReadOnlyAfterGrace(t *testing.T) {
	h := newRevokeHarness(t, true, true)
	ctx := context.Background()
	h.hub.set(http.StatusForbidden, revokedBody, "")

	h.svc.RefreshNow(ctx)
	st := h.svc.Current()
	if !st.Revoked || st.RevokedAt == nil || st.RevokeGraceUntil == nil {
		t.Fatalf("revocation not surfaced: %+v", st)
	}
	if st.WritesBlocked() || st.Status != "valid" {
		t.Fatalf("inside the grace the instance must keep operating: %+v", st)
	}
	if st.Reason == "" {
		t.Fatalf("warning reason missing inside grace")
	}

	*h.clock = h.clock.Add(71 * time.Hour)
	h.svc.RefreshNow(ctx)
	if h.svc.Current().WritesBlocked() {
		t.Fatalf("71h in: still inside the 72h grace")
	}

	*h.clock = h.clock.Add(2 * time.Hour) // 73h after the first 403
	h.svc.RefreshNow(ctx)
	st = h.svc.Current()
	if st.Status != "revoked" || st.Valid || !st.WritesBlocked() || !st.InstallBlocked() {
		t.Fatalf("after the grace the instance must be read-only: %+v", st)
	}
}

// Default behaviour is unchanged: without the revoke flag a 403 is recorded
// and shown but never blocks, however long ago it happened.
func TestRevoke_FlagOffNeverBlocks(t *testing.T) {
	h := newRevokeHarness(t, true, false)
	ctx := context.Background()
	h.hub.set(http.StatusForbidden, revokedBody, "")
	h.svc.RefreshNow(ctx)
	*h.clock = h.clock.Add(30 * 24 * time.Hour)
	h.svc.RefreshNow(ctx)
	st := h.svc.Current()
	// (30 days without a successful check-in makes the lease "stale", which is
	// the pre-existing, still-operable posture; what matters is it never turns
	// "revoked" nor blocks.)
	if st.WritesBlocked() || st.Status == "revoked" || !st.Valid {
		t.Fatalf("flag off must not change the posture: %+v", st)
	}
	if !st.Revoked {
		t.Fatalf("revocation should still be visible")
	}
}

// Enforcement itself off (the default for every existing instance): nothing is
// blocked even with the revoke flag on.
func TestRevoke_EnforceOffNeverBlocks(t *testing.T) {
	h := newRevokeHarness(t, false, true)
	h.hub.set(http.StatusForbidden, revokedBody, "")
	h.svc.RefreshNow(context.Background())
	*h.clock = h.clock.Add(10 * 24 * time.Hour)
	h.svc.RefreshNow(context.Background())
	if h.svc.Current().WritesBlocked() {
		t.Fatalf("LICENSING_ENFORCE off must never block")
	}
}

// Only a revocation starts the clock: other 403s (instance not paired, id
// mismatch) and network-ish errors do not.
func TestRevoke_Other403DoesNotRevoke(t *testing.T) {
	h := newRevokeHarness(t, true, true)
	h.hub.set(http.StatusForbidden, `{"error":"instance not paired"}`, "")
	h.svc.RefreshNow(context.Background())
	*h.clock = h.clock.Add(100 * time.Hour)
	h.hub.set(http.StatusInternalServerError, `boom`, "")
	h.svc.RefreshNow(context.Background())
	st := h.svc.Current()
	if st.Revoked || st.WritesBlocked() {
		t.Fatalf("non-revocation errors must not revoke: %+v", st)
	}
}

// Older hubs have no "code": the error text is enough.
func TestRevoke_LegacyHubBodyWithoutCode(t *testing.T) {
	h := newRevokeHarness(t, true, true)
	h.hub.set(http.StatusForbidden, `{"error":"license revoked; renewal refused"}`, "")
	h.svc.RefreshNow(context.Background())
	if !h.svc.Current().Revoked {
		t.Fatalf("legacy hub revocation not detected")
	}
}

// The grace clock is anchored on the FIRST 403: repeated 403s and a restart do
// not reset it.
func TestRevoke_GraceAnchoredAndSurvivesRestart(t *testing.T) {
	h := newRevokeHarness(t, true, true)
	ctx := context.Background()
	h.hub.set(http.StatusForbidden, revokedBody, "")
	h.svc.RefreshNow(ctx)
	first := *h.svc.Current().RevokedAt

	*h.clock = h.clock.Add(50 * time.Hour)
	h.svc.RefreshNow(ctx)
	if !h.svc.Current().RevokedAt.Equal(first) {
		t.Fatalf("repeated 403 moved the anchor")
	}

	// "Restart" at +73h: a new Service over the same store must be read-only.
	*h.clock = first.Add(73 * time.Hour)
	svc2 := h.newService(t, true, true, h.store)
	svc2.Boot(ctx)
	if st := svc2.Current(); !st.WritesBlocked() || st.Status != "revoked" {
		t.Fatalf("restart reset the revoke grace: %+v", st)
	}
}

// If the issuer reinstates the license, the next successful renew clears it.
func TestRevoke_SuccessfulRenewClears(t *testing.T) {
	h := newRevokeHarness(t, true, true)
	ctx := context.Background()
	h.hub.set(http.StatusForbidden, revokedBody, "")
	h.svc.RefreshNow(ctx)
	*h.clock = h.clock.Add(80 * time.Hour)
	h.svc.RefreshNow(ctx)
	if !h.svc.Current().WritesBlocked() {
		t.Fatalf("precondition: read-only")
	}

	c := baseClaims()
	c.MaxOfflineHours = 24 * 14
	c.IssuedAt = *h.clock
	h.hub.set(http.StatusOK, "", mintToken(t, h.priv, c))
	h.svc.RefreshNow(ctx)
	st := h.svc.Current()
	if st.Revoked || st.WritesBlocked() || st.Status != "valid" {
		t.Fatalf("reinstated license must recover: %+v", st)
	}
}

// With the policy on, a non-leased long token is still checked every cycle so
// the revocation is noticed within one recheck.
func TestRevoke_PolicyChecksInEveryCycle(t *testing.T) {
	priv, pub := testKeypair(t)
	now := time.Now().UTC()
	hub := &fakeHub{}
	hub.set(http.StatusForbidden, revokedBody, "")
	srv := httptest.NewServer(hub)
	defer srv.Close()
	st := NewMemoryStore()
	bc := baseClaims() // 1y, no lease
	bexp := bc.ExpiresAt
	_ = st.Save(context.Background(), &Record{Token: mintToken(t, priv, bc), ExpiresAt: &bexp})
	_, ipriv, _ := ed25519.GenerateKey(nil)
	mk := func(revokeEnforce bool) *Service {
		return New(Config{Enforce: true, RevokeEnforce: revokeEnforce, HubBaseURL: srv.URL, PubKeyHex: pub,
			InstanceID: uuid.New(), InstancePriv: ipriv, Store: st, Now: func() time.Time { return now }})
	}
	off := mk(false)
	off.RefreshNow(context.Background())
	if hub.hits != 0 {
		t.Fatalf("flag off keeps the old cadence (no renew for a far-from-expiry token), hits=%d", hub.hits)
	}
	on := mk(true)
	on.RefreshNow(context.Background())
	if hub.hits != 1 || !on.Current().Revoked {
		t.Fatalf("flag on must check in every cycle: hits=%d state=%+v", hub.hits, on.Current())
	}
}

func TestRevoke_EnvConfig(t *testing.T) {
	t.Setenv("LICENSING_REVOKE_ENFORCE", "1")
	t.Setenv("LICENSING_REVOKE_GRACE_HOURS", "24")
	cfg := NewConfigFromEnv()
	if !cfg.RevokeEnforce || cfg.RevokeGrace != 24*time.Hour {
		t.Fatalf("env not parsed: %+v", cfg)
	}
	if got := New(Config{}).revokeGrace; got != DefaultRevokeGrace {
		t.Fatalf("default grace = %v want 72h", got)
	}
	if New(Config{}).revokeEnforce {
		t.Fatalf("revoke policy must default to off")
	}
}

// brokenStore models an embedder whose store predates Record.RevokedAt: it
// drops the field on Save. The anchor must still hold across refresh cycles.
type brokenStore struct{ *MemoryStore }

func (b brokenStore) Save(ctx context.Context, rec *Record) error {
	cp := *rec
	cp.RevokedAt = nil
	return b.MemoryStore.Save(ctx, &cp)
}

func TestRevoke_StoreWithoutFieldKeepsAnchorInProcess(t *testing.T) {
	h := newRevokeHarness(t, true, true)
	svc := h.newService(t, true, true, nil)
	svc.store = brokenStore{h.store}
	ctx := context.Background()
	h.hub.set(http.StatusForbidden, revokedBody, "")
	svc.RefreshNow(ctx)
	for i := 0; i < 5; i++ { // 5 cycles of 20h: the grace (72h) must elapse
		*h.clock = h.clock.Add(20 * time.Hour)
		svc.RefreshNow(ctx)
	}
	if st := svc.Current(); !st.WritesBlocked() || st.Status != "revoked" {
		t.Fatalf("anchor was reset every cycle by a store without the field: %+v", st)
	}
}
