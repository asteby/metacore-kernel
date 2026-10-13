package installer

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/asteby/metacore-kernel/hub"
	"github.com/asteby/metacore-kernel/lifecycle"
	"github.com/google/uuid"
)

type staticRevocations struct {
	list *hub.RevocationList
	err  error
}

func (s staticRevocations) List(context.Context) (*hub.RevocationList, error) { return s.list, s.err }

func revList(rs ...hub.Revocation) *hub.RevocationList {
	return &hub.RevocationList{GeneratedAt: "2026-10-01T00:00:00Z", Revoked: rs}
}

func newRevInstaller(t *testing.T, src RevocationSource) (*Installer, uuid.UUID) {
	t.Helper()
	db := upgradeTestDB(t)
	db.Exec(`ALTER TABLE metacore_installations ADD COLUMN last_error TEXT`)
	inst := &Installer{DB: db, KernelVersion: "2.0.0", AllowUnsigned: true, Lifecycles: lifecycle.NewRegistry(),
		Interceptors: lifecycle.NewInterceptorRegistry(), Broadcaster: NoopBroadcaster{}}
	inst.WithRevocations(src)
	return inst, uuid.New()
}

func status(t *testing.T, i *Installer, org uuid.UUID, key string) string {
	t.Helper()
	var row Installation
	if err := i.DB.Where("organization_id = ? AND addon_key = ?", org, key).Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	return row.Status
}

func TestInstall_RevokedBundleRejectedBeforeAnyState(t *testing.T) {
	inst, org := newRevInstaller(t, staticRevocations{list: revList(hub.Revocation{AddonKey: "demo", Version: "1.0.0", Reason: "cve-1"})})
	_, _, err := inst.Install(org, makeBundle("demo", "1.0.0", nil))
	if !errors.Is(err, ErrRevoked) {
		t.Fatalf("want ErrRevoked, got %v", err)
	}
	var n int64
	inst.DB.Model(&Installation{}).Count(&n)
	if n != 0 {
		t.Fatalf("a revoked install must leave no row, got %d", n)
	}
}

func TestInstall_NoListAvailable_FailsClosedUnlessOptedOut(t *testing.T) {
	src := staticRevocations{err: hub.ErrRevocationsUnavailable}
	inst, org := newRevInstaller(t, src)
	if _, _, err := inst.Install(org, makeBundle("demo", "1.0.0", nil)); !errors.Is(err, hub.ErrRevocationsUnavailable) {
		t.Fatalf("strict mode must fail closed when no list is known, got %v", err)
	}
	// Opt-out: passes the revocation gate (it then fails later on the
	// sqlite-incompatible schema steps, which is not a revocation error).
	inst.WithRevocationsFailOpenWhenUnknown()
	if err := inst.checkNotRevoked(context.Background(), "demo", "1.0.0", true); err != nil {
		t.Fatalf("fail-open-when-unknown must pass: %v", err)
	}
	// A KNOWN revocation is enforced even with the opt-out.
	inst.WithRevocations(staticRevocations{list: revList(hub.Revocation{AddonKey: "demo", Version: "*", Reason: "x"})})
	if err := inst.checkNotRevoked(context.Background(), "demo", "9.9.9", true); !errors.Is(err, ErrRevoked) {
		t.Fatalf("known revocation must still block: %v", err)
	}
}

func TestUpgrade_RevokedTargetRejected(t *testing.T) {
	inst, org := newRevInstaller(t, staticRevocations{list: revList(hub.Revocation{AddonKey: "demo", Version: "1.1.0", Reason: "bad"})})
	seedInstallation(t, inst.DB, org, "demo", "1.0.0", nil)
	_, err := inst.Upgrade(t.Context(), org, makeBundle("demo", "1.1.0", nil))
	if !errors.Is(err, ErrRevoked) {
		t.Fatalf("want ErrRevoked, got %v", err)
	}
	var row Installation
	inst.DB.Where("addon_key = ?", "demo").Take(&row)
	if row.Version != "1.0.0" {
		t.Fatalf("installed version must be untouched, got %s", row.Version)
	}
}

