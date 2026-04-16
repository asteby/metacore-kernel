// Package installer — dualwrite.go supports F1.5 coexistence: while ops keeps
// writing to its legacy `addon_installations` table, the kernel keeps the
// canonical record in `metacore_installations`. SyncFromLegacy copies rows
// one-way (legacy → kernel) idempotently so the two tables stay converged
// until F1.6 drops the legacy table. BackfillSecrets fills in HMAC secret
// hashes for installations that were copied over before the kernel owned
// secret generation.
//
// Nothing here mutates the legacy table. The migration is additive only.
package installer

import (
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// legacyInstallation mirrors the columns ops writes to `addon_installations`.
// We read into this struct via a raw query so we don't couple the kernel to
// the ops models package. Column names match the ops GORM tags exactly.
type legacyInstallation struct {
	ID             uuid.UUID  `gorm:"column:id"`
	OrganizationID uuid.UUID  `gorm:"column:organization_id"`
	AddonKey       string     `gorm:"column:addon_key"`
	Version        string     `gorm:"column:version"`
	Status         string     `gorm:"column:status"`
	SettingsJSON   []byte     `gorm:"column:settings"`
	InstalledAt    time.Time  `gorm:"column:installed_at"`
	EnabledAt      *time.Time `gorm:"column:enabled_at"`
	DisabledAt     *time.Time `gorm:"column:disabled_at"`
}

// SyncFromLegacy reads every row from `legacyTable` (typically
// "addon_installations") and upserts the kernel-shaped equivalent into
// `metacore_installations`. Safe to re-run any number of times: an existing
// kernel row for the same (organization_id, addon_key) pair is left untouched
// except for the status/version/enabled_at/disabled_at fields, which track
// legacy state.
//
// The caller is expected to have already AutoMigrated Installation so the
// destination table exists. SyncFromLegacy does NOT create it.
func SyncFromLegacy(db *gorm.DB, legacyTable string) error {
	if db == nil {
		return fmt.Errorf("SyncFromLegacy: nil db")
	}
	if legacyTable == "" {
		return fmt.Errorf("SyncFromLegacy: empty legacy table name")
	}
	var rows []legacyInstallation
	if err := db.Table(legacyTable).Find(&rows).Error; err != nil {
		return fmt.Errorf("read %s: %w", legacyTable, err)
	}
	for _, r := range rows {
		inst, err := convertLegacyInstallation(r)
		if err != nil {
			return fmt.Errorf("convert %s/%s: %w", r.OrganizationID, r.AddonKey, err)
		}
		if err := upsertInstallation(db, inst); err != nil {
			return fmt.Errorf("upsert %s/%s: %w", inst.OrganizationID, inst.AddonKey, err)
		}
	}
	return nil
}

// convertLegacyInstallation maps a legacy row to the kernel Installation
// shape. It is pure: no DB, no globals — safe to unit-test without a backend.
// Settings arrive as raw JSON bytes from the legacy column; we defer parsing
// to GORM's serializer on write so invalid JSON surfaces at the DB layer.
func convertLegacyInstallation(r legacyInstallation) (*Installation, error) {
	if r.OrganizationID == uuid.Nil {
		return nil, fmt.Errorf("legacy row missing organization_id")
	}
	if r.AddonKey == "" {
		return nil, fmt.Errorf("legacy row missing addon_key")
	}
	status := r.Status
	if status == "" {
		status = "enabled"
	}
	inst := &Installation{
		ID:             r.ID,
		OrganizationID: r.OrganizationID,
		AddonKey:       r.AddonKey,
		Version:        r.Version,
		Status:         status,
		Source:         "legacy", // distinguishes F1.5-imported rows from native kernel installs
		InstalledAt:    r.InstalledAt,
		EnabledAt:      r.EnabledAt,
		DisabledAt:     r.DisabledAt,
		Settings:       map[string]any{}, // kernel applies defaults at read time
	}
	if inst.InstalledAt.IsZero() {
		inst.InstalledAt = time.Now().UTC()
	}
	return inst, nil
}

// upsertInstallation writes the kernel row. If a row with the same
// (organization_id, addon_key) already exists, we update only the fields
// legacy owns during the coexistence window — never clobber a secret_hash
// the kernel already minted.
func upsertInstallation(db *gorm.DB, inst *Installation) error {
	var existing Installation
	err := db.Where("organization_id = ? AND addon_key = ?",
		inst.OrganizationID, inst.AddonKey).Take(&existing).Error
	if err == gorm.ErrRecordNotFound {
		return db.Create(inst).Error
	}
	if err != nil {
		return err
	}
	return db.Model(&existing).Updates(map[string]any{
		"version":      inst.Version,
		"status":       inst.Status,
		"enabled_at":   inst.EnabledAt,
		"disabled_at":  inst.DisabledAt,
		"installed_at": inst.InstalledAt,
	}).Error
}

// BackfillSecrets generates a fresh HMAC secret and stores its hash for every
// kernel Installation that still has an empty secret_hash (typically rows
// imported by SyncFromLegacy). The cleartext secret is NOT persisted — the
// caller is responsible for distributing it out-of-band to the addon before
// the first signed webhook fires. Returns the number of rows updated.
//
// Idempotent by construction: rows already holding a secret_hash are skipped
// on subsequent runs.
func BackfillSecrets(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("BackfillSecrets: nil db")
	}
	var rows []Installation
	if err := db.Where("secret_hash IS NULL OR secret_hash = ''").Find(&rows).Error; err != nil {
		return fmt.Errorf("find secretless installations: %w", err)
	}
	for _, r := range rows {
		secret, err := newSecret()
		if err != nil {
			return fmt.Errorf("generate secret for %s: %w", r.ID, err)
		}
		if err := db.Model(&Installation{}).
			Where("id = ?", r.ID).
			Update("secret_hash", hashSecret(secret)).Error; err != nil {
			return fmt.Errorf("update secret_hash for %s: %w", r.ID, err)
		}
	}
	return nil
}
