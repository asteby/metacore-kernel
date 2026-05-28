package installer

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/asteby/metacore-kernel/bundle"
	"github.com/asteby/metacore-kernel/lifecycle"
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/google/uuid"
)

// recordingBackendLoader captures every LoadFromBundle call so the test can
// assert call count + the manifest key that landed.
type recordingBackendLoader struct {
	mu    sync.Mutex
	calls []*bundle.Bundle
	err   error
}

func (r *recordingBackendLoader) LoadFromBundle(_ context.Context, b *bundle.Bundle) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, b)
	return r.err
}

func (r *recordingBackendLoader) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func TestNeedsBackendRuntime(t *testing.T) {
	cases := []struct {
		name string
		spec *manifest.BackendSpec
		want bool
	}{
		{"nil", nil, false},
		{"empty runtime", &manifest.BackendSpec{}, false},
		{"webhook", &manifest.BackendSpec{Runtime: "webhook"}, false},
		{"wasm", &manifest.BackendSpec{Runtime: "wasm"}, true},
		{"binary", &manifest.BackendSpec{Runtime: "binary"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := needsBackendRuntime(c.spec); got != c.want {
				t.Fatalf("needsBackendRuntime(%v)=%v want %v", c.spec, got, c.want)
			}
		})
	}
}

// TestUpgrade_FiresBackendLoader exercises the WASM runtime materialisation
// path the new install pipeline added. We use Upgrade rather than Install
// because Upgrade already accepts a schemaApplier stub that bypasses
// postgres-only DDL in sqlite-backed tests; the loader fire-site is
// symmetric in both paths.
func TestUpgrade_FiresBackendLoader_WhenWASM(t *testing.T) {
	db := upgradeTestDB(t)
	orgID := uuid.New()
	seedInstallation(t, db, orgID, "demo", "1.0.0", nil)

	loader := &recordingBackendLoader{}
	inst := &Installer{
		DB:             db,
		KernelVersion:  "2.0.0",
		Lifecycles:     lifecycle.NewRegistry(),
		AllowUnsigned:  true,
		schemaApplier:  &recordingApplier{},
		Broadcaster:    NoopBroadcaster{},
		BackendRuntime: loader,
	}

	newBundle := makeBundle("demo", "1.1.0", nil)
	newBundle.Manifest.Backend = &manifest.BackendSpec{
		Runtime: "wasm",
		Entry:   "backend/backend.wasm",
		Exports: []string{"OnInstall"},
	}
	newBundle.Backend = map[string][]byte{
		"backend/backend.wasm": []byte("fake wasm bytes — not actually compiled in this test"),
	}

	if _, err := inst.Upgrade(context.Background(), orgID, newBundle); err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	if loader.callCount() != 1 {
		t.Fatalf("expected 1 LoadFromBundle call, got %d", loader.callCount())
	}
	if loader.calls[0].Manifest.Key != "demo" {
		t.Fatalf("unexpected bundle key: %s", loader.calls[0].Manifest.Key)
	}
}

func TestUpgrade_SkipsBackendLoader_WhenWebhook(t *testing.T) {
	db := upgradeTestDB(t)
	orgID := uuid.New()
	seedInstallation(t, db, orgID, "demo", "1.0.0", nil)

	loader := &recordingBackendLoader{}
	inst := &Installer{
		DB:             db,
		KernelVersion:  "2.0.0",
		Lifecycles:     lifecycle.NewRegistry(),
		AllowUnsigned:  true,
		schemaApplier:  &recordingApplier{},
		Broadcaster:    NoopBroadcaster{},
		BackendRuntime: loader,
	}

	newBundle := makeBundle("demo", "1.1.0", nil)
	newBundle.Manifest.Backend = &manifest.BackendSpec{Runtime: "webhook", URL: "https://example.com"}

	if _, err := inst.Upgrade(context.Background(), orgID, newBundle); err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	if loader.callCount() != 0 {
		t.Fatalf("expected webhook backend to skip loader, got %d calls", loader.callCount())
	}
}

func TestUpgrade_AbortsOnBackendLoaderError(t *testing.T) {
	db := upgradeTestDB(t)
	orgID := uuid.New()
	seedInstallation(t, db, orgID, "demo", "1.0.0", nil)

	wantErr := errors.New("compile failed")
	loader := &recordingBackendLoader{err: wantErr}
	inst := &Installer{
		DB:             db,
		KernelVersion:  "2.0.0",
		Lifecycles:     lifecycle.NewRegistry(),
		AllowUnsigned:  true,
		schemaApplier:  &recordingApplier{},
		Broadcaster:    NoopBroadcaster{},
		BackendRuntime: loader,
	}

	newBundle := makeBundle("demo", "1.1.0", nil)
	newBundle.Manifest.Backend = &manifest.BackendSpec{Runtime: "wasm", Entry: "backend/backend.wasm"}
	newBundle.Backend = map[string][]byte{"backend/backend.wasm": {0x00}}

	_, err := inst.Upgrade(context.Background(), orgID, newBundle)
	if err == nil || !errors.Is(err, wantErr) {
		t.Fatalf("expected Upgrade to surface loader error, got %v", err)
	}
}

func TestNoopBackendRuntimeLoader_Noop(t *testing.T) {
	if err := (NoopBackendRuntimeLoader{}).LoadFromBundle(context.Background(), &bundle.Bundle{}); err != nil {
		t.Fatalf("noop must return nil, got %v", err)
	}
}

func TestWithBackendRuntime_NilRevertsToNoop(t *testing.T) {
	inst := &Installer{BackendRuntime: &recordingBackendLoader{}}
	inst.WithBackendRuntime(nil)
	if _, ok := inst.BackendRuntime.(NoopBackendRuntimeLoader); !ok {
		t.Fatalf("expected NoopBackendRuntimeLoader after nil swap, got %T", inst.BackendRuntime)
	}
}
