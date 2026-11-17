-- +goose Up
-- webhooks / webhook_deliveries: columnas de las suscripciones declarativas (RFC 0003).
-- Aditiva y compatible hacia atras: todo es NULL / vacio / FALSE, y un webhook
-- con declarative NULL se comporta exactamente como antes.
-- +goose StatementBegin
ALTER TABLE webhooks ADD COLUMN declarative TEXT;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE webhooks ADD COLUMN consecutive_failures INTEGER NOT NULL DEFAULT 0;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE webhooks ADD COLUMN last_failed_at TIMESTAMP;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE webhooks ADD COLUMN last_http_status INTEGER NOT NULL DEFAULT 0;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE webhooks ADD COLUMN last_error TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE webhooks ADD COLUMN auto_disabled_at TIMESTAMP;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE webhook_deliveries ADD COLUMN event_id TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE webhook_deliveries ADD COLUMN idempotency_key TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE webhook_deliveries ADD COLUMN ordering_key TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE webhook_deliveries ADD COLUMN seq BIGINT NOT NULL DEFAULT 0;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE webhook_deliveries ADD COLUMN held BOOLEAN NOT NULL DEFAULT FALSE;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE webhook_deliveries ADD COLUMN in_flight BOOLEAN NOT NULL DEFAULT FALSE;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE webhook_deliveries ADD COLUMN payload_purged BOOLEAN NOT NULL DEFAULT FALSE;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_event_id    ON webhook_deliveries(webhook_id, event_id);
CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_ordering    ON webhook_deliveries(webhook_id, ordering_key, seq);
CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_held        ON webhook_deliveries(held);
-- Un evento republicado por el bus (at-least-once) no crea una segunda entrega.
CREATE UNIQUE INDEX IF NOT EXISTS uq_webhook_deliveries_event ON webhook_deliveries(webhook_id, event_id) WHERE event_id <> '';
-- Ventana de candidatos del worker.
CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_due ON webhook_deliveries(succeeded, next_attempt_at);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_webhook_deliveries_due;
DROP INDEX IF EXISTS uq_webhook_deliveries_event;
DROP INDEX IF EXISTS idx_webhook_deliveries_held;
DROP INDEX IF EXISTS idx_webhook_deliveries_ordering;
DROP INDEX IF EXISTS idx_webhook_deliveries_event_id;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE webhook_deliveries DROP COLUMN payload_purged;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE webhook_deliveries DROP COLUMN in_flight;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE webhook_deliveries DROP COLUMN held;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE webhook_deliveries DROP COLUMN seq;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE webhook_deliveries DROP COLUMN ordering_key;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE webhook_deliveries DROP COLUMN idempotency_key;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE webhook_deliveries DROP COLUMN event_id;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE webhooks DROP COLUMN auto_disabled_at;
ALTER TABLE webhooks DROP COLUMN last_error;
ALTER TABLE webhooks DROP COLUMN last_http_status;
ALTER TABLE webhooks DROP COLUMN last_failed_at;
ALTER TABLE webhooks DROP COLUMN consecutive_failures;
ALTER TABLE webhooks DROP COLUMN declarative;
-- +goose StatementEnd
