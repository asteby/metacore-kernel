package installer

import (
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// TestEnsureInstallationUniqueIndex reproduces the prod failure: a
// metacore_installations table that exists but carries no unique index over
// (organization_id, addon_key). Before the fix the ON CONFLICT upsert raised
// 42P10 ("no unique or exclusion constraint matching the ON CONFLICT
// specification"). ensureInstallationUniqueIndex must create the index so the
// upsert succeeds and stays idempotent across re-installs.
func TestEnsureInstallationUniqueIndex(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		DisableForeignKeyConstraintWhenMigrating: true,
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	// Table WITHOUT the composite unique index — the prod state.
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

	// Sanity: a conflict-target upsert must fail before the index exists.
	org := uuid.New()
	upsert := func() error {
		return db.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "organization_id"}, {Name: "addon_key"}},
			DoUpdates: clause.AssignmentColumns([]string{"version"}),
		}).Create(&Installation{
			ID: uuid.New(), OrganizationID: org, AddonKey: "acme", Version: "1.0.0", Status: "enabled", Source: "bundle",
		}).Error
	}
	if err := upsert(); err == nil {
		t.Fatal("expected upsert to fail before the unique index exists")
	}

	// Apply the fix.
	if err := ensureInstallationUniqueIndex(db); err != nil {
		t.Fatalf("ensureInstallationUniqueIndex: %v", err)
	}

	// Now the upsert works…
	if err := upsert(); err != nil {
		t.Fatalf("upsert after ensure: %v", err)
	}
	// …and is idempotent: a second upsert for the same (org, addon) updates in
	// place rather than inserting a duplicate.
	if err := db.Model(&Installation{}).Where("organization_id = ? AND addon_key = ?", org, "acme").
		Update("version", "2.0.0").Error; err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := upsert(); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	var count int64
	db.Model(&Installation{}).Where("organization_id = ? AND addon_key = ?", org, "acme").Count(&count)
	if count != 1 {
		t.Fatalf("expected exactly 1 row after upserts, got %d", count)
	}

	// Idempotent: calling ensure again is a no-op (does not error).
	if err := ensureInstallationUniqueIndex(db); err != nil {
		t.Fatalf("second ensure should be a no-op: %v", err)
	}
}
