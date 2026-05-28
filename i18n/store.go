// store.go persists addon-level i18n catalogs in a kernel-owned table so
// every host (ops, link, future apps) inherits the same registration path
// instead of reimplementing it (ops historically duplicated this in
// services/addonbundle/loader.go — that workaround now collapses into the
// kernel; consumers call kernel.GetAddonI18n directly).
//
// Shape:
//
//	metacore_addon_i18n(addon_key, lang, key, value)
//	    PRIMARY KEY (addon_key, lang, key)
//
// The kernel persists three sources of i18n strings, in this order of
// authority (later layer wins on collision):
//
//   1. `manifest.I18n` — the legacy per-app string bundle ({lang→{key→val}}).
//      Carries the addon's UI labels.
//   2. `manifest.MetadataI18n` — the v3 marketplace catalog block
//      ({lang→{name|description|features}}). We unfold those three fields
//      into deterministic dotted keys ("catalog.name", "catalog.description",
//      "catalog.features.<n>") so consumers can look them up like any other
//      string and the metadata localizer pipeline doesn't need a new shape.
//
// Reads are tenant-blind: i18n is process-wide. A host that wants
// per-tenant copy overrides layers that on top via its own translator.
package i18n

import (
	"context"
	"fmt"
	"strconv"

	"github.com/asteby/metacore-kernel/manifest"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// AddonI18nString is the persisted row. One row per (addon, lang, key).
type AddonI18nString struct {
	AddonKey string `gorm:"size:100;not null;primaryKey;column:addon_key"`
	Lang     string `gorm:"size:16;not null;primaryKey;column:lang"`
	Key      string `gorm:"size:200;not null;primaryKey;column:key"`
	Value    string `gorm:"type:text;not null;column:value"`
}

// TableName pins the kernel-owned table name.
func (AddonI18nString) TableName() string { return "metacore_addon_i18n" }

// EnsureSchema runs AutoMigrate for the persisted catalog. Idempotent —
// safe to call on every host boot.
func EnsureSchema(db *gorm.DB) error {
	if db == nil {
		return nil
	}
	return db.AutoMigrate(&AddonI18nString{})
}

// RegisterAddonI18n persists the addon's i18n catalog into the kernel-owned
// table. It is the single entry point hosts (and the installer) call after
// validating a manifest. Re-installs are idempotent: rows are upserted on
// the (addon_key, lang, key) composite primary key, so re-running RegisterAddonI18n
// with the same input is a no-op on disk, and a changed value overwrites.
//
// Pre-existing rows for the addon that no longer appear in the new catalog
// are PURGED so an upgrade can drop a translation that was removed in the
// new version. Without that step a renamed/removed key would linger forever.
//
// A nil DB short-circuits to nil (hosts that have not wired persistence yet,
// e.g. early test bootstrap). An empty catalog deletes any prior rows for
// the addon — useful when an upgrade strips i18n entirely.
func RegisterAddonI18n(ctx context.Context, db *gorm.DB, addonKey string, m manifest.Manifest) error {
	if db == nil || addonKey == "" {
		return nil
	}
	if err := EnsureSchema(db); err != nil {
		return fmt.Errorf("i18n.RegisterAddonI18n: ensure schema: %w", err)
	}
	rows := buildI18nRows(addonKey, m)

	tx := db.WithContext(ctx).Begin()
	if tx.Error != nil {
		return fmt.Errorf("i18n.RegisterAddonI18n: begin tx: %w", tx.Error)
	}
	// Purge stale rows first so removed keys disappear; then upsert the new
	// set. We can't rely on DELETE + INSERT because the kernel runs against
	// both Postgres and sqlite-for-tests — UPSERT on the PK is the portable
	// idiom both support.
	if err := tx.Where("addon_key = ?", addonKey).Delete(&AddonI18nString{}).Error; err != nil {
		tx.Rollback()
		return fmt.Errorf("i18n.RegisterAddonI18n: purge stale: %w", err)
	}
	if len(rows) > 0 {
		// Upsert keyed on the composite PK. ON CONFLICT works on both
		// Postgres and sqlite >= 3.24.
		if err := tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{
				{Name: "addon_key"},
				{Name: "lang"},
				{Name: "key"},
			},
			DoUpdates: clause.AssignmentColumns([]string{"value"}),
		}).CreateInBatches(rows, 200).Error; err != nil {
			tx.Rollback()
			return fmt.Errorf("i18n.RegisterAddonI18n: upsert rows: %w", err)
		}
	}
	return tx.Commit().Error
}

