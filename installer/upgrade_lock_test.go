package installer

import (
	"context"
	"errors"
	"testing"

	"github.com/asteby/metacore-kernel/bundle"
	"github.com/asteby/metacore-kernel/lifecycle"
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// midUpgradeLoader simulates the host writing to the installation row while
// Upgrade is busy loading the backend (the long window between the initial
// read and the final persist).
type midUpgradeLoader struct{ mutate func() }

func (l midUpgradeLoader) LoadFromBundle(context.Context, *bundle.Bundle) error {
	l.mutate()
	return nil
}
func (l midUpgradeLoader) UnloadAddon(context.Context, string) error { return nil }

func wasmUpgradeBundle() *bundle.Bundle {
	b := makeBundle("demo", "1.1.0", nil)
	b.Manifest.Backend = &manifest.BackendSpec{Runtime: "wasm", Entry: "backend/backend.wasm", Exports: []string{"OnInstall"}}
	b.Backend = map[string][]byte{"backend/backend.wasm": []byte("fake")}
	return b
}

func lockTestInstaller(db *gorm.DB, l BackendRuntimeLoader) *Installer {
	return &Installer{
		DB: db, KernelVersion: "2.0.0", Lifecycles: lifecycle.NewRegistry(),
		AllowUnsigned: true, schemaApplier: &recordingApplier{},
		Broadcaster: NoopBroadcaster{}, BackendRuntime: l,
	}
}

func TestUpgrade_PreservesConcurrentSettingsAndStatus(t *testing.T) {
	db := upgradeTestDB(t)
	org := uuid.New()
	seedInstallation(t, db, org, "demo", "1.0.0", map[string]any{"keep": "old"})

	loader := midUpgradeLoader{mutate: func() {
		var row Installation
		if err := db.Where("organization_id = ? AND addon_key = ?", org, "demo").Take(&row).Error; err != nil {
			t.Errorf("read: %v", err)
			return
		}
		row.Status = "blocked"
		row.Settings = map[string]any{"keep": "old", "capability": "chosen-during-upgrade"}
		if err := db.Save(&row).Error; err != nil {
			t.Errorf("concurrent save: %v", err)
		}
	}}
	got, err := lockTestInstaller(db, loader).Upgrade(context.Background(), org, wasmUpgradeBundle())
	if err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	var row Installation
	if err := db.Where("organization_id = ? AND addon_key = ?", org, "demo").Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Version != "1.1.0" || row.ManifestHash == "sha256:seed" {
		t.Errorf("upgrade-owned fields not written: version=%q hash=%q", row.Version, row.ManifestHash)
	}
	if row.Status != "blocked" || got.Status != "blocked" {
		t.Errorf("status was reverted: row=%q returned=%q", row.Status, got.Status)
	}
	if row.Settings["capability"] != "chosen-during-upgrade" || row.Settings["keep"] != "old" {
		t.Errorf("concurrent settings lost: %v", row.Settings)
	}
}

func TestUpgrade_RowLockHook_RunsInFinalTxBeforeWrite(t *testing.T) {
	db := upgradeTestDB(t)
	org := uuid.New()
	seedInstallation(t, db, org, "demo", "1.0.0", nil)
	calls := 0
	inst := lockTestInstaller(db, nil).WithRowLockHook(func(tx *gorm.DB, existing *Installation) error {
		calls++
		if existing.OrganizationID != org || existing.AddonKey != "demo" {
			t.Errorf("hook got wrong row: %+v", existing)
		}
		var v string
		if err := tx.Raw(`SELECT version FROM metacore_installations WHERE id = ?`, existing.ID).Scan(&v).Error; err != nil {
			return err
		}
		if v != "1.0.0" {
			t.Errorf("hook must run before the row is written, saw version %q", v)
		}
		return nil
	})
	if _, err := inst.Upgrade(context.Background(), org, makeBundle("demo", "1.1.0", nil)); err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	if calls != 1 {
		t.Fatalf("hook calls = %d, want 1", calls)
	}
}

func TestUpgrade_RowLockHookError_AbortsPersist(t *testing.T) {
	db := upgradeTestDB(t)
	org := uuid.New()
	seedInstallation(t, db, org, "demo", "1.0.0", nil)
	want := errors.New("lock busy")
	inst := lockTestInstaller(db, nil).WithRowLockHook(func(*gorm.DB, *Installation) error { return want })
	if _, err := inst.Upgrade(context.Background(), org, makeBundle("demo", "1.1.0", nil)); !errors.Is(err, want) {
		t.Fatalf("want hook error, got %v", err)
	}
	var row Installation
	db.Where("organization_id = ?", org).Take(&row)
	if row.Version != "1.0.0" {
		t.Errorf("row must stay at 1.0.0, got %q", row.Version)
	}
}