func TestEnable_RevokedInstalledVersionRefused(t *testing.T) {
	inst, org := newRevInstaller(t, staticRevocations{list: revList(hub.Revocation{AddonKey: "demo", Version: "1.0.0", Reason: "bad"})})
	b := makeBundle("demo", "1.0.0", nil)
	inst.Lifecycles.Register("demo", &lifecycle.ManifestOnly{Data: b.Manifest})
	seedInstallation(t, inst.DB, org, "demo", "1.0.0", nil)
	inst.DB.Model(&Installation{}).Where("addon_key = ?", "demo").Update("status", "disabled")
	if err := inst.Enable(org, "demo"); !errors.Is(err, ErrRevoked) {
		t.Fatalf("want ErrRevoked, got %v", err)
	}
	if s := status(t, inst, org, "demo"); s != "disabled" {
		t.Fatalf("revoked addon must stay disabled, got %s", s)
	}
	// A non-revoked version enables normally.
	inst.Revocations = staticRevocations{list: revList()}
	if err := inst.Enable(org, "demo"); err != nil {
		t.Fatalf("Enable: %v", err)
	}
}

func TestCheckRevocations_DisablesOnlyRevokedEnabledInstalls(t *testing.T) {
	inst, org := newRevInstaller(t, staticRevocations{list: revList(
		hub.Revocation{AddonKey: "bad", Version: "1.0.0", Reason: "malware"},
		hub.Revocation{AddonKey: "wild", Version: "*", Reason: "pulled"},
	)})
	org2 := uuid.New()
	for _, k := range []string{"bad", "wild", "ok"} {
		inst.Lifecycles.Register(k, &lifecycle.ManifestOnly{Data: makeBundle(k, "1.0.0", nil).Manifest})
		seedInstallation(t, inst.DB, org, k, "1.0.0", nil)
	}
	seedInstallation(t, inst.DB, org2, "bad", "1.0.0", nil) // same addon, other tenant
	seedInstallation(t, inst.DB, org, "bad2", "2.0.0", nil) // different version/key: untouched
	inst.Lifecycles.Register("bad2", &lifecycle.ManifestOnly{Data: makeBundle("bad2", "2.0.0", nil).Manifest})

	got, err := inst.CheckRevocations(t.Context())
	if err != nil {
		t.Fatalf("CheckRevocations: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 disabled (bad@org, bad@org2, wild@org), got %d: %+v", len(got), got)
	}
	for _, c := range []struct {
		o uuid.UUID
		k string
		s string
	}{{org, "bad", "disabled"}, {org2, "bad", "disabled"}, {org, "wild", "disabled"}, {org, "ok", "enabled"}, {org, "bad2", "enabled"}} {
		if s := status(t, inst, c.o, c.k); s != c.s {
			t.Errorf("%s: status %s, want %s", c.k, s, c.s)
		}
	}
	if msg := inst.LastInstallError(org, "bad"); msg != "revoked: malware" {
		t.Errorf("last_error = %q", msg)
	}
	// Idempotent: a second sweep finds nothing enabled to disable.
	if again, _ := inst.CheckRevocations(t.Context()); len(again) != 0 {
		t.Errorf("second sweep disabled %d", len(again))
	}
}

// Disable fails when the addon is not registered in the in-memory lifecycle
// registry (e.g. right after a restart); the revoked row must still end up
// disabled and the failure be reported.
func TestCheckRevocations_DisableFailureStillFailsClosed(t *testing.T) {
	inst, org := newRevInstaller(t, staticRevocations{list: revList(hub.Revocation{AddonKey: "bad", Version: "1.0.0", Reason: "x"})})
	seedInstallation(t, inst.DB, org, "bad", "1.0.0", nil) // no lifecycle registered
	got, err := inst.CheckRevocations(t.Context())
	if err == nil || len(got) != 1 || got[0].Err == nil {
		t.Fatalf("want reported disable failure, got %v %+v", err, got)
	}
	if s := status(t, inst, org, "bad"); s != "disabled" {
		t.Fatalf("revoked addon must be force-disabled, got %s", s)
	}
}

