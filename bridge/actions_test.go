package bridge

import (
	"sync"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
)

// fakeRegistry is an in-memory ActionInterceptorRegistry for tests. It
// stores entries keyed by "addon::model::action" so the tests can assert
// which addon owns a given (model, action) pair after a sync.
type fakeRegistry struct {
	mu      sync.Mutex
	entries map[string]ActionInterceptor
	owners  map[string]string // (model::action) → addonKey
}

func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{
		entries: map[string]ActionInterceptor{},
		owners:  map[string]string{},
	}
}

func (r *fakeRegistry) Register(addonKey, model, action string, fn ActionInterceptor) {
	r.mu.Lock()
	defer r.mu.Unlock()
	pairKey := model + "::" + action
	r.entries[addonKey+"::"+pairKey] = fn
	r.owners[pairKey] = addonKey
}

func (r *fakeRegistry) Unregister(addonKey, model, action string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	pairKey := model + "::" + action
	delete(r.entries, addonKey+"::"+pairKey)
	if r.owners[pairKey] == addonKey {
		delete(r.owners, pairKey)
	}
}

func (r *fakeRegistry) ownerOf(model, action string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	o, ok := r.owners[model+"::"+action]
	return o, ok
}

func (r *fakeRegistry) get(addonKey, model, action string) (ActionInterceptor, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fn, ok := r.entries[addonKey+"::"+model+"::"+action]
	return fn, ok
}

// TestSyncAddonActions_RegistersInterceptors verifies the bridge projects
// manifest.Actions into the supplied interceptor registry so the host UI
// picks them up after install.
func TestSyncAddonActions_RegistersInterceptors(t *testing.T) {
	reg := newFakeRegistry()
	b := NewActionsBridge(nil, reg, nil)
	m := manifest.Manifest{
		Key: "orders_demo",
		Actions: map[string][]manifest.ActionDef{
			"orders": {
				{Key: "cancel", Label: "Cancel"},
				{Key: "refund", Label: "Refund"},
			},
			"invoices": {
				{Key: "stamp", Label: "Stamp"},
			},
		},
	}
	if err := b.SyncAddonActions(m); err != nil {
		t.Fatalf("sync: %v", err)
	}

	for _, want := range []struct{ model, action string }{
		{"orders", "cancel"},
		{"orders", "refund"},
		{"invoices", "stamp"},
	} {
		owner, ok := reg.ownerOf(want.model, want.action)
		if !ok {
			t.Fatalf("missing interceptor %s::%s", want.model, want.action)
		}
		if owner != "orders_demo" {
			t.Fatalf("interceptor owner = %s, want orders_demo", owner)
		}
		if fn, ok := reg.get("orders_demo", want.model, want.action); !ok || fn == nil {
			t.Fatalf("interceptor fn missing for %s::%s", want.model, want.action)
		}
	}

	keys := b.RegisteredKeys("orders_demo")
	if len(keys) != 3 {
		t.Fatalf("RegisteredKeys = %d, want 3", len(keys))
	}
}

// TestSyncAddonActions_Idempotent verifies re-syncing the same manifest
// leaves the registry stable — and that a shrinking manifest drops the
// removed action from bookkeeping.
func TestSyncAddonActions_Idempotent(t *testing.T) {
	reg := newFakeRegistry()
	b := NewActionsBridge(nil, reg, nil)
	full := manifest.Manifest{
		Key: "idem",
		Actions: map[string][]manifest.ActionDef{
			"orders": {{Key: "cancel"}, {Key: "refund"}},
		},
	}
	if err := b.SyncAddonActions(full); err != nil {
		t.Fatalf("sync full: %v", err)
	}
	if err := b.SyncAddonActions(full); err != nil {
		t.Fatalf("sync full again: %v", err)
	}
	shrunk := manifest.Manifest{
		Key: "idem",
		Actions: map[string][]manifest.ActionDef{
			"orders": {{Key: "cancel"}},
		},
	}
	if err := b.SyncAddonActions(shrunk); err != nil {
		t.Fatalf("sync shrunk: %v", err)
	}
	if keys := b.RegisteredKeys("idem"); len(keys) != 1 {
		t.Fatalf("after shrink keys = %v, want 1", keys)
	}
	// The dropped action's slot must be gone from the registry.
	if _, ok := reg.ownerOf("orders", "refund"); ok {
		t.Fatal("refund should have been unregistered after shrink")
	}
}

// TestRemoveAddonActions_ClearsBookkeeping verifies uninstall empties the
// per-addon tracking so a later re-install starts clean.
func TestRemoveAddonActions_ClearsBookkeeping(t *testing.T) {
	reg := newFakeRegistry()
	b := NewActionsBridge(nil, reg, nil)
	m := manifest.Manifest{
		Key: "rm",
		Actions: map[string][]manifest.ActionDef{
			"items": {{Key: "archive"}},
		},
	}
	if err := b.SyncAddonActions(m); err != nil {
		t.Fatalf("sync: %v", err)
	}
	b.RemoveAddonActions("rm")
	if keys := b.RegisteredKeys("rm"); len(keys) != 0 {
		t.Fatalf("RegisteredKeys after remove = %v, want empty", keys)
	}
	if _, ok := reg.ownerOf("items", "archive"); ok {
		t.Fatal("registry should have been cleared")
	}
}

// TestSyncAddonActions_NilRegistry verifies a clean error path when the
// host forgets to wire a registry.
func TestSyncAddonActions_NilRegistry(t *testing.T) {
	b := NewActionsBridge(nil, nil, nil)
	err := b.SyncAddonActions(manifest.Manifest{Key: "x"})
	if err == nil {
		t.Fatal("expected error for nil registry")
	}
}
