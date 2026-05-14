package installer

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/asteby/metacore-kernel/bundle"
	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/lifecycle"
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// ─── helpers ─────────────────────────────────────────────────────────────

// captureDispatcher records every hook request and optionally returns a
// stubbed error. Local to the upgrade tests so the harness doesn't pull in
// the broader installer-test dependency surface.
type captureDispatcher struct {
	mu    sync.Mutex
	calls []lifecycle.HookRequest
	err   error
}

func (c *captureDispatcher) Dispatch(_ context.Context, req lifecycle.HookRequest) (lifecycle.HookResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, req)
	return lifecycle.HookResponse{}, c.err
}

func (c *captureDispatcher) snapshot() []lifecycle.HookRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]lifecycle.HookRequest, len(c.calls))
	copy(out, c.calls)
	return out
}

// recordingApplier captures invocations so tests can assert that schema
// changes ran in the right order relative to lifecycle hooks. The real
// dynamic.Apply / EnsureSchema / CreateTable family is Postgres-only.
type recordingApplier struct {
	mu     sync.Mutex
	calls  []string
	err    error
	bundle *bundle.Bundle
}

func (r *recordingApplier) ApplyForUpgrade(_ *gorm.DB, _ uuid.UUID, _ dynamic.Isolation, b *bundle.Bundle) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, "ApplyForUpgrade:"+b.Manifest.Version)
	r.bundle = b
	return r.err
}

func (r *recordingApplier) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// upgradeTestDB sets up an in-memory sqlite with the kernel installations
// table and migrations table seeded so countAppliedMigrations works. We can't
// AutoMigrate the kernel structs verbatim because they declare Postgres-only
// defaults (`gen_random_uuid()`) — the raw DDL below mirrors the columns
// Upgrade reads/writes.
func upgradeTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		DisableForeignKeyConstraintWhenMigrating: true,
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
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
		t.Fatalf("create installations: %v", err)
	}
	if err := db.Exec(`CREATE TABLE metacore_addon_migrations (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		addon_key TEXT NOT NULL,
		version TEXT NOT NULL,
		checksum TEXT NOT NULL,
		applied_at DATETIME
	)`).Error; err != nil {
		t.Fatalf("create migrations: %v", err)
	}
	return db
}

// seedInstallation persists an installation row at a given version so
// Upgrade has something to find.
func seedInstallation(t *testing.T, db *gorm.DB, orgID uuid.UUID, key, version string, settings map[string]any) {
	t.Helper()
	row := &Installation{
		ID:             uuid.New(),
		OrganizationID: orgID,
		AddonKey:       key,
		Version:        version,
		Status:         "enabled",
		Source:         "bundle",
		SecretHash:     "sha256:seed",
		Settings:       settings,
		ManifestHash:   "sha256:seed",
	}
	if err := db.Create(row).Error; err != nil {
		t.Fatalf("seed installation: %v", err)
	}
}

// makeBundle returns a minimally-valid bundle the Upgrade flow accepts.
// Signature verification is bypassed by setting AllowUnsigned on the
// caller's Installer.
func makeBundle(key, version string, migrations []dynamic.File) *bundle.Bundle {
	return &bundle.Bundle{
		Manifest: manifest.Manifest{
			Key:         key,
			Name:        key,
			Description: "x",
			Category:    "utility",
			Version:     version,
		},
		Migrations: migrations,
	}
}

// ─── guardUpgradeVersion ─────────────────────────────────────────────────

func TestGuardUpgradeVersion(t *testing.T) {
	cases := []struct {
		name    string
		from    string
		to      string
		wantErr error
	}{
		{"strict greater semver", "1.0.0", "1.1.0", nil},
		{"major bump", "1.5.0", "2.0.0", nil},
		{"same version semver", "1.0.0", "1.0.0", ErrSameVersionUpgrade},
		{"downgrade semver", "1.1.0", "1.0.0", ErrCannotDowngrade},
		{"downgrade major", "2.0.0", "1.9.9", ErrCannotDowngrade},
		{"calver greater (lex fallback)", "2025.10.01", "2025.11.01", nil},
		{"calver same (lex fallback)", "2025.10.01", "2025.10.01", ErrSameVersionUpgrade},
		{"calver downgrade (lex fallback)", "2025.11.01", "2025.10.01", ErrCannotDowngrade},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := guardUpgradeVersion(c.from, c.to)
			if !errors.Is(got, c.wantErr) {
				t.Fatalf("from=%q to=%q: got %v, want %v", c.from, c.to, got, c.wantErr)
			}
		})
	}
}

