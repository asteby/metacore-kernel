-- +goose Up
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS webhooks (
    id                  TEXT        NOT NULL PRIMARY KEY DEFAULT (lower(hex(randomblob(4))) || '-' || lower(hex(randomblob(2))) || '-4' || substr(lower(hex(randomblob(2))),2) || '-' || substr('89ab', abs(random()) % 4 + 1, 1) || substr(lower(hex(randomblob(2))),2) || '-' || lower(hex(randomblob(6)))),
    organization_id     TEXT        NOT NULL,
    created_by_id       TEXT,
    name                TEXT        NOT NULL DEFAULT '',
    url                 TEXT        NOT NULL DEFAULT '',
    events              TEXT        NOT NULL DEFAULT '',
    secret              TEXT        NOT NULL DEFAULT '',
    active              INTEGER     NOT NULL DEFAULT 1,
    retry_max           INTEGER     NOT NULL DEFAULT 3,
    timeout_sec         INTEGER     NOT NULL DEFAULT 15,
    owner_type          TEXT        NOT NULL DEFAULT '',
    owner_id            TEXT        NOT NULL DEFAULT '',
    last_triggered_at   DATETIME,
    failure_count       INTEGER     NOT NULL DEFAULT 0,
    success_count       INTEGER     NOT NULL DEFAULT 0,
    created_at          DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at          DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    deleted_at          DATETIME
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS idx_webhooks_org_id        ON webhooks(organization_id);
CREATE INDEX IF NOT EXISTS idx_webhooks_owner_type    ON webhooks(owner_type);
CREATE INDEX IF NOT EXISTS idx_webhooks_owner_id      ON webhooks(owner_id);
CREATE INDEX IF NOT EXISTS idx_webhooks_deleted_at    ON webhooks(deleted_at);
CREATE INDEX IF NOT EXISTS idx_webhooks_created_by_id ON webhooks(created_by_id);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS webhook_deliveries (
    id                  TEXT        NOT NULL PRIMARY KEY DEFAULT (lower(hex(randomblob(4))) || '-' || lower(hex(randomblob(2))) || '-4' || substr(lower(hex(randomblob(2))),2) || '-' || substr('89ab', abs(random()) % 4 + 1, 1) || substr(lower(hex(randomblob(2))),2) || '-' || lower(hex(randomblob(6)))),
    organization_id     TEXT        NOT NULL,
    created_by_id       TEXT,
    webhook_id          TEXT        NOT NULL,
    event               TEXT        NOT NULL DEFAULT '',
    payload             TEXT        NOT NULL DEFAULT '',
    request_headers     TEXT,
    response_status     INTEGER     NOT NULL DEFAULT 0,
    response_body       TEXT        NOT NULL DEFAULT '',
    response_headers    TEXT,
    attempt_count       INTEGER     NOT NULL DEFAULT 0,
    succeeded           INTEGER     NOT NULL DEFAULT 0,
    error_message       TEXT        NOT NULL DEFAULT '',
    delivered_at        DATETIME,
    next_attempt_at     DATETIME,
    created_at          DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at          DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    deleted_at          DATETIME,
    FOREIGN KEY (webhook_id) REFERENCES webhooks(id)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_webhook_id      ON webhook_deliveries(webhook_id);
CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_event           ON webhook_deliveries(event);
CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_succeeded       ON webhook_deliveries(succeeded);
CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_next_attempt_at ON webhook_deliveries(next_attempt_at);
CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_deleted_at      ON webhook_deliveries(deleted_at);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS webhook_deliveries;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE IF EXISTS webhooks;
-- +goose StatementEnd