func TestCheckRevocations_NoListNothingDisabled(t *testing.T) {
	inst, org := newRevInstaller(t, staticRevocations{err: hub.ErrRevocationsUnavailable})
	seedInstallation(t, inst.DB, org, "bad", "1.0.0", nil)
	if _, err := inst.CheckRevocations(t.Context()); !errors.Is(err, hub.ErrRevocationsUnavailable) {
		t.Fatalf("want unavailable error, got %v", err)
	}
	if s := status(t, inst, org, "bad"); s != "enabled" {
		t.Fatalf("with no known list nothing is disabled, got %s", s)
	}
	if _, err := (&Installer{DB: inst.DB}).CheckRevocations(t.Context()); err == nil {
		t.Fatal("no source configured must error")
	}
}

// End to end with the real hub client: signature verified against the
// installer's trusted key, hub outage keeps enforcing the cached list.
func TestUseHubRevocations_EndToEndWithOutageAndForgery(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	var down atomic.Bool
	var forged atomic.Bool
	list := hub.RevocationList{GeneratedAt: "2026-10-01T00:00:00Z", Revoked: []hub.Revocation{{AddonKey: "bad", Version: "1.0.0", Reason: "cve", RevokedAt: "2026-10-01T00:00:00Z"}}}
	signed := func(k ed25519.PrivateKey, l hub.RevocationList) []byte {
		// Contract field order == struct order of RevocationList.
		msg, _ := json.Marshal(l)
		sig := ed25519.Sign(k, msg)
		out, _ := json.Marshal(map[string]any{"generated_at": l.GeneratedAt, "revoked": l.Revoked, "signature": base64.StdEncoding.EncodeToString(sig)})
		return out
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		if forged.Load() {
			_, evil, _ := ed25519.GenerateKey(rand.Reader)
			w.Write(signed(evil, hub.RevocationList{GeneratedAt: "2026-10-09T00:00:00Z", Revoked: []hub.Revocation{}}))
			return
		}
		w.Write(signed(priv, list))
	}))
	defer srv.Close()

	inst, org := newRevInstaller(t, nil)
	inst.PublicKeys = []ed25519.PublicKey{pub}
	hc, _ := hub.NewClient(srv.URL, "")
	rc := inst.UseHubRevocations(hc, hub.WithRevocationTTL(1)) // 1ns TTL: every call revalidates
	_ = rc
	b := makeBundle("bad", "1.0.0", nil)
	inst.Lifecycles.Register("bad", &lifecycle.ManifestOnly{Data: b.Manifest})
	seedInstallation(t, inst.DB, org, "bad", "1.0.0", nil)

	if got, err := inst.CheckRevocations(t.Context()); err != nil || len(got) != 1 {
		t.Fatalf("sweep: %v %v", got, err)
	}
	// Hub goes down and then serves a forged empty list: the revocation known
	// from the last valid list must still block re-enabling.
	down.Store(true)
	if err := inst.Enable(org, "bad"); !errors.Is(err, ErrRevoked) {
		t.Fatalf("hub down: revoked addon must stay blocked, got %v", err)
	}
	down.Store(false)
	forged.Store(true)
	if err := inst.Enable(org, "bad"); !errors.Is(err, ErrRevoked) {
		t.Fatalf("forged list must not lift a revocation, got %v", err)
	}
}

func hub1(key string) hub.Revocation {
	return hub.Revocation{AddonKey: key, Version: "*", Reason: "test"}
}
