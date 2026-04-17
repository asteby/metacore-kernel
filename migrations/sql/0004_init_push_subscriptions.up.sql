-- +goose Up
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS push_subscriptions (
    id               TEXT        NOT NULL PRIMARY KEY DEFAULT (lower(hex(randomblob(4))) || '-' || lower(hex(randomblob(2))) || '-4' || substr(lower(hex(randomblob(2))),2) || '-' || substr('89ab', abs(random()) % 4 + 1, 1) || substr(lower(hex(randomblob(2))),2) || '-' || lower(hex(randomblob(6)))),
    organization_id  TEXT        NOT NULL,
    created_by_id    TEXT,
    user_id          TEXT        NOT NULL,
    endpoint         TEXT        NOT NULL,
    p256dh           TEXT        NOT NULL DEFAULT '',
    auth             TEXT        NOT NULL DEFAULT '',
    device_type      TEXT        NOT NULL DEFAULT '',
    user_agent       TEXT        NOT NULL DEFAULT '',
    last_used_at     DATETIME,
    created_at       DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at       DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    deleted_at       DATETIME
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE UNIQUE INDEX IF NOT EXISTS idx_push_subscriptions_endpoint      ON push_subscriptions(endpoint)        WHERE deleted_at IS NULL;
CREATE        INDEX IF NOT EXISTS idx_push_subscriptions_user_id       ON push_subscriptions(user_id);
CREATE        INDEX IF NOT EXISTS idx_push_subscriptions_org_id        ON push_subscriptions(organization_id);
CREATE        INDEX IF NOT EXISTS idx_push_subscriptions_deleted_at    ON push_subscriptions(deleted_at);
-- +goose StatementEnd
