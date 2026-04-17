-- +goose Up
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS metacore_installations (
    id               TEXT        NOT NULL PRIMARY KEY DEFAULT (lower(hex(randomblob(4))) || '-' || lower(hex(randomblob(2))) || '-4' || substr(lower(hex(randomblob(2))),2) || '-' || substr('89ab', abs(random()) % 4 + 1, 1) || substr(lower(hex(randomblob(2))),2) || '-' || lower(hex(randomblob(6)))),
    organization_id  TEXT        NOT NULL,
    addon_key        TEXT        NOT NULL,
    version          TEXT        NOT NULL DEFAULT '',
    status           TEXT        NOT NULL DEFAULT 'enabled',
    source           TEXT        NOT NULL DEFAULT '',
    secret_hash      TEXT        NOT NULL DEFAULT '',
    secret_enc       TEXT        NOT NULL DEFAULT '',
    settings         TEXT        NOT NULL DEFAULT '{}',
    installed_at     DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    enabled_at       DATETIME,
    disabled_at      DATETIME,
    created_at       DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at       DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    deleted_at       DATETIME
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE UNIQUE INDEX IF NOT EXISTS idx_metacore_installations_org_addon  ON metacore_installations(organization_id, addon_key) WHERE deleted_at IS NULL;
CREATE        INDEX IF NOT EXISTS idx_metacore_installations_deleted_at ON metacore_installations(deleted_at);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS metacore_installations;
-- +goose StatementEnd