// ─── mergeSettings ───────────────────────────────────────────────────────

func TestMergeSettings_NewDefaultsAddedUserValuesWin(t *testing.T) {
	old := map[string]any{"existing": "user-value", "shared": "user"}
	newDefaults := map[string]any{"shared": "default-changed", "newKey": true}
	out := mergeSettings(old, newDefaults)
	if out["existing"] != "user-value" {
		t.Errorf("existing key dropped: %v", out)
	}
	if out["shared"] != "user" {
		t.Errorf("user value overwritten by new default: %v", out["shared"])
	}
	if out["newKey"] != true {
		t.Errorf("new default not added: %v", out)
	}
}

// ─── Upgrade end-to-end ──────────────────────────────────────────────────

func TestUpgrade_FiresBeforeAndAfterHookWithRichPayload(t *testing.T) {
	db := upgradeTestDB(t)
	orgID := uuid.New()
	seedInstallation(t, db, orgID, "demo", "1.0.0", map[string]any{"old": "kept"})

	cap := &captureDispatcher{}
	runner := lifecycle.NewHookRunner(nil)
	runner.Register(lifecycle.HookTargetWebhook, cap)

	applier := &recordingApplier{}
	inst := &Installer{
		DB:            db,
		KernelVersion: "2.0.0",
		Lifecycles:    lifecycle.NewRegistry(),
		HookRunner:    runner,
		AllowUnsigned: true,
		schemaApplier: applier,
		Broadcaster:   NoopBroadcaster{},
	}

	newBundle := makeBundle("demo", "1.1.0", nil)
	newBundle.Manifest.LifecycleHooks = map[string][]manifest.HookDef{
		lifecycle.HookEventUpgrade: {{Target: manifest.HookTarget{Type: "webhook", URL: "https://x/upgrade"}}},
	}

	row, err := inst.Upgrade(context.Background(), orgID, newBundle)
	if err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	if row == nil || row.Version != "1.1.0" {
		t.Fatalf("row not updated: %+v", row)
	}
	calls := cap.snapshot()
	if len(calls) != 2 {
		t.Fatalf("expected 2 hook calls (before+after), got %d", len(calls))
	}
	for n, want := range []string{"before", "after"} {
		var p map[string]any
		if err := json.Unmarshal(calls[n].Payload, &p); err != nil {
			t.Fatalf("payload[%d] unmarshal: %v", n, err)
		}
		if p["event"] != lifecycle.HookEventUpgrade {
			t.Errorf("payload[%d].event = %v", n, p["event"])
		}
		if p["from_version"] != "1.0.0" || p["to_version"] != "1.1.0" {
			t.Errorf("payload[%d] versions wrong: %v", n, p)
		}
		if p["phase"] != want {
			t.Errorf("payload[%d].phase = %v, want %q", n, p["phase"], want)
		}
		if p["addon_key"] != "demo" {
			t.Errorf("payload[%d].addon_key = %v", n, p["addon_key"])
		}
	}
	// AFTER payload carries migrations_applied.
	var after map[string]any
	_ = json.Unmarshal(calls[1].Payload, &after)
	if _, ok := after["migrations_applied"]; !ok {
		t.Errorf("after payload missing migrations_applied: %v", after)
	}
	if applier.callCount() != 1 {
		t.Errorf("schema applier called %d times, want 1", applier.callCount())
	}
	// Settings merged: old user value preserved.
	if row.Settings["old"] != "kept" {
		t.Errorf("settings lost user value: %+v", row.Settings)
	}
}

