-- +goose Up
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS users (
    id               TEXT        NOT NULL PRIMARY KEY DEFAULT (lower(hex(randomblob(4))) || '-' || lower(hex(randomblob(2))) || '-4' || substr(lower(hex(randomblob(2))),2) || '-' || substr('89ab', abs(random()) % 4 + 1, 1) || substr(lower(hex(randomblob(2))),2) || '-' || lower(hex(randomblob(6)))),
    organization_id  TEXT        NOT NULL,
    created_by_id    TEXT,
    name             TEXT        NOT NULL DEFAULT '',
    email            TEXT        NOT NULL,
    password_hash    TEXT        NOT NULL DEFAULT '',
    role             TEXT        NOT NULL DEFAULT 'agent',
    avatar           TEXT        NOT NULL DEFAULT '',
    last_login_at    DATETIME,
    created_at       DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at       DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    deleted_at       DATETIME
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE UNIQUE INDEX IF NOT EXISTS idx_users_email         ON users(email)           WHERE deleted_at IS NULL;
CREATE        INDEX IF NOT EXISTS idx_users_org_id        ON users(organization_id) WHERE deleted_at IS NULL;
CREATE        INDEX IF NOT EXISTS idx_users_role          ON users(role);
CREATE        INDEX IF NOT EXISTS idx_users_deleted_at    ON users(deleted_at);
CREATE        INDEX IF NOT EXISTS idx_users_created_by_id ON users(created_by_id);
-- +goose StatementEnd
