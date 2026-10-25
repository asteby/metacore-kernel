package installer

// Real-Postgres proof for the Upgrade lost-update fix. Skipped without
// TEST_POSTGRES_DSN.

import (
	"context"
	"errors"
	"testing"

	"github.com/asteby/metacore-kernel/bundle"
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// The host changes status and settings (a capability selection, a wasm block)
// while Upgrade is loading the backend, i.e. after Upgrade read the row and
// before it persists. Without the fix the final full-row Save reverts both.
func TestUpgrade_PG_ConcurrentHostWriteSurvives(t *testing.T) {
	db, key, _, _, m := freshInstallFixture(t)
	org := uuid.New()
	inst := freshInstaller(db)
	if _, _, err := inst.Install(org, &bundle.Bundle{Manifest: m}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	inst.BackendRuntime = midUpgradeLoader{mutate: func() {
		err := db.Transaction(func(tx *gorm.DB) error {
			var row Installation
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("organization_id = ? AND addon_key = ?", org, key).Take(&row).Error; err != nil {
				return err
			}
			row.Status = "blocked"
			row.Settings = map[string]any{"capability": "chosen"}
			return tx.Select("Status", "Settings").Save(&row).Error
		})
		if err != nil {
			t.Errorf("host tx: %v", err)
		}
	}}

	b2 := &bundle.Bundle{Manifest: m}
	b2.Manifest.Version = "1.1.0"
	b2.Manifest.Backend = &manifest.BackendSpec{Runtime: "wasm", Entry: "backend/backend.wasm", Exports: []string{"OnInstall"}}
	b2.Backend = map[string][]byte{"backend/backend.wasm": []byte("fake")}
	if _, err := inst.Upgrade(context.Background(), org, b2); err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	var row Installation
	if err := db.Where("organization_id = ? AND addon_key = ?", org, key).Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Version != "1.1.0" {
		t.Errorf("version = %q", row.Version)
	}
	if row.Status != "blocked" || row.Settings["capability"] != "chosen" {
		t.Errorf("host write lost: status=%q settings=%v", row.Status, row.Settings)
	}
}

// A competing upgrade to 1.2.0 lands while this one (1.1.0) loads the backend:
// the late one must fail instead of moving the version back.
func TestUpgrade_PG_ConcurrentHigherVersionNotRolledBack(t *testing.T) {
	db, key, _, _, m := freshInstallFixture(t)
	org := uuid.New()
	inst := freshInstaller(db)
	if _, _, err := inst.Install(org, &bundle.Bundle{Manifest: m}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	inst.BackendRuntime = midUpgradeLoader{mutate: func() {
		if err := db.Model(&Installation{}).Where("organization_id = ? AND addon_key = ?", org, key).
			Update("version", "1.2.0").Error; err != nil {
			t.Errorf("competing upgrade: %v", err)
		}
	}}
	b2 := &bundle.Bundle{Manifest: m}
	b2.Manifest.Version = "1.1.0"
	b2.Manifest.Backend = &manifest.BackendSpec{Runtime: "wasm", Entry: "backend/backend.wasm", Exports: []string{"OnInstall"}}
	b2.Backend = map[string][]byte{"backend/backend.wasm": []byte("fake")}
	if _, err := inst.Upgrade(context.Background(), org, b2); !errors.Is(err, ErrCannotDowngrade) {
		t.Fatalf("want ErrCannotDowngrade, got %v", err)
	}
	var row Installation
	db.Where("organization_id = ? AND addon_key = ?", org, key).Take(&row)
	if row.Version != "1.2.0" {
		t.Errorf("version regressed to %q", row.Version)
	}
}
