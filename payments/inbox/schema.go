package inbox

import (
	"context"
	"fmt"
	"hash/fnv"

	"gorm.io/gorm"
)

// Tables the inbox owns.
const (
	TableNotifications = "commerce_payment_notifications"
	TableAttempts      = "commerce_payment_notification_attempts"
)

// Migrate creates the tables, indexes and (Postgres) the append-only guard if
// they do not exist. Idempotent and safe from every replica at boot: the DDL
// runs in one transaction under an advisory lock.
func Migrate(ctx context.Context, db *gorm.DB) error {
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		pg := tx.Dialector.Name() == "postgres"
		if pg {
			if err := tx.Exec("SELECT pg_advisory_xact_lock(?)", lockKey("migrate:"+TableNotifications)).Error; err != nil {
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
		return fmt.Errorf("payments inbox: migrate: %w", err)
	}
	return nil
}

// Schema returns the idempotent DDL for a GORM dialect name.
func Schema(dialect string) []string {
	uuidT, jsonT, tsT, blobT, boolT, now := "TEXT", "TEXT", "DATETIME", "BLOB", "BOOLEAN", "CURRENT_TIMESTAMP"
	if dialect == "postgres" {
		uuidT, jsonT, tsT, blobT, now = "UUID", "JSONB", "TIMESTAMPTZ", "BYTEA", "now()"
	}
	q := []string{
		`CREATE TABLE IF NOT EXISTS ` + TableNotifications + ` (
			id                 ` + uuidT + ` NOT NULL PRIMARY KEY,
			organization_id    ` + uuidT + ` NOT NULL,
			provider           TEXT NOT NULL,
			route              TEXT NOT NULL DEFAULT '',
			received_at        ` + tsT + ` NOT NULL DEFAULT ` + now + `,
			source_ip          TEXT NOT NULL DEFAULT '',
			method             TEXT NOT NULL DEFAULT 'POST',
			query              ` + jsonT + ` NOT NULL DEFAULT '{}',
			headers            ` + jsonT + ` NOT NULL DEFAULT '{}',
			body_raw           ` + blobT + `,
			body_sha256        TEXT NOT NULL DEFAULT '',
			body_truncated     ` + boolT + ` NOT NULL DEFAULT FALSE,
			body_redacted      ` + jsonT + ` NOT NULL DEFAULT '{}',
			resource_kind      TEXT NOT NULL DEFAULT '',
			resource_id        TEXT NOT NULL DEFAULT '',
			event_id           TEXT NOT NULL DEFAULT '',
			auth               TEXT NOT NULL DEFAULT 'pendiente',
			auth_reason        TEXT NOT NULL DEFAULT '',
			estado             TEXT NOT NULL DEFAULT 'recibida',
			motivo             TEXT NOT NULL DEFAULT '',
			intentos           INTEGER NOT NULL DEFAULT 0,
			proximo_intento_at ` + tsT + `,
			procesada_at       ` + tsT + `,
			payment_id         ` + uuidT + `,
			checkout_ref       TEXT NOT NULL DEFAULT '',
			incident_id        ` + uuidT + `,
			replay_of          ` + uuidT + `,
			dedupe_key         TEXT NOT NULL DEFAULT '',
			purged_at          ` + tsT + `,
			updated_at         ` + tsT + ` NOT NULL DEFAULT ` + now + `
		)`,
		`CREATE INDEX IF NOT EXISTS idx_` + TableNotifications + `_recv ON ` + TableNotifications + ` (organization_id, received_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_` + TableNotifications + `_queue ON ` + TableNotifications + ` (organization_id, provider, estado) WHERE estado IN ('recibida', 'reintentar', 'requiere_revision', 'dead_letter')`,
		`CREATE INDEX IF NOT EXISTS idx_` + TableNotifications + `_res ON ` + TableNotifications + ` (organization_id, provider, resource_id)`,
		`CREATE INDEX IF NOT EXISTS idx_` + TableNotifications + `_dedupe ON ` + TableNotifications + ` (organization_id, dedupe_key, received_at)`,
		`CREATE INDEX IF NOT EXISTS idx_` + TableNotifications + `_retry ON ` + TableNotifications + ` (proximo_intento_at) WHERE estado = 'reintentar'`,

		`CREATE TABLE IF NOT EXISTS ` + TableAttempts + ` (
			notification_id ` + uuidT + ` NOT NULL,
			n               INTEGER NOT NULL,
			organization_id ` + uuidT + ` NOT NULL,
			started_at      ` + tsT + ` NOT NULL DEFAULT ` + now + `,
			ended_at        ` + tsT + `,
			resultado       TEXT NOT NULL DEFAULT '',
			motivo          TEXT NOT NULL DEFAULT '',
			actor           TEXT NOT NULL DEFAULT 'system',
			http_status     INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (notification_id, n)
		)`,
	}
	if dialect == "postgres" {
		q = append(q, guardFunction, `DROP TRIGGER IF EXISTS trg_`+TableNotifications+`_guard ON `+TableNotifications,
			`CREATE TRIGGER trg_`+TableNotifications+`_guard BEFORE UPDATE ON `+TableNotifications+
				` FOR EACH ROW EXECUTE FUNCTION `+TableNotifications+`_guard()`)
	}
	return q
}

// guardFunction makes the input columns append-only. headers and body_raw may
// change only inside a purge (set_config('metacore.inbox_purge','on',true)).
const guardFunction = `CREATE OR REPLACE FUNCTION ` + TableNotifications + `_guard() RETURNS trigger AS $$
BEGIN
	IF NEW.id IS DISTINCT FROM OLD.id
	OR NEW.organization_id IS DISTINCT FROM OLD.organization_id
	OR NEW.provider IS DISTINCT FROM OLD.provider
	OR NEW.route IS DISTINCT FROM OLD.route
	OR NEW.received_at IS DISTINCT FROM OLD.received_at
	OR NEW.source_ip IS DISTINCT FROM OLD.source_ip
	OR NEW.method IS DISTINCT FROM OLD.method
	OR NEW.query IS DISTINCT FROM OLD.query
	OR NEW.body_sha256 IS DISTINCT FROM OLD.body_sha256
	OR NEW.body_truncated IS DISTINCT FROM OLD.body_truncated
	OR NEW.body_redacted IS DISTINCT FROM OLD.body_redacted
	OR NEW.dedupe_key IS DISTINCT FROM OLD.dedupe_key
	THEN
		RAISE EXCEPTION 'commerce_payment_notifications: input columns are append-only' USING ERRCODE = '42501';
	END IF;
	IF (NEW.headers IS DISTINCT FROM OLD.headers OR NEW.body_raw IS DISTINCT FROM OLD.body_raw)
	   AND coalesce(current_setting('metacore.inbox_purge', true), '') <> 'on' THEN
		RAISE EXCEPTION 'commerce_payment_notifications: headers and body_raw are append-only outside a purge' USING ERRCODE = '42501';
	END IF;
	RETURN NEW;
END;
$$ LANGUAGE plpgsql`

func lockKey(name string) int64 {
	h := fnv.New64a()
	h.Write([]byte("metacore:payments:" + name))
	return int64(h.Sum64())
}
