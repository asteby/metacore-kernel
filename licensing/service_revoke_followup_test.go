package licensing

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

// failSaveAnchorStore is an anchorStore whose side-channel write can fail.
type failSaveAnchorStore struct {
	flakyAnchorStore
	failSave bool
}

func (f *failSaveAnchorStore) SaveRevokeAnchor(ctx context.Context, at *time.Time) error {
	if f.failSave {
		return errors.New("write failed")
	}
	return f.anchorStore.SaveRevokeAnchor(ctx, at)
}

// The first 403 lands while the anchor read fails: the anchor lives only in
// memory. Once reads recover, a healthy cycle must persist it, so a restart
// does not restart the grace.
func TestRevoke_MemoryAnchorPersistedAfterReadRecovers(t *testing.T) {
	h := newRevokeHarness(t, true, true)
	ctx := context.Background()
	fs := &flakyAnchorStore{anchorStore: anchorStore{LicenseStore: h.store}}
	svc := h.newService(t, true, true, nil)
	svc.store = fs
	h.hub.set(http.StatusForbidden, revokedBody, "")

	fs.failLoad = true
	svc.RefreshNow(ctx)
	first := *svc.Current().RevokedAt
	fs.failLoad = false
	if fs.at != nil {
		t.Fatalf("precondition: anchor must not be persisted yet")
	}
	for i := 0; i < 5; i++ { // 5 healthy cycles, 30h
		*h.clock = h.clock.Add(6 * time.Hour)
		svc.RefreshNow(ctx)
	}
	if fs.at == nil || !fs.at.Equal(first) {
		t.Fatalf("in-memory anchor never persisted: %v want %v", fs.at, first)
	}

	svc2 := h.newService(t, true, true, nil) // restart
	svc2.store = fs
	svc2.Boot(ctx)
	if st := svc2.Current(); !st.Revoked || !st.RevokedAt.Equal(first) {
		t.Fatalf("restart restarted the grace: %+v", st)
	}
	*h.clock = h.clock.Add(43 * time.Hour) // 73h after the first 403
	svc2.RefreshNow(ctx)
	if st := svc2.Current(); st.Status != "revoked" || !st.WritesBlocked() {
		t.Fatalf("grace restarted after restart: %+v", st)
	}
}

// Same hole when the first SaveRevokeAnchor fails: a later cycle retries.
func TestRevoke_FailedAnchorSaveRetriedNextCycle(t *testing.T) {
	h := newRevokeHarness(t, true, true)
	ctx := context.Background()
	fs := &failSaveAnchorStore{flakyAnchorStore: flakyAnchorStore{anchorStore: anchorStore{LicenseStore: h.store}}}
	svc := h.newService(t, true, true, nil)
	svc.store = fs
	h.hub.set(http.StatusForbidden, revokedBody, "")

	fs.failSave = true
	svc.RefreshNow(ctx)
	first := *svc.Current().RevokedAt
	if fs.at != nil {
		t.Fatalf("precondition: save should have failed")
	}
	fs.failSave = false
	*h.clock = h.clock.Add(6 * time.Hour)
	svc.RefreshNow(ctx)
	if fs.at == nil || !fs.at.Equal(first) {
		t.Fatalf("failed save not retried: %v want %v", fs.at, first)
	}
}

// LICENSING_REVOKE_GRACE_HOURS=0 means read-only immediately; unset means 72h.
func TestRevoke_EnvGraceZeroIsImmediateAndUnsetIs72h(t *testing.T) {
	t.Setenv("LICENSING_REVOKE_GRACE_HOURS", "0")
	cfg := NewConfigFromEnv()
	if cfg.RevokeGrace != 0 || !cfg.RevokeGraceSet {
		t.Fatalf("zero not parsed as explicit: %+v", cfg)
	}
	if got := New(cfg).revokeGrace; got != 0 {
		t.Fatalf("explicit 0h grace = %v want 0", got)
	}

	h := newRevokeHarness(t, true, true)
	svc := New(Config{
		Enforce: true, RevokeEnforce: true, RevokeGraceSet: true,
		HubBaseURL: h.srv.URL, PubKeyHex: h.pub, Store: h.store,
		Now: func() time.Time { return *h.clock },
	})
	svc.instanceID, svc.instancePriv = h.svc.instanceID, h.svc.instancePriv
	h.hub.set(http.StatusForbidden, revokedBody, "")
	svc.RefreshNow(context.Background())
	if st := svc.Current(); st.Status != "revoked" || !st.WritesBlocked() {
		t.Fatalf("0h grace must be read-only at once: %+v", st)
	}

	t.Setenv("LICENSING_REVOKE_GRACE_HOURS", "")
	cfg = NewConfigFromEnv()
	if cfg.RevokeGraceSet {
		t.Fatalf("unset env must not mark the grace as set")
	}
	if got := New(cfg).revokeGrace; got != DefaultRevokeGrace {
		t.Fatalf("unset grace = %v want 72h", got)
	}
}

// A revoked anchor with no token installed (status missing) must not surface
// as revoked: there is nothing to revoke.
func TestRevoke_NoTokenInstalledStaysMissing(t *testing.T) {
	h := newRevokeHarness(t, true, true)
	ctx := context.Background()
	past := h.clock.Add(-200 * time.Hour)
	h.svc.recompute(ctx, &Record{RevokedAt: &past})
	st := h.svc.Current()
	if st.Status != "missing" || st.Revoked || st.RevokedAt != nil {
		t.Fatalf("revocation without a token must stay missing: %+v", st)
	}
}
