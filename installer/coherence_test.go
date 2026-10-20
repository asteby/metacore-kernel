package installer

import (
	"context"
	"sync"
	"testing"

	"github.com/asteby/metacore-kernel/bundle"
	"github.com/asteby/metacore-kernel/cachecoherence"
	"github.com/asteby/metacore-kernel/lifecycle"
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/google/uuid"
)

// driftLoader simulates the wasm adapter in the "new manifest, old module"
// state: the first verification reports a missing export; a reload cures it
// (cure=true) or does not (cure=false).
type driftLoader struct {
	mu       sync.Mutex
	cure     bool
	reloaded int
	outcomes []string
	missing  []string
}

func (d *driftLoader) LoadFromBundle(context.Context, *bundle.Bundle) error { return nil }

func (d *driftLoader) VerifyLoaded(context.Context, *bundle.Bundle) ([]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.missing...), nil
}

func (d *driftLoader) ReloadFromBundle(context.Context, *bundle.Bundle) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reloaded++
	if d.cure {
		d.missing = nil
	}
	return nil
}

func (d *driftLoader) NoteDrift(_, outcome string) {
	d.mu.Lock()
	d.outcomes = append(d.outcomes, outcome)
	d.mu.Unlock()
}

func driftBundle() *bundle.Bundle {
	b := makeBundle("storefront_cart", "0.2.2", nil)
	b.Manifest.Backend = &manifest.BackendSpec{Runtime: "wasm", Entry: "backend/backend.wasm", Exports: []string{"cart_get", "address_post"}}
	b.Backend = map[string][]byte{"backend/backend.wasm": []byte("x")}
	return b
}

func TestVerifyBackendExports_ReloadsOnceAndCures(t *testing.T) {
	l := &driftLoader{cure: true, missing: []string{"address_post"}}
	i := &Installer{BackendRuntime: l}
	if err := i.verifyBackendExports(context.Background(), l, driftBundle()); err != nil {
		t.Fatal(err)
	}
	if l.reloaded != 1 || len(l.missing) != 0 {
		t.Fatalf("reloaded=%d missing=%v", l.reloaded, l.missing)
	}
	if len(l.outcomes) != 1 || l.outcomes[0] != "reloaded" {
		t.Fatalf("outcomes = %v", l.outcomes)
	}
}

func TestVerifyBackendExports_PersistingIsReportedAndOnlyFailsWhenStrict(t *testing.T) {
	l := &driftLoader{cure: false, missing: []string{"address_post"}}
	i := &Installer{BackendRuntime: l}
	if err := i.verifyBackendExports(context.Background(), l, driftBundle()); err != nil {
		t.Fatalf("lenient mode must not fail the install: %v", err)
	}
	if l.reloaded != 1 {
		t.Fatalf("must reload exactly once, got %d", l.reloaded)
	}
	if len(l.outcomes) != 1 || l.outcomes[0] != "persisting" {
		t.Fatalf("outcomes = %v", l.outcomes)
	}
	i.StrictBackendExports = true
	if err := i.verifyBackendExports(context.Background(), l, driftBundle()); err == nil {
		t.Fatal("strict mode must fail while exports are missing")
	}
}

func TestVerifyBackendExports_NoDriftIsSilent(t *testing.T) {
	l := &driftLoader{}
	i := &Installer{BackendRuntime: l}
	if err := i.verifyBackendExports(context.Background(), l, driftBundle()); err != nil {
		t.Fatal(err)
	}
	if l.reloaded != 0 || len(l.outcomes) != 0 {
		t.Fatalf("reloaded=%d outcomes=%v", l.reloaded, l.outcomes)
	}
	// A loader without the optional interfaces is skipped.
	if err := i.verifyBackendExports(context.Background(), NoopBackendRuntimeLoader{}, driftBundle()); err != nil {
		t.Fatal(err)
	}
}

// Upgrade (and every other lifecycle operation) ends with a coherence bump
// scoped to the org and addon, after which the installer's consumers can tell
// their caches are stale by comparing epochs.
func TestUpgrade_BumpsCoherenceScopedToOrgAndAddon(t *testing.T) {
	db := upgradeTestDB(t)
	orgID := uuid.New()
	seedInstallation(t, db, orgID, "demo", "1.0.0", nil)

	reg := cachecoherence.NewRegistry()
	var seen []cachecoherence.Event
	reg.Subscribe(func(ev cachecoherence.Event) { seen = append(seen, ev) })
	var flushed int
	reg.Register("probe", cachecoherence.OrderMetadata, func(context.Context, cachecoherence.Event) error { flushed++; return nil })

	watcher := reg.NewWatcher(orgID.String(), "demo")
	other := reg.NewWatcher(uuid.NewString(), "another")

	inst := &Installer{
		DB:             db,
		KernelVersion:  "2.0.0",
		Lifecycles:     lifecycle.NewRegistry(),
		AllowUnsigned:  true,
		schemaApplier:  &recordingApplier{},
		Broadcaster:    NoopBroadcaster{},
		BackendRuntime: NoopBackendRuntimeLoader{},
		Coherence:      reg,
	}
	if _, err := inst.Upgrade(context.Background(), orgID, makeBundle("demo", "1.1.0", nil)); err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	if len(seen) != 1 || seen[0].Reason != "addon.upgrade" || seen[0].AddonKey != "demo" || seen[0].OrgID != orgID.String() {
		t.Fatalf("events = %+v", seen)
	}
	if flushed != 1 {
		t.Fatalf("invalidators ran %d times", flushed)
	}
	if !watcher.Stale() {
		t.Fatal("the upgraded org/addon view must be stale")
	}
	if other.Stale() {
		t.Fatal("an unrelated org/addon view must not be touched")
	}
}

// A rejected upgrade may already have run migrations: it still flushes.
func TestUpgrade_FailureStillBumps(t *testing.T) {
	db := upgradeTestDB(t)
	orgID := uuid.New()
	reg := cachecoherence.NewRegistry()
	n := 0
	reg.Subscribe(func(cachecoherence.Event) { n++ })
	inst := &Installer{
		DB: db, KernelVersion: "2.0.0", Lifecycles: lifecycle.NewRegistry(), AllowUnsigned: true,
		schemaApplier: &recordingApplier{}, Broadcaster: NoopBroadcaster{}, BackendRuntime: NoopBackendRuntimeLoader{},
		Coherence: reg,
	}
	// not installed -> ErrNotInstalled
	if _, err := inst.Upgrade(context.Background(), orgID, makeBundle("ghost", "1.0.0", nil)); err == nil {
		t.Fatal("expected an error")
	}
	if n != 1 {
		t.Fatalf("bumps = %d, want 1", n)
	}
}