func TestUpgrade_BeforeHookFailure_RollsBackVersion(t *testing.T) {
	db := upgradeTestDB(t)
	orgID := uuid.New()
	seedInstallation(t, db, orgID, "demo", "1.0.0", map[string]any{"old": "kept"})

	cap := &captureDispatcher{err: errors.New("guest vetoed")}
	runner := lifecycle.NewHookRunner(nil)
	runner.Register(lifecycle.HookTargetWebhook, cap)

	applier := &recordingApplier{}
	inst := &Installer{
		DB:            db,
		KernelVersion: "2.0.0",
		Lifecycles:    lifecycle.NewRegistry(),
		HookRunner:    runner,
		AllowUnsigned: true,
		schemaApplier: applier,
		Broadcaster:   NoopBroadcaster{},
	}

	newBundle := makeBundle("demo", "1.1.0", nil)
	newBundle.Manifest.LifecycleHooks = map[string][]manifest.HookDef{
		lifecycle.HookEventUpgrade: {{Target: manifest.HookTarget{Type: "webhook", URL: "https://x/upgrade"}}},
	}

	_, err := inst.Upgrade(context.Background(), orgID, newBundle)
	if err == nil || !strings.Contains(err.Error(), "before") {
		t.Fatalf("expected before-hook failure error, got %v", err)
	}
	if applier.callCount() != 0 {
		t.Errorf("schema applier ran after before-hook veto: %d", applier.callCount())
	}
	// Row stays at 1.0.0.
	var got Installation
	if err := db.Where("organization_id = ? AND addon_key = ?", orgID, "demo").Take(&got).Error; err != nil {
		t.Fatalf("reload row: %v", err)
	}
	if got.Version != "1.0.0" {
		t.Errorf("row version mutated after veto: %v", got.Version)
	}
	if got.Settings["old"] != "kept" {
		t.Errorf("row settings mutated after veto: %v", got.Settings)
	}
}

func TestUpgrade_NoMigrations_BundleSwapOnly(t *testing.T) {
	db := upgradeTestDB(t)
	orgID := uuid.New()
	seedInstallation(t, db, orgID, "demo", "1.0.0", nil)

	applier := &recordingApplier{}
	inst := &Installer{
		DB:            db,
		KernelVersion: "2.0.0",
		Lifecycles:    lifecycle.NewRegistry(),
		AllowUnsigned: true,
		schemaApplier: applier,
		Broadcaster:   NoopBroadcaster{},
	}

	row, err := inst.Upgrade(context.Background(), orgID, makeBundle("demo", "2.0.0", nil))
	if err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	if row.Version != "2.0.0" {
		t.Fatalf("version not bumped: %v", row.Version)
	}
	if applier.callCount() != 1 {
		t.Errorf("applier should still be called even with no migrations (model definitions / schema ensure): %d", applier.callCount())
	}
}

func TestUpgrade_WithNewMigrations_ApplierSeesThem(t *testing.T) {
	db := upgradeTestDB(t)
	orgID := uuid.New()
	seedInstallation(t, db, orgID, "demo", "1.0.0", nil)

	// Record one migration as already applied to mimic the 1.0.0 history.
	if err := db.Create(&dynamic.Migration{
		AddonKey: "demo",
		Version:  "0001_init",
		Checksum: dynamic.Checksum("CREATE TABLE x"),
	}).Error; err != nil {
		t.Fatalf("seed migration: %v", err)
	}

	cap := &captureDispatcher{}
	runner := lifecycle.NewHookRunner(nil)
	runner.Register(lifecycle.HookTargetWebhook, cap)

	applier := &recordingApplier{}
	inst := &Installer{
		DB:            db,
		KernelVersion: "2.0.0",
		Lifecycles:    lifecycle.NewRegistry(),
		HookRunner:    runner,
		AllowUnsigned: true,
		schemaApplier: applier,
		Broadcaster:   NoopBroadcaster{},
	}

	newBundle := makeBundle("demo", "1.1.0", []dynamic.File{
		{Version: "0001_init", SQL: "CREATE TABLE x"},
		{Version: "0002_add_col", SQL: "ALTER TABLE x ADD c INT"},
	})
	newBundle.Manifest.LifecycleHooks = map[string][]manifest.HookDef{
		lifecycle.HookEventUpgrade: {{Target: manifest.HookTarget{Type: "webhook", URL: "https://x"}}},
	}

	if _, err := inst.Upgrade(context.Background(), orgID, newBundle); err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	if applier.bundle == nil || len(applier.bundle.Migrations) != 2 {
		t.Fatalf("applier did not see new migrations: %+v", applier.bundle)
	}
	// AFTER payload should reflect that one (only one) new migration would
	// have been applied — countAppliedMigrations reports 2 candidates minus
	// 1 already-recorded = 1.
	calls := cap.snapshot()
	if len(calls) != 2 {
		t.Fatalf("hook calls = %d", len(calls))
	}
	var after map[string]any
	_ = json.Unmarshal(calls[1].Payload, &after)
	got, _ := after["migrations_applied"].(float64)
	if int(got) != 1 {
		t.Errorf("after payload migrations_applied = %v, want 1", after["migrations_applied"])
	}
}

