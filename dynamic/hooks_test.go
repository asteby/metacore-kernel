package dynamic

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/google/uuid"
)

// stubInvoker records every Run call so tests can assert dispatch
// happened with the right event + addon key. The vetoErr field lets a
// test simulate a before_* hook abort.
type stubInvoker struct {
	mu      sync.Mutex
	hits    []invokerCall
	vetoErr error
}

type invokerCall struct {
	addonKey string
	orgID    uuid.UUID
	event    string
}

func (s *stubInvoker) Run(_ context.Context, addonKey string, orgID uuid.UUID, event string, _ manifest.Manifest, _ []byte) error {
	s.mu.Lock()
	s.hits = append(s.hits, invokerCall{addonKey, orgID, event})
	s.mu.Unlock()
	return s.vetoErr
}

func (s *stubInvoker) snapshot() []invokerCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]invokerCall, len(s.hits))
	copy(out, s.hits)
	return out
}

func newManifestWithCRUDHooks(model string, events ...string) manifest.Manifest {
	hooks := map[string][]manifest.HookDef{}
	for _, ev := range events {
		hooks[ev] = []manifest.HookDef{
			{Target: manifest.HookTarget{Type: "webhook", URL: "https://example/hook"}},
		}
	}
	return manifest.Manifest{
		Key:     "demo",
		Name:    "Demo",
		Version: "1.0.0",
		ModelDefinitions: []manifest.ModelDefinition{
			{ModelKey: model, TableName: model, Columns: []manifest.ColumnDef{{Name: "title", Type: "string"}}},
		},
		LifecycleHooks: hooks,
	}
}

func TestHookRegistry_RegisterManifestHooks_WiresEveryCRUDEvent(t *testing.T) {
	reg := NewHookRegistry()
	inv := &stubInvoker{}
	m := newManifestWithCRUDHooks("tickets",
		"before_create", "after_create",
		"before_update", "after_update",
		"before_delete", "after_delete",
	)
	reg.RegisterManifestHooks("demo", m, inv)

	hc := HookContext{Model: "tickets", User: &fakeUser{orgID: uuid.New()}}
	ctx := context.Background()

	if err := reg.runBeforeCreate(ctx, hc, map[string]any{}); err != nil {
		t.Fatalf("runBeforeCreate: %v", err)
	}
	if err := reg.runAfterCreate(ctx, hc, struct{}{}); err != nil {
		t.Fatalf("runAfterCreate: %v", err)
	}
	if err := reg.runBeforeUpdate(ctx, hc, "id-1", map[string]any{}); err != nil {
		t.Fatalf("runBeforeUpdate: %v", err)
	}
	if err := reg.runAfterUpdate(ctx, hc, struct{}{}); err != nil {
		t.Fatalf("runAfterUpdate: %v", err)
	}
	if err := reg.runBeforeDelete(ctx, hc, "id-1"); err != nil {
		t.Fatalf("runBeforeDelete: %v", err)
	}
	if err := reg.runAfterDelete(ctx, hc, "id-1"); err != nil {
		t.Fatalf("runAfterDelete: %v", err)
	}
	got := inv.snapshot()
	if len(got) != 6 {
		t.Fatalf("expected 6 invoker calls, got %d", len(got))
	}
	wantEvents := []string{
		"before_create", "after_create",
		"before_update", "after_update",
		"before_delete", "after_delete",
	}
	for i, ev := range wantEvents {
		if got[i].event != ev {
			t.Errorf("call[%d].event = %q, want %q", i, got[i].event, ev)
		}
		if got[i].addonKey != "demo" {
			t.Errorf("call[%d].addonKey = %q, want demo", i, got[i].addonKey)
		}
	}
}

