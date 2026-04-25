package tool

import (
	"testing"

	"github.com/asteby/metacore-kernel/installer"
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/security"
	"github.com/google/uuid"
)

// noopSignerLookup returns no signer so the dispatcher produces unsigned
// calls — we never actually send here, but SyncFromManifest needs a real
// *WebhookDispatcher to hand to the HTTPDispatcher.
func noopSignerLookup(uuid.UUID) (*security.Signer, error) { return nil, nil }

func TestSyncFromManifest_RegistersAndReplaces(t *testing.T) {
	reg := NewRegistry()
	disp := security.NewWebhookDispatcher(noopSignerLookup)

	inst := installer.Installation{
		ID:             uuid.New(),
		OrganizationID: uuid.New(),
		AddonKey:       "test.addon",
	}
	m := manifest.Manifest{
		Key: "test.addon",
		Tools: []manifest.ToolDef{
			{ID: "t1", Name: "Tool1", Endpoint: "https://example.test/t1"},
			{ID: "t2", Name: "Tool2", Endpoint: "https://example.test/t2"},
		},
	}

	n, err := SyncFromManifest(m, inst, disp, reg)
	if err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if n != 2 {
		t.Fatalf("want 2 tools, got %d", n)
	}
	if _, ok := reg.ByID("test.addon", "t1"); !ok {
		t.Fatalf("expected t1 registered")
	}
	if _, ok := reg.ByID("test.addon", "t2"); !ok {
		t.Fatalf("expected t2 registered")
	}

	// Idempotent: re-sync with one fewer tool must drop the orphan.
	m.Tools = []manifest.ToolDef{
		{ID: "t1", Name: "Tool1", Endpoint: "https://example.test/t1"},
	}
	n, err = SyncFromManifest(m, inst, disp, reg)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if n != 1 {
		t.Fatalf("want 1 tool after replace, got %d", n)
	}
	if _, ok := reg.ByID("test.addon", "t2"); ok {
		t.Fatalf("t2 should have been purged on re-sync")
	}
}

func TestSyncFromManifest_RejectsBadInputs(t *testing.T) {
	reg := NewRegistry()
	disp := security.NewWebhookDispatcher(noopSignerLookup)
	inst := installer.Installation{ID: uuid.New(), OrganizationID: uuid.New(), AddonKey: "x"}

	if _, err := SyncFromManifest(manifest.Manifest{Key: "x"}, inst, disp, nil); err == nil {
		t.Fatal("nil registry should error")
	}
	if _, err := SyncFromManifest(manifest.Manifest{Key: "x"}, inst, nil, reg); err == nil {
		t.Fatal("nil dispatcher should error")
	}
	if _, err := SyncFromManifest(manifest.Manifest{}, inst, disp, reg); err == nil {
		t.Fatal("missing manifest key should error")
	}
}

func TestRemoveAddon(t *testing.T) {
	reg := NewRegistry()
	disp := security.NewWebhookDispatcher(noopSignerLookup)
	inst := installer.Installation{ID: uuid.New(), OrganizationID: uuid.New(), AddonKey: "a"}
	m := manifest.Manifest{Key: "a", Tools: []manifest.ToolDef{
		{ID: "one", Endpoint: "https://example.test/one"},
	}}
	if _, err := SyncFromManifest(m, inst, disp, reg); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if got := RemoveAddon(reg, "a"); got != 1 {
		t.Fatalf("want 1 removed, got %d", got)
	}
	if _, ok := reg.ByID("a", "one"); ok {
		t.Fatal("tool should be purged")
	}
}

func TestGlobalRegistry_Singleton(t *testing.T) {
	a := GlobalRegistry()
	b := GlobalRegistry()
	if a != b {
		t.Fatal("registry should be a singleton")
	}
}
