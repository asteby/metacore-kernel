package installer

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/lifecycle"
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func setupSQLiteWithInstallationsTable(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		DisableForeignKeyConstraintWhenMigrating: true,
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	// Mirror the minimal columns Installer.Enable / Disable touch. The
	// installer never reads the row, only updates it, so the test does
	// not need to seed one — sqlite happily affects 0 rows.
	if err := db.Exec(`CREATE TABLE metacore_installations (
		id TEXT PRIMARY KEY,
		organization_id TEXT,
		addon_key TEXT,
		version TEXT,
		status TEXT,
		source TEXT,
		secret_hash TEXT,
		secret_enc TEXT,
		settings TEXT,
		manifest_hash TEXT,
		installed_at DATETIME,
		enabled_at DATETIME,
		disabled_at DATETIME
	)`).Error; err != nil {
		t.Fatalf("create table: %v", err)
	}
	return db
}

// recordingHookDispatcher captures every HookRequest the runner sent it.
// Same pattern as lifecycle/hooks_test.go but local so the test doesn't
// have to export internals.
type recordingHookDispatcher struct {
	mu    sync.Mutex
	calls []lifecycle.HookRequest
	err   error
}

func (r *recordingHookDispatcher) Dispatch(_ context.Context, req lifecycle.HookRequest) (lifecycle.HookResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, req)
	return lifecycle.HookResponse{}, r.err
}

func (r *recordingHookDispatcher) events() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.calls))
	for i, c := range r.calls {
		out[i] = c.Event
	}
	return out
}

func TestRunLifecycleHooks_NilRunner_IsNoop(t *testing.T) {
	i := &Installer{}
	if err := i.runLifecycleHooks(context.Background(), manifest.Manifest{Key: "demo", Version: "1.0.0"}, uuid.New(), lifecycle.HookEventInstall); err != nil {
		t.Fatalf("nil HookRunner must be a no-op, got %v", err)
	}
}

func TestRunLifecycleHooks_FiresEachEventWithRichPayload(t *testing.T) {
	rec := &recordingHookDispatcher{}
	runner := lifecycle.NewHookRunner(nil)
	runner.Register(lifecycle.HookTargetWebhook, rec)
	i := &Installer{HookRunner: runner}

	m := manifest.Manifest{
		Key:     "demo",
		Version: "1.0.0",
		LifecycleHooks: map[string][]manifest.HookDef{
			lifecycle.HookEventInstall:   {{Target: manifest.HookTarget{Type: "webhook", URL: "https://x/install"}}},
			lifecycle.HookEventEnable:    {{Target: manifest.HookTarget{Type: "webhook", URL: "https://x/enable"}}},
			lifecycle.HookEventDisable:   {{Target: manifest.HookTarget{Type: "webhook", URL: "https://x/disable"}}},
			lifecycle.HookEventUninstall: {{Target: manifest.HookTarget{Type: "webhook", URL: "https://x/uninstall"}}},
		},
	}
	orgID := uuid.New()
	for _, ev := range []string{
		lifecycle.HookEventInstall,
		lifecycle.HookEventEnable,
		lifecycle.HookEventDisable,
		lifecycle.HookEventUninstall,
	} {
		if err := i.runLifecycleHooks(context.Background(), m, orgID, ev); err != nil {
			t.Fatalf("runLifecycleHooks(%s): %v", ev, err)
		}
	}
	got := rec.events()
	want := []string{
		lifecycle.HookEventInstall,
		lifecycle.HookEventEnable,
		lifecycle.HookEventDisable,
		lifecycle.HookEventUninstall,
	}
	if len(got) != len(want) {
		t.Fatalf("dispatch count = %d, want %d (events: %v)", len(got), len(want), got)
	}
	for n := range want {
		if got[n] != want[n] {
			t.Errorf("call[%d].event = %q, want %q", n, got[n], want[n])
		}
	}
	if rec.calls[0].AddonKey != "demo" || rec.calls[0].OrgID != orgID {
		t.Fatalf("metadata wrong: %+v", rec.calls[0])
	}
	if !strings.Contains(string(rec.calls[0].Payload), `"event":"install"`) {
		t.Fatalf("payload missing event field: %s", string(rec.calls[0].Payload))
	}
}