func TestUpgrade_Downgrade_Refused(t *testing.T) {
	db := upgradeTestDB(t)
	orgID := uuid.New()
	seedInstallation(t, db, orgID, "demo", "2.0.0", nil)

	inst := &Installer{
		DB:            db,
		KernelVersion: "2.0.0",
		Lifecycles:    lifecycle.NewRegistry(),
		AllowUnsigned: true,
		schemaApplier: &recordingApplier{},
		Broadcaster:   NoopBroadcaster{},
	}
	_, err := inst.Upgrade(context.Background(), orgID, makeBundle("demo", "1.5.0", nil))
	if !errors.Is(err, ErrCannotDowngrade) {
		t.Fatalf("want ErrCannotDowngrade, got %v", err)
	}
}

func TestUpgrade_SameVersion_Refused(t *testing.T) {
	db := upgradeTestDB(t)
	orgID := uuid.New()
	seedInstallation(t, db, orgID, "demo", "1.0.0", nil)

	inst := &Installer{
		DB:            db,
		KernelVersion: "2.0.0",
		Lifecycles:    lifecycle.NewRegistry(),
		AllowUnsigned: true,
		schemaApplier: &recordingApplier{},
		Broadcaster:   NoopBroadcaster{},
	}
	_, err := inst.Upgrade(context.Background(), orgID, makeBundle("demo", "1.0.0", nil))
	if !errors.Is(err, ErrSameVersionUpgrade) {
		t.Fatalf("want ErrSameVersionUpgrade, got %v", err)
	}
}

func TestUpgrade_NotInstalled_Refused(t *testing.T) {
	db := upgradeTestDB(t)
	orgID := uuid.New()

	inst := &Installer{
		DB:            db,
		KernelVersion: "2.0.0",
		Lifecycles:    lifecycle.NewRegistry(),
		AllowUnsigned: true,
		schemaApplier: &recordingApplier{},
		Broadcaster:   NoopBroadcaster{},
	}
	_, err := inst.Upgrade(context.Background(), orgID, makeBundle("demo", "1.0.0", nil))
	if !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("want ErrNotInstalled, got %v", err)
	}
}

func TestUpgrade_AfterHookFailure_DoesNotRollBack(t *testing.T) {
	db := upgradeTestDB(t)
	orgID := uuid.New()
	seedInstallation(t, db, orgID, "demo", "1.0.0", nil)

	// Dispatcher returns error every time — applies to BOTH before and
	// after. The before would normally veto; assert that ordering works
	// the other way by using priority and a fail-on-after-only stub.
	cap := &phasedFailDispatcher{failOn: "after"}
	runner := lifecycle.NewHookRunner(nil)
	runner.Register(lifecycle.HookTargetWebhook, cap)

	inst := &Installer{
		DB:            db,
		KernelVersion: "2.0.0",
		Lifecycles:    lifecycle.NewRegistry(),
		HookRunner:    runner,
		AllowUnsigned: true,
		schemaApplier: &recordingApplier{},
		Broadcaster:   NoopBroadcaster{},
	}
	newBundle := makeBundle("demo", "1.1.0", nil)
	newBundle.Manifest.LifecycleHooks = map[string][]manifest.HookDef{
		lifecycle.HookEventUpgrade: {{Target: manifest.HookTarget{Type: "webhook", URL: "https://x"}}},
	}
	row, err := inst.Upgrade(context.Background(), orgID, newBundle)
	if err != nil {
		t.Fatalf("Upgrade: after-hook errors must be swallowed, got %v", err)
	}
	if row.Version != "1.1.0" {
		t.Fatalf("version not bumped despite swallowed after-hook error: %v", row.Version)
	}
}

// phasedFailDispatcher fails only when the payload's `phase` matches the
// configured value. Lets one test reuse a single dispatcher to cover the
// before-success / after-failure ordering.
type phasedFailDispatcher struct {
	failOn string
}

func (p *phasedFailDispatcher) Dispatch(_ context.Context, req lifecycle.HookRequest) (lifecycle.HookResponse, error) {
	var m map[string]any
	_ = json.Unmarshal(req.Payload, &m)
	if m["phase"] == p.failOn {
		return lifecycle.HookResponse{}, errors.New("phased fail")
	}
	return lifecycle.HookResponse{}, nil
}