func TestHookRegistry_RegisterManifestHooks_BeforeCreateVetoIsPropagated(t *testing.T) {
	reg := NewHookRegistry()
	inv := &stubInvoker{vetoErr: errors.New("guest rejected")}
	m := newManifestWithCRUDHooks("tickets", "before_create")
	reg.RegisterManifestHooks("demo", m, inv)

	hc := HookContext{Model: "tickets", User: &fakeUser{orgID: uuid.New()}}
	err := reg.runBeforeCreate(context.Background(), hc, map[string]any{})
	if err == nil {
		t.Fatal("expected veto to propagate through HookRegistry.runBeforeCreate")
	}
}

func TestHookRegistry_RegisterManifestHooks_Idempotent(t *testing.T) {
	// Reinstalling the same addon (manifest re-applied) must not
	// double-fire the hooks — the registry has to strip previous
	// registrations before adding the new shape.
	reg := NewHookRegistry()
	inv := &stubInvoker{}
	m := newManifestWithCRUDHooks("tickets", "after_create")
	reg.RegisterManifestHooks("demo", m, inv)
	reg.RegisterManifestHooks("demo", m, inv)

	hc := HookContext{Model: "tickets", User: &fakeUser{orgID: uuid.New()}}
	if err := reg.runAfterCreate(context.Background(), hc, struct{}{}); err != nil {
		t.Fatalf("runAfterCreate: %v", err)
	}
	if got := len(inv.snapshot()); got != 1 {
		t.Fatalf("expected 1 invoker call after reinstall, got %d", got)
	}
}

func TestHookRegistry_UnregisterAddon_RemovesManifestHooks(t *testing.T) {
	reg := NewHookRegistry()
	inv := &stubInvoker{}
	m := newManifestWithCRUDHooks("tickets", "after_create")
	reg.RegisterManifestHooks("demo", m, inv)
	reg.UnregisterAddon("demo")

	hc := HookContext{Model: "tickets", User: &fakeUser{orgID: uuid.New()}}
	if err := reg.runAfterCreate(context.Background(), hc, struct{}{}); err != nil {
		t.Fatalf("runAfterCreate: %v", err)
	}
	if got := len(inv.snapshot()); got != 0 {
		t.Fatalf("expected 0 invoker calls after UnregisterAddon, got %d", got)
	}
}

func TestHookRegistry_RegisterManifestHooks_NoCRUDHooksIsNoop(t *testing.T) {
	// Manifest declares only a lifecycle hook ("install") — the CRUD
	// adapter must not register anything against the dynamic registry,
	// because lifecycle events fire from the installer, not the
	// per-request CRUD path.
	reg := NewHookRegistry()
	inv := &stubInvoker{}
	m := manifest.Manifest{
		Key:     "demo",
		Version: "1.0.0",
		ModelDefinitions: []manifest.ModelDefinition{
			{ModelKey: "tickets", TableName: "tickets",
				Columns: []manifest.ColumnDef{{Name: "title", Type: "string"}}},
		},
		LifecycleHooks: map[string][]manifest.HookDef{
			"install": {{Target: manifest.HookTarget{Type: "webhook", URL: "x"}}},
		},
	}
	reg.RegisterManifestHooks("demo", m, inv)

	hc := HookContext{Model: "tickets", User: &fakeUser{orgID: uuid.New()}}
	if err := reg.runAfterCreate(context.Background(), hc, struct{}{}); err != nil {
		t.Fatalf("runAfterCreate: %v", err)
	}
	if got := len(inv.snapshot()); got != 0 {
		t.Fatalf("expected 0 invoker calls — install hooks are not CRUD, got %d", got)
	}
}

func TestHookRegistry_NilInvoker_IsNoop(t *testing.T) {
	reg := NewHookRegistry()
	m := newManifestWithCRUDHooks("tickets", "after_create")
	reg.RegisterManifestHooks("demo", m, nil) // nil invoker is silent
	hc := HookContext{Model: "tickets", User: &fakeUser{orgID: uuid.New()}}
	if err := reg.runAfterCreate(context.Background(), hc, struct{}{}); err != nil {
		t.Fatalf("runAfterCreate: %v", err)
	}
}
