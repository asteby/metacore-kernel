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

// Re-pasting the SAME (older) token must not restart the revoke grace: a
// revoked customer could otherwise paste every 70h and never turn read-only.
func TestRevoke_RepastingSameTokenKeepsAnchor(t *testing.T) {
	h := newRevokeHarness(t, true, true)
	ctx := context.Background()
	rec, _ := h.store.Load(ctx)
	old := rec.Token
	h.hub.set(http.StatusForbidden, revokedBody, "")
	h.svc.RefreshNow(ctx)
	first := *h.svc.Current().RevokedAt

	*h.clock = h.clock.Add(70 * time.Hour)
	if _, err := h.svc.Activate(ctx, old); err != nil {
		t.Fatal(err)
	}
	if st := h.svc.Current(); !st.Revoked || !st.RevokedAt.Equal(first) {
		t.Fatalf("repaste reset the revocation: %+v", st)
	}
	*h.clock = h.clock.Add(3 * time.Hour) // 73h after the first 403
	h.svc.RefreshNow(ctx)
	if st := h.svc.Current(); !st.WritesBlocked() || st.Status != "revoked" {
		t.Fatalf("repaste evaded the read-only: %+v", st)
	}
}

// A token issued AFTER the revocation proves the issuer reinstated it: pasting
// it lifts the revocation.
func TestRevoke_PastingTokenIssuedAfterRevocationLifts(t *testing.T) {
	h := newRevokeHarness(t, true, true)
	ctx := context.Background()
	h.hub.set(http.StatusForbidden, revokedBody, "")
	h.svc.RefreshNow(ctx)
	*h.clock = h.clock.Add(time.Hour)
	c := baseClaims()
	c.MaxOfflineHours = 24 * 14
	c.IssuedAt = *h.clock
	if _, err := h.svc.Activate(ctx, mintToken(t, h.priv, c)); err != nil {
		t.Fatal(err)
	}
	if st := h.svc.Current(); st.Revoked {
		t.Fatalf("newer token should lift: %+v", st)
	}
}

// anchorStore models a store whose Record cannot carry RevokedAt (ops GORM
// table) but implements the RevokeAnchorStore side channel.
type anchorStore struct {
	LicenseStore
	at *time.Time
}

func (a *anchorStore) Save(ctx context.Context, r *Record) error {
	cp := *r
	cp.RevokedAt = nil // the underlying table has no column
	return a.LicenseStore.Save(ctx, &cp)
}
func (a *anchorStore) LoadRevokeAnchor(context.Context) (*time.Time, error) { return a.at, nil }
func (a *anchorStore) SaveRevokeAnchor(_ context.Context, at *time.Time) error {
	a.at = at
	return nil
}

// A GORM-like store with the side channel keeps the grace across a restart
// (a NEW Service, empty process memory) and across a re-paste.
func TestRevoke_AnchorStoreSurvivesRestartAndRepaste(t *testing.T) {
	h := newRevokeHarness(t, true, true)
	ctx := context.Background()
	rec, _ := h.store.Load(ctx)
	old := rec.Token
	as := &anchorStore{LicenseStore: h.store}
	svc := h.newService(t, true, true, nil)
	svc.store = as
	h.hub.set(http.StatusForbidden, revokedBody, "")
	svc.RefreshNow(ctx)
	if as.at == nil {
		t.Fatalf("anchor not persisted through the side channel")
	}
	first := *as.at

	*h.clock = h.clock.Add(60 * time.Hour)
	svc2 := h.newService(t, true, true, nil) // restart: no in-memory anchor
	svc2.store = as
	svc2.Boot(ctx)
	if st := svc2.Current(); !st.Revoked || !st.RevokedAt.Equal(first) {
		t.Fatalf("restart lost the anchor: %+v", st)
	}
	if _, err := svc2.Activate(ctx, old); err != nil {
		t.Fatal(err)
	}
	*h.clock = h.clock.Add(14 * time.Hour) // 74h from the first 403
	svc2.RefreshNow(ctx)
	if st := svc2.Current(); st.Status != "revoked" || !st.WritesBlocked() {
		t.Fatalf("repaste after restart reset the grace: %+v", st)
	}
	// a successful renew clears the side channel too
	c := baseClaims()
	c.MaxOfflineHours = 24 * 14
	c.IssuedAt = *h.clock
	h.hub.set(http.StatusOK, "", mintToken(t, h.priv, c))
	svc2.RefreshNow(ctx)
	if as.at != nil || svc2.Current().Revoked {
		t.Fatalf("renew did not clear the anchor: %v", as.at)
	}
}

// flakyAnchorStore is an anchorStore whose side-channel read can fail.
type flakyAnchorStore struct {
	anchorStore
	failLoad bool
}

func (f *flakyAnchorStore) LoadRevokeAnchor(ctx context.Context) (*time.Time, error) {
	if f.failLoad {
		return nil, context.DeadlineExceeded
	}
	return f.anchorStore.LoadRevokeAnchor(ctx)
}