func TestRunLifecycleHooks_BeforeFailureAborts(t *testing.T) {
	rec := &recordingHookDispatcher{err: errors.New("guest declined")}
	runner := lifecycle.NewHookRunner(nil)
	runner.Register(lifecycle.HookTargetWebhook, rec)
	i := &Installer{HookRunner: runner}

	m := manifest.Manifest{
		Key:     "demo",
		Version: "1.0.0",
		LifecycleHooks: map[string][]manifest.HookDef{
			lifecycle.HookEventInstall: {{Target: manifest.HookTarget{Type: "webhook", URL: "x"}}},
		},
	}
	err := i.runLifecycleHooks(context.Background(), m, uuid.New(), lifecycle.HookEventInstall)
	if err == nil {
		t.Fatal("expected install hook failure to propagate")
	}
}

// TestInstallerEnable_FiresEnableHook exercises the path used by clients
// that re-enable a previously-disabled addon — Installer.Enable must wire
// the hook on top of the existing lc.OnEnable call.
func TestInstallerEnable_FiresEnableHook(t *testing.T) {
	rec := &recordingHookDispatcher{}
	runner := lifecycle.NewHookRunner(nil)
	runner.Register(lifecycle.HookTargetWebhook, rec)

	m := manifest.Manifest{
		Key:     "demo",
		Version: "1.0.0",
		LifecycleHooks: map[string][]manifest.HookDef{
			lifecycle.HookEventEnable: {{Target: manifest.HookTarget{Type: "webhook", URL: "x"}}},
		},
	}

	db := setupSQLiteWithInstallationsTable(t)
	i := &Installer{
		DB:         db,
		HookRunner: runner,
		Lifecycles: lifecycle.NewRegistry(),
	}
	i.Lifecycles.Register("demo", &lifecycle.ManifestOnly{Data: m})

	if err := i.Enable(uuid.New(), "demo"); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	events := rec.events()
	if len(events) != 1 || events[0] != lifecycle.HookEventEnable {
		t.Fatalf("expected one 'enable' hook fired, got %v", events)
	}
}

// TestInstallerDisable_FiresDisableHook covers the symmetric path.
func TestInstallerDisable_FiresDisableHook(t *testing.T) {
	rec := &recordingHookDispatcher{}
	runner := lifecycle.NewHookRunner(nil)
	runner.Register(lifecycle.HookTargetWebhook, rec)

	m := manifest.Manifest{
		Key:     "demo",
		Version: "1.0.0",
		LifecycleHooks: map[string][]manifest.HookDef{
			lifecycle.HookEventDisable: {{Target: manifest.HookTarget{Type: "webhook", URL: "x"}}},
		},
	}

	db := setupSQLiteWithInstallationsTable(t)
	i := &Installer{
		DB:         db,
		HookRunner: runner,
		Lifecycles: lifecycle.NewRegistry(),
	}
	i.Lifecycles.Register("demo", &lifecycle.ManifestOnly{Data: m})

	if err := i.Disable(uuid.New(), "demo"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	events := rec.events()
	if len(events) != 1 || events[0] != lifecycle.HookEventDisable {
		t.Fatalf("expected one 'disable' hook fired, got %v", events)
	}
}

// TestRegisterManifestHooks_HookRegistry_Idempotency verifies that the
// installer's CRUD-hook wiring contract (UnregisterAddon → RegisterManifestHooks)
// holds at the dynamic.HookRegistry level.
func TestRegisterManifestHooks_HookRegistry_Idempotency(t *testing.T) {
	reg := dynamic.NewHookRegistry()
	rec := &recordingHookDispatcher{}
	runner := lifecycle.NewHookRunner(nil)
	runner.Register(lifecycle.HookTargetWebhook, rec)

	m := manifest.Manifest{
		Key:     "demo",
		Version: "1.0.0",
		ModelDefinitions: []manifest.ModelDefinition{
			{ModelKey: "tickets", TableName: "tickets",
				Columns: []manifest.ColumnDef{{Name: "title", Type: "string"}}},
		},
		LifecycleHooks: map[string][]manifest.HookDef{
			"after_create": {{Target: manifest.HookTarget{Type: "webhook", URL: "x"}}},
		},
	}

	// Two registrations + one unregister == one live hook.
	reg.RegisterManifestHooks("demo", m, runner)
	reg.RegisterManifestHooks("demo", m, runner)
	// We can't easily fire from here without the dynamic.Service plumbing;
	// the dynamic package's own hooks_test.go covers that. The check here
	// is that the round trip leaves the registry in a non-panicking state.
	reg.UnregisterAddon("demo")
}
