package outbox

import (
	"context"
	"fmt"
	"hash/fnv"

	"gorm.io/gorm"
)

// TableName is the table the outbox owns.
const TableName = "metacore_outbox_jobs"

// Migrate creates metacore_outbox_jobs and its indexes if they do not
// exist. It is idempotent and safe to run from every replica at boot: on
// Postgres the DDL runs under a transaction-scoped advisory lock.
//
// New calls it unless Config.SkipMigrate is set; host.NewApp calls it when
// AppConfig.EnableOutbox is true. Hosts that own their schema through
// their own migration tool can copy the DDL from Schema.
func Migrate(ctx context.Context, db *gorm.DB) error {
	pg := isPostgres(db)
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if pg {
			if err := tx.Exec("SELECT pg_advisory_xact_lock(?)", advisoryKey(TableName)).Error; err != nil {
				return err
			}
		}
		for _, q := range Schema(db.Dialector.Name()) {
			if err := tx.Exec(q).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("outbox: migrate %s: %w", TableName, err)
	}
	return nil
}

// Schema returns the idempotent DDL statements for the given GORM dialect
// name ("postgres"; anything else gets the SQLite flavour).
func Schema(dialect string) []string {
	uuidT, jsonT, tsT, now := "TEXT", "TEXT", "DATETIME", "CURRENT_TIMESTAMP"
	if dialect == "postgres" {
		uuidT, jsonT, tsT, now = "UUID", "JSONB", "TIMESTAMPTZ", "now()"
	}
	return []string{
		`CREATE TABLE IF NOT EXISTS ` + TableName + ` (
			id           ` + uuidT + ` NOT NULL PRIMARY KEY,
			org_id       ` + uuidT + `,
			kind         TEXT    NOT NULL,
			payload      ` + jsonT + ` NOT NULL,
			status       TEXT    NOT NULL DEFAULT 'pending',
			attempts     INTEGER NOT NULL DEFAULT 0,
			max_attempts INTEGER NOT NULL DEFAULT 10,
			run_at       ` + tsT + ` NOT NULL DEFAULT ` + now + `,
			last_error   TEXT    NOT NULL DEFAULT '',
			dedupe_key   TEXT,
			locked_until ` + tsT + `,
			locked_by    TEXT,
			created_at   ` + tsT + ` NOT NULL DEFAULT ` + now + `,
			updated_at   ` + tsT + ` NOT NULL DEFAULT ` + now + `,
			CONSTRAINT ` + TableName + `_status_check CHECK (status IN ('pending', 'running', 'done', 'dead'))
		)`,
		// Claim path: due pending jobs, oldest first.
		`CREATE INDEX IF NOT EXISTS idx_` + TableName + `_due ON ` + TableName + ` (run_at) WHERE status = 'pending'`,
		// Stuck-job recovery: running jobs by lease expiry.
		`CREATE INDEX IF NOT EXISTS idx_` + TableName + `_lease ON ` + TableName + ` (locked_until) WHERE status = 'running'`,
		// Dedupe: one live job per (kind, dedupe_key).
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_` + TableName + `_dedupe ON ` + TableName +
			` (kind, dedupe_key) WHERE dedupe_key IS NOT NULL AND status IN ('pending', 'running')`,
		// Admin listing and retention cleanup.
		`CREATE INDEX IF NOT EXISTS idx_` + TableName + `_org_status ON ` + TableName + ` (org_id, status, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_` + TableName + `_status_updated ON ` + TableName + ` (status, updated_at)`,
	}
}

func isPostgres(db *gorm.DB) bool { return db.Dialector.Name() == "postgres" }

// advisoryKey maps a table name to a stable pg_advisory_xact_lock key.
func advisoryKey(name string) int64 {
	h := fnv.New64a()
	h.Write([]byte("metacore:migrate:" + name))
	return int64(h.Sum64())
}