// A transient read failure of the stored anchor during Activate must neither
// lift nor restart the revocation, and must not erase the stored anchor.
func TestRevoke_AnchorReadErrorDuringActivateKeepsAnchor(t *testing.T) {
	h := newRevokeHarness(t, true, true)
	ctx := context.Background()
	rec, _ := h.store.Load(ctx)
	old := rec.Token
	fs := &flakyAnchorStore{anchorStore: anchorStore{LicenseStore: h.store}}
	svc := h.newService(t, true, true, nil)
	svc.store = fs
	h.hub.set(http.StatusForbidden, revokedBody, "")
	svc.RefreshNow(ctx)
	first := *fs.at

	// restart: empty process memory, so only the stored anchor remembers
	*h.clock = h.clock.Add(10 * time.Hour)
	svc2 := h.newService(t, true, true, nil)
	svc2.store = fs
	fs.failLoad = true
	if _, err := svc2.Activate(ctx, old); err != nil {
		t.Fatal(err)
	}
	fs.failLoad = false
	if fs.at == nil || !fs.at.Equal(first) {
		t.Fatalf("a failed read erased/moved the stored anchor: %v want %v", fs.at, first)
	}
	*h.clock = h.clock.Add(63 * time.Hour) // 73h from the first 403
	svc2.RefreshNow(ctx)
	if st := svc2.Current(); st.Status != "revoked" || !st.WritesBlocked() {
		t.Fatalf("revocation lifted by a transient read error: %+v", st)
	}
}

// A transient read failure at the moment of the 403 must not stamp a fresh
// anchor over the real one.
func TestRevoke_AnchorReadErrorDuringRevokeDoesNotOverwrite(t *testing.T) {
	h := newRevokeHarness(t, true, true)
	ctx := context.Background()
	fs := &flakyAnchorStore{anchorStore: anchorStore{LicenseStore: h.store}}
	svc := h.newService(t, true, true, nil)
	svc.store = fs
	h.hub.set(http.StatusForbidden, revokedBody, "")
	svc.RefreshNow(ctx)
	first := *fs.at

	// restart (empty memory) while the store read fails, then another 403
	svc2 := h.newService(t, true, true, nil)
	svc2.store = fs
	*h.clock = h.clock.Add(30 * time.Hour)
	fs.failLoad = true
	svc2.RefreshNow(ctx)
	fs.failLoad = false
	if fs.at == nil || !fs.at.Equal(first) {
		t.Fatalf("a failed read let a new anchor overwrite the real one: %v want %v", fs.at, first)
	}
}

// If the box clock is behind the hub's, the anchor must not land before the
// iat of the token we hold: re-pasting that token must keep the revocation.
func TestRevoke_ClockBehindHubDoesNotLiftOnRepaste(t *testing.T) {
	h := newRevokeHarness(t, true, true)
	ctx := context.Background()
	rec, _ := h.store.Load(ctx)
	cur := rec.Token
	// the held token was issued slightly "in the future" of this box clock (within the verify leeway)
	c := baseClaims()
	c.MaxOfflineHours = 24 * 14
	c.IssuedAt = h.clock.Add(4 * time.Minute) // inside the 5 min verify leeway
	future := mintToken(t, h.priv, c)
	if _, err := h.svc.Activate(ctx, future); err != nil {
		t.Fatal(err)
	}
	h.hub.set(http.StatusForbidden, revokedBody, "")
	h.svc.RefreshNow(ctx)
	if _, err := h.svc.Activate(ctx, future); err != nil {
		t.Fatal(err)
	}
	if st := h.svc.Current(); !st.Revoked {
		t.Fatalf("re-pasting the held token lifted the revocation (clock skew): %+v", st)
	}
	_ = cur
}

// bareStore implements ONLY LicenseStore (a host that predates the optional
// RevokeAnchorStore). It must keep working exactly as before.
type bareStore struct {
	mu  sync.Mutex
	rec *Record
}

func (b *bareStore) Load(context.Context) (*Record, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.rec == nil {
		return nil, nil
	}
	cp := *b.rec
	return &cp, nil
}
func (b *bareStore) Save(_ context.Context, r *Record) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	cp := *r
	b.rec = &cp
	return nil
}

func TestRevokeAnchorStore_IsOptional(t *testing.T) {
	var ls LicenseStore = &bareStore{}
	if _, ok := ls.(RevokeAnchorStore); ok {
		t.Fatal("a store with only Load/Save must not satisfy RevokeAnchorStore")
	}
	if _, ok := LicenseStore(NewMemoryStore()).(RevokeAnchorStore); ok {
		t.Fatal("MemoryStore must stay a plain LicenseStore")
	}
	// End to end with the bare store: activate, revoke (flag on), renew clears.
	h := newRevokeHarness(t, true, true)
	ctx := context.Background()
	bs := &bareStore{}
	svc := h.newService(t, true, true, nil)
	svc.store = bs
	rec, _ := h.store.Load(ctx)
	if _, err := svc.Activate(ctx, rec.Token); err != nil {
		t.Fatal(err)
	}
	h.hub.set(http.StatusForbidden, revokedBody, "")
	svc.RefreshNow(ctx)
	if st := svc.Current(); !st.Revoked {
		t.Fatalf("bare store: revocation not tracked in process: %+v", st)
	}
}

// With nothing revoked, State serializes exactly as before this feature: no
// "revoked" key at all (host /api/license responses must not change).
func TestState_NoRevokedKeyWhenNotRevoked(t *testing.T) {
	b, err := json.Marshal(State{Status: "valid", Valid: true})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	for _, k := range []string{"revoked", "revoked_at", "revoke_grace_until"} {
		if _, ok := m[k]; ok {
			t.Fatalf("%q must be omitted when not revoked: %s", k, b)
		}
	}
}
