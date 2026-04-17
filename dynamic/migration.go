package dynamic

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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
	Version   string    `gorm:"size:40;not null;uniqueIndex:idx_addon_ver"`
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
	schema := SchemaName(addonKey, orgID, iso)
	for _, f := range files {
		got := Checksum(f.SQL)
		var existing Migration
		err := db.Where("addon_key = ? AND version = ?", addonKey, f.Version).First(&existing).Error
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
			tx.Rollback()
			return fmt.Errorf("apply %s@%s: %w", addonKey, f.Version, err)
		}
		if err := tx.Create(&Migration{AddonKey: addonKey, Version: f.Version, Checksum: got}).Error; err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit().Error; err != nil {
			return err
		}
	}
	return nil
}

func isNotFound(err error) bool {
	return err != nil && err.Error() == "record not found"
}
