package dynamic

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Migration is a versioned SQL file applied to the addon's schema.
// Each migration is identified by (addon_key, version) and locked by checksum
// so tampered files are rejected at apply time.
type Migration struct {
	ID        uint64    `gorm:"primaryKey"`
	AddonKey  string    `gorm:"size:100;not null;uniqueIndex:idx_addon_ver"`
	// Was size:40 — tight enough that a normal descriptive filename
	// ("<n>_<what>_<why>.up", e.g. pos@017_sale_payment_organization_id_tenant_schema)
	// blew past it and aborted the upgrade with SQLSTATE 22001 before running
	// any SQL (see asteby-hq/addons#930). AutoMigrate widens the column on
	// existing installs; no manual ALTER needed.
	Version   string    `gorm:"size:100;not null;uniqueIndex:idx_addon_ver"`
	Checksum  string    `gorm:"size:64;not null"`
	AppliedAt time.Time `gorm:"autoCreateTime"`
}

func (Migration) TableName() string { return "metacore_addon_migrations" }

// File is an unapplied migration candidate loaded from a bundle.
type File struct {
	Version string // e.g. "0001_init"
	SQL     string
}

// Checksum returns the sha256 of a migration file's SQL content.
func Checksum(sql string) string {
	h := sha256.Sum256([]byte(sql))
	return hex.EncodeToString(h[:])
}

// Apply runs each pending migration inside its own transaction, scoped to
// the addon schema. If a migration is already applied it is skipped; if its
// on-disk checksum diverges from what was recorded, Apply returns an error
// instead of silently re-running mutated SQL.
func Apply(db *gorm.DB, addonKey string, orgID uuid.UUID, iso Isolation, files []File) error {
	if err := db.AutoMigrate(&Migration{}); err != nil {
		return fmt.Errorf("migrate metacore_addon_migrations: %w", err)
	}
	// Ledger always lives in public — never follow an addon search_path.
	ledger := db.Table("public.metacore_addon_migrations")
	schema := SchemaName(addonKey, orgID, iso)
	for _, f := range files {
		got := Checksum(f.SQL)
		var existing Migration
		err := ledger.Where("addon_key = ? AND version = ?", addonKey, f.Version).First(&existing).Error
		if err == nil {
			if existing.Checksum != got {
				return fmt.Errorf(
					"migration %s@%s checksum mismatch: recorded %s, file %s (refusing to re-apply mutated SQL)",
					addonKey, f.Version, existing.Checksum, got)
			}
			continue
		}
		if !isNotFound(err) {
			return err
		}
		tx := db.Begin()
		// Scope session search_path so bare table names land in the addon schema.
		if err := tx.Exec(fmt.Sprintf(`SET LOCAL search_path TO %q, public`, schema)).Error; err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Exec(f.SQL).Error; err != nil {
			if !isBenignDDLConflict(err) {
				tx.Rollback()
				return fmt.Errorf("apply %s@%s: %w", addonKey, f.Version, err)
			}
			// Postgres aborts the whole tx after any error (SQLSTATE 25P02).
			// Objects already exist from a partial prior run — roll back and
			// record the ledger row in a fresh transaction (always public).
			tx.Rollback()
			if err := recordMigration(ledger, addonKey, f.Version, got); err != nil {
				return err
			}
			continue
		}
		// Reset search_path before writing the ledger so a migration file that
		// issued SET search_path (non-LOCAL) cannot redirect the INSERT into
		// addon_<key>.metacore_addon_migrations (local-dev 23505 on retry).
		if err := tx.Exec(`SET LOCAL search_path TO public`).Error; err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Table("public.metacore_addon_migrations").Create(&Migration{AddonKey: addonKey, Version: f.Version, Checksum: got}).Error; err != nil {
			tx.Rollback()
			if isBenignDDLConflict(err) {
				// Concurrent retry / already recorded in public — treat as applied.
				continue
			}
			return err
		}
		if err := tx.Commit().Error; err != nil {
			return err
		}
	}
	return nil
}

func recordMigration(ledger *gorm.DB, addonKey, version, checksum string) error {
	err := ledger.Create(&Migration{AddonKey: addonKey, Version: version, Checksum: checksum}).Error
	if err != nil && isBenignDDLConflict(err) {
		return nil
	}
	return err
}

func isNotFound(err error) bool {
	return err != nil && err.Error() == "record not found"
}

// isBenignDDLConflict reports Postgres "already exists" errors during addon
// migrations. Local dev often retries a partially-applied install (hub
// reinstall, air restart) leaving tables/indexes behind while the migration
// ledger row is missing — treating these as applied keeps onboarding unblocked.
func isBenignDDLConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "already exists") ||
		strings.Contains(msg, "duplicate key") ||
		strings.Contains(msg, "sqlstate 42p07") ||
		strings.Contains(msg, "sqlstate 42710") ||
		errors.Is(err, gorm.ErrDuplicatedKey)
}
