package diag

import (
	"context"
	"fmt"
	"hash/fnv"

	"gorm.io/gorm"
)

// Tablas del diagnostico. Aisladas: sin claves foraneas hacia tablas legacy ni
// modelos declarativos (no las exponen /options ni el CRUD generico).
const (
	TableGroups = "addon_diagnostic_groups"
	TableEvents = "addon_diagnostic_events"
)

// Migrate crea las tablas si no existen (idempotente y segura desde cada
// replica al arrancar; en Postgres bajo un advisory lock). La migracion goose
// 0009 hace lo mismo para quien migra por goose.
func Migrate(ctx context.Context, db *gorm.DB) error {
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if tx.Dialector.Name() == "postgres" {
			if err := tx.Exec("SELECT pg_advisory_xact_lock(?)", lockKey()).Error; err != nil {
				return err
			}
		}
		for _, q := range Schema(tx.Dialector.Name()) {
			if err := tx.Exec(q).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("diag: migrate: %w", err)
	}
	return nil
}

// Schema devuelve el DDL idempotente para un dialecto GORM ("postgres"; el resto
// recibe el sabor SQLite de los tests).
func Schema(dialect string) []string {
	// Mismos DEFAULT portables en ambos dialectos (CURRENT_TIMESTAMP, false):
	// la migracion goose 0009 corre tambien contra SQLite en los tests.
	uuidT, jsonT, tsT, now, boolF := "TEXT", "TEXT", "DATETIME", "CURRENT_TIMESTAMP", "false"
	if dialect == "postgres" {
		uuidT, jsonT, tsT = "UUID", "JSONB", "TIMESTAMPTZ"
	}
	boolT := "BOOLEAN"
	return []string{
		`CREATE TABLE IF NOT EXISTS ` + TableGroups + ` (
			id            ` + uuidT + ` NOT NULL PRIMARY KEY,
			org_id        ` + uuidT + ` NOT NULL,
			addon_key     TEXT NOT NULL,
			addon_version TEXT NOT NULL DEFAULT '',
			operation     TEXT NOT NULL DEFAULT '',
			kind          TEXT NOT NULL,
			code          TEXT NOT NULL,
			severity      TEXT NOT NULL DEFAULT 'error',
			http_status   INTEGER NOT NULL DEFAULT 0,
			fingerprint   TEXT NOT NULL,
			count         BIGINT NOT NULL DEFAULT 1,
			first_seen_at ` + tsT + ` NOT NULL DEFAULT ` + now + `,
			last_seen_at  ` + tsT + ` NOT NULL DEFAULT ` + now + `,
			status        TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'muted', 'resolved')),
			last_event_id ` + uuidT + `
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_` + TableGroups + `_fp ON ` + TableGroups + ` (org_id, fingerprint)`,
		`CREATE INDEX IF NOT EXISTS idx_` + TableGroups + `_addon ON ` + TableGroups + ` (org_id, addon_key, last_seen_at)`,
		`CREATE INDEX IF NOT EXISTS idx_` + TableGroups + `_code ON ` + TableGroups + ` (org_id, code)`,
		`CREATE TABLE IF NOT EXISTS ` + TableEvents + ` (
			id             ` + uuidT + ` NOT NULL PRIMARY KEY,
			org_id         ` + uuidT + ` NOT NULL,
			group_id       ` + uuidT + ` NOT NULL,
			correlation_id TEXT NOT NULL DEFAULT '',
			invocation_id  TEXT NOT NULL DEFAULT '',
			diag_ref       TEXT NOT NULL DEFAULT '',
			subject_type   TEXT NOT NULL DEFAULT '',
			subject_id     TEXT NOT NULL DEFAULT '',
			is_first       ` + boolT + ` NOT NULL DEFAULT ` + boolF + `,
			occurred_at    ` + tsT + ` NOT NULL DEFAULT ` + now + `,
			payload        ` + jsonT + ` NOT NULL DEFAULT '{}'
		)`,
		`CREATE INDEX IF NOT EXISTS idx_` + TableEvents + `_group ON ` + TableEvents + ` (org_id, group_id, occurred_at)`,
		`CREATE INDEX IF NOT EXISTS idx_` + TableEvents + `_corr ON ` + TableEvents + ` (org_id, correlation_id)`,
		`CREATE INDEX IF NOT EXISTS idx_` + TableEvents + `_subject ON ` + TableEvents + ` (org_id, subject_type, subject_id)`,
		`CREATE INDEX IF NOT EXISTS idx_` + TableEvents + `_ref ON ` + TableEvents + ` (org_id, diag_ref)`,
		`CREATE INDEX IF NOT EXISTS idx_` + TableEvents + `_occ ON ` + TableEvents + ` (occurred_at)`,
	}
}

func lockKey() int64 {
	h := fnv.New64a()
	h.Write([]byte("metacore:diag:migrate:" + TableGroups))
	return int64(h.Sum64())
}
