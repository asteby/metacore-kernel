package incidents

import (
	"context"
	"fmt"
	"hash/fnv"

	"gorm.io/gorm"
)

// TableIncidents is the incident table, shared by every domain.
const TableIncidents = "platform_incidents"

// Migrate creates the table and indexes if they do not exist. Idempotent and
// safe from every replica at boot: on Postgres the DDL runs under a
// transaction-scoped advisory lock.
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
		return fmt.Errorf("incidents: migrate: %w", err)
	}
	return nil
}

// Schema returns the idempotent DDL for a GORM dialect name ("postgres";
// anything else gets the SQLite flavour).
func Schema(dialect string) []string {
	uuidT, jsonT, tsT, now := "TEXT", "TEXT", "DATETIME", "CURRENT_TIMESTAMP"
	if dialect == "postgres" {
		uuidT, jsonT, tsT, now = "UUID", "JSONB", "TIMESTAMPTZ", "now()"
	}
	return []string{
		`CREATE TABLE IF NOT EXISTS ` + TableIncidents + ` (
			id               ` + uuidT + ` NOT NULL PRIMARY KEY,
			organization_id  ` + uuidT + ` NOT NULL,
			domain           TEXT NOT NULL,
			source           TEXT NOT NULL DEFAULT '',
			kind             TEXT NOT NULL,
			severity         TEXT NOT NULL,
			status           TEXT NOT NULL DEFAULT 'abierta',
			fingerprint      TEXT NOT NULL,
			first_seen_at    ` + tsT + ` NOT NULL DEFAULT ` + now + `,
			last_seen_at     ` + tsT + ` NOT NULL DEFAULT ` + now + `,
			occurrences      INTEGER NOT NULL DEFAULT 1,
			resource_type    TEXT NOT NULL DEFAULT '',
			resource_id      TEXT NOT NULL DEFAULT '',
			meta             ` + jsonT + ` NOT NULL DEFAULT '{}',
			title            TEXT NOT NULL DEFAULT '',
			action_hint      TEXT NOT NULL DEFAULT '',
			detail           TEXT NOT NULL DEFAULT '',
			resolved_by      TEXT NOT NULL DEFAULT '',
			resolved_at      ` + tsT + `,
			resolution       TEXT NOT NULL DEFAULT '',
			evidence_ids     ` + jsonT + ` NOT NULL DEFAULT '[]',
			acknowledged_by  TEXT NOT NULL DEFAULT '',
			acknowledged_at  ` + tsT + `,
			created_at       ` + tsT + ` NOT NULL DEFAULT ` + now + `,
			updated_at       ` + tsT + ` NOT NULL DEFAULT ` + now + `
		)`,
		// One OPEN incident per fingerprint; closed ones are history.
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_` + TableIncidents + `_open ON ` + TableIncidents + ` (organization_id, domain, fingerprint) WHERE status IN ('abierta', 'reconocida')`,
		`CREATE INDEX IF NOT EXISTS idx_` + TableIncidents + `_org ON ` + TableIncidents + ` (organization_id, domain, status, last_seen_at)`,
	}
}

func lockKey() int64 {
	h := fnv.New64a()
	h.Write([]byte("metacore:incidents:migrate:" + TableIncidents))
	return int64(h.Sum64())
}
