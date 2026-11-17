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
-- +goose StatementEnd