// GetAddonI18n returns the merged catalog for the given lang across ALL
// addons in the kernel — flat map of dotted key → translated value. The
// caller (frontend translator) decides what to do with key collisions
// between addons; the kernel does NOT prefix the addon key into the
// returned map because the manifest authors already namespace their
// strings (e.g. "tires.products.title").
//
// An empty lang returns an empty map without error — callers that don't
// know the user language yet just get nothing localized instead of a
// random fallback (the metadata localizer handles fallback chains
// elsewhere).
func GetAddonI18n(ctx context.Context, db *gorm.DB, lang string) (map[string]string, error) {
	if db == nil || lang == "" {
		return map[string]string{}, nil
	}
	var rows []AddonI18nString
	if err := db.WithContext(ctx).
		Where("lang = ?", lang).
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("i18n.GetAddonI18n: query: %w", err)
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[r.Key] = r.Value
	}
	return out, nil
}

// GetAddonI18nFor returns just one addon's catalog for the given lang. Same
// shape as GetAddonI18n but scoped to addonKey.
func GetAddonI18nFor(ctx context.Context, db *gorm.DB, addonKey, lang string) (map[string]string, error) {
	if db == nil || addonKey == "" || lang == "" {
		return map[string]string{}, nil
	}
	var rows []AddonI18nString
	if err := db.WithContext(ctx).
		Where("addon_key = ? AND lang = ?", addonKey, lang).
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("i18n.GetAddonI18nFor: query: %w", err)
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[r.Key] = r.Value
	}
	return out, nil
}

// UnregisterAddonI18n drops every row for the addon. Called by the
// installer's Uninstall path so a removed addon's strings stop surfacing.
// Idempotent.
func UnregisterAddonI18n(ctx context.Context, db *gorm.DB, addonKey string) error {
	if db == nil || addonKey == "" {
		return nil
	}
	return db.WithContext(ctx).
		Where("addon_key = ?", addonKey).
		Delete(&AddonI18nString{}).Error
}

// buildI18nRows lowers the manifest's two i18n sources into the persisted
// row shape. Pure helper — no DB access — so unit tests can lock the
// projection without spinning up a database.
//
// Projection rules:
//
//   - manifest.I18n[lang][key] = value → (addon_key, lang, key, value)
//   - manifest.MetadataI18n[lang].Name → (addon_key, lang, "catalog.name", val)
//   - manifest.MetadataI18n[lang].Description → "catalog.description"
//   - manifest.MetadataI18n[lang].Features[i] → "catalog.features.<i>"
//
// MetadataI18n keys land AFTER the manifest.I18n keys in the slice but the
// upsert path treats both as equally authoritative — a manifest can
// override a catalog string by declaring the same dotted key in I18n.
func buildI18nRows(addonKey string, m manifest.Manifest) []AddonI18nString {
	var rows []AddonI18nString
	// 1) plain manifest.I18n — already in {lang: {key: val}} shape.
	for lang, kv := range m.I18n {
		if lang == "" {
			continue
		}
		for k, v := range kv {
			if k == "" {
				continue
			}
			rows = append(rows, AddonI18nString{
				AddonKey: addonKey,
				Lang:     lang,
				Key:      k,
				Value:    v,
			})
		}
	}
	// 2) marketplace catalog block (v3 metadata.i18n). Unfold name /
	// description / features[i] into deterministic dotted keys so the
	// existing flat-key translator surface picks them up without a new
	// codepath.
	for lang, loc := range m.MetadataI18n {
		if lang == "" {
			continue
		}
		if loc.Name != "" {
			rows = append(rows, AddonI18nString{
				AddonKey: addonKey,
				Lang:     lang,
				Key:      "catalog.name",
				Value:    loc.Name,
			})
		}
		if loc.Description != "" {
			rows = append(rows, AddonI18nString{
				AddonKey: addonKey,
				Lang:     lang,
				Key:      "catalog.description",
				Value:    loc.Description,
			})
		}
		for i, f := range loc.Features {
			if f == "" {
				continue
			}
			rows = append(rows, AddonI18nString{
				AddonKey: addonKey,
				Lang:     lang,
				Key:      "catalog.features." + strconv.Itoa(i),
				Value:    f,
			})
		}
	}
	return rows
}
