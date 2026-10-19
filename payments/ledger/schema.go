package ledger

import (
	"context"
	"fmt"
	"hash/fnv"

	"gorm.io/gorm"
)

// Tables the ledger owns. The commerce_ prefix keeps them apart from the
// receivables `payments` table of the customers addon.
const (
	TablePayments = "commerce_payments"
	TableEvents   = "commerce_payment_events"
	TableRefunds  = "commerce_refunds"
	TableHolds    = "commerce_payment_holds"
	TableLeases   = "commerce_payment_reconcile_leases"
)

// Migrate creates the ledger tables and indexes if they do not exist. It is
// idempotent and safe from every replica at boot: on Postgres the DDL runs
// under a transaction-scoped advisory lock. New calls it unless
// Config.SkipMigrate; hosts that own their schema copy the DDL from Schema.
func Migrate(ctx context.Context, db *gorm.DB) error {
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if isPostgres(tx) {
			if err := tx.Exec("SELECT pg_advisory_xact_lock(?)", lockKey("migrate:"+TablePayments)).Error; err != nil {
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
		return fmt.Errorf("payments ledger: migrate: %w", err)
	}
	return nil
}

// Schema returns the idempotent DDL for a GORM dialect name ("postgres";
// anything else gets the SQLite flavour). Money is NUMERIC on Postgres and
// TEXT on SQLite; the ledger always reads it back as an exact decimal.
func Schema(dialect string) []string {
	uuidT, jsonT, tsT, moneyT, boolT, now := "TEXT", "TEXT", "DATETIME", "TEXT", "BOOLEAN", "CURRENT_TIMESTAMP"
	if dialect == "postgres" {
		uuidT, jsonT, tsT, moneyT, now = "UUID", "JSONB", "TIMESTAMPTZ", "NUMERIC(20,6)", "now()"
	}
	open := `('pending', 'requires_action', 'at_device', 'authorized', 'cancelling')`
	return []string{
		`CREATE TABLE IF NOT EXISTS ` + TablePayments + ` (
			id              ` + uuidT + ` NOT NULL PRIMARY KEY,
			organization_id ` + uuidT + ` NOT NULL,
			document_type   TEXT NOT NULL,
			document_id     TEXT NOT NULL,
			order_ref       TEXT NOT NULL DEFAULT '',
			channel         TEXT NOT NULL,
			provider        TEXT NOT NULL,
			provider_ref    TEXT,
			session_id      TEXT NOT NULL DEFAULT '',
			device_id       TEXT NOT NULL DEFAULT '',
			idempotency_key TEXT NOT NULL,
			status          TEXT NOT NULL,
			amount          ` + moneyT + ` NOT NULL,
			currency        TEXT NOT NULL,
			document_total  ` + moneyT + `,
			amount_refunded ` + moneyT + ` NOT NULL DEFAULT 0,
			fee             ` + moneyT + `,
			net             ` + moneyT + `,
			method          TEXT NOT NULL DEFAULT '',
			installments    INTEGER NOT NULL DEFAULT 0,
			card            ` + jsonT + `,
			cash            ` + jsonT + `,
			seller          ` + jsonT + `,
			test_mode       ` + boolT + ` NOT NULL DEFAULT FALSE,
			status_detail   TEXT NOT NULL DEFAULT '',
			anomaly         TEXT NOT NULL DEFAULT '',
			anomaly_status  TEXT NOT NULL DEFAULT '',
			created_by      TEXT NOT NULL DEFAULT '',
			expires_at      ` + tsT + `,
			last_checked_at ` + tsT + `,
			paid_at         ` + tsT + `,
			created_at      ` + tsT + ` NOT NULL DEFAULT ` + now + `,
			updated_at      ` + tsT + ` NOT NULL DEFAULT ` + now + `
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_` + TablePayments + `_key ON ` + TablePayments + ` (organization_id, provider, idempotency_key)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_` + TablePayments + `_ref ON ` + TablePayments + ` (organization_id, provider, provider_ref) WHERE provider_ref IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_` + TablePayments + `_doc ON ` + TablePayments + ` (organization_id, document_type, document_id)`,
		`CREATE INDEX IF NOT EXISTS idx_` + TablePayments + `_open ON ` + TablePayments + ` (organization_id, last_checked_at) WHERE status IN ` + open,
		`CREATE INDEX IF NOT EXISTS idx_` + TablePayments + `_anomaly ON ` + TablePayments + ` (organization_id) WHERE anomaly <> ''`,

		`CREATE TABLE IF NOT EXISTS ` + TableEvents + ` (
			id              ` + uuidT + ` NOT NULL PRIMARY KEY,
			organization_id ` + uuidT + ` NOT NULL,
			payment_id      ` + uuidT + `,
			provider        TEXT NOT NULL,
			event_id        TEXT NOT NULL,
			kind            TEXT NOT NULL,
			source          TEXT NOT NULL,
			status          TEXT NOT NULL,
			decision        TEXT NOT NULL,
			reason          TEXT NOT NULL DEFAULT '',
			outcome         ` + jsonT + `,
			observed_at     ` + tsT + ` NOT NULL DEFAULT ` + now + `
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_` + TableEvents + `_dedupe ON ` + TableEvents + ` (organization_id, provider, event_id)`,
		`CREATE INDEX IF NOT EXISTS idx_` + TableEvents + `_payment ON ` + TableEvents + ` (payment_id, observed_at)`,

		`CREATE TABLE IF NOT EXISTS ` + TableRefunds + ` (
			id                   ` + uuidT + ` NOT NULL PRIMARY KEY,
			organization_id      ` + uuidT + ` NOT NULL,
			payment_id           ` + uuidT + ` NOT NULL,
			destination          TEXT NOT NULL,
			provider             TEXT NOT NULL DEFAULT '',
			provider_refund_id   TEXT NOT NULL DEFAULT '',
			amount               ` + moneyT + ` NOT NULL,
			currency             TEXT NOT NULL,
			status               TEXT NOT NULL,
			reason               TEXT NOT NULL DEFAULT '',
			idempotency_key      TEXT NOT NULL,
			source_type          TEXT NOT NULL DEFAULT '',
			source_id            TEXT NOT NULL DEFAULT '',
			credit_note_required ` + boolT + ` NOT NULL DEFAULT FALSE,
			adjusts_total        ` + boolT + ` NOT NULL DEFAULT FALSE,
			credit_note_id       TEXT NOT NULL DEFAULT '',
			cash_session_id      TEXT NOT NULL DEFAULT '',
			instructions         TEXT NOT NULL DEFAULT '',
			created_by           TEXT NOT NULL DEFAULT '',
			settled_at           ` + tsT + `,
			created_at           ` + tsT + ` NOT NULL DEFAULT ` + now + `,
			updated_at           ` + tsT + ` NOT NULL DEFAULT ` + now + `
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_` + TableRefunds + `_key ON ` + TableRefunds + ` (organization_id, payment_id, idempotency_key)`,

		`CREATE TABLE IF NOT EXISTS ` + TableHolds + ` (
			id              ` + uuidT + ` NOT NULL PRIMARY KEY,
			organization_id ` + uuidT + ` NOT NULL,
			document_type   TEXT NOT NULL,
			document_id     TEXT NOT NULL,
			token_hash      TEXT NOT NULL,
			holder          TEXT NOT NULL,
			channel         TEXT NOT NULL DEFAULT '',
			user_id         TEXT NOT NULL DEFAULT '',
			status          TEXT NOT NULL,
			created_by      TEXT NOT NULL DEFAULT '',
			expires_at      ` + tsT + ` NOT NULL,
			created_at      ` + tsT + ` NOT NULL DEFAULT ` + now + `,
			updated_at      ` + tsT + ` NOT NULL DEFAULT ` + now + `
		)`,
		// One live hold per document, whatever the point of sale.
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_` + TableHolds + `_active ON ` + TableHolds + ` (organization_id, document_type, document_id) WHERE status = 'active'`,

		`CREATE TABLE IF NOT EXISTS ` + TableLeases + ` (
			organization_id ` + uuidT + ` NOT NULL PRIMARY KEY,
			locked_by       TEXT NOT NULL DEFAULT '',
			locked_until    ` + tsT + `,
			last_run_at     ` + tsT + `,
			last_report     ` + jsonT + `
		)`,
	}
}

func isPostgres(db *gorm.DB) bool { return db.Dialector.Name() == "postgres" }

// lockKey maps a name to a stable pg_advisory_xact_lock key.
func lockKey(name string) int64 {
	h := fnv.New64a()
	h.Write([]byte("metacore:payments:" + name))
	return int64(h.Sum64())
}
