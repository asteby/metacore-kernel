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
