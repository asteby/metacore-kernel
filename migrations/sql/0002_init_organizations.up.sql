-- +goose Up
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS organizations (
    id               TEXT        NOT NULL PRIMARY KEY DEFAULT (lower(hex(randomblob(4))) || '-' || lower(hex(randomblob(2))) || '-4' || substr(lower(hex(randomblob(2))),2) || '-' || substr('89ab', abs(random()) % 4 + 1, 1) || substr(lower(hex(randomblob(2))),2) || '-' || lower(hex(randomblob(6)))),
    organization_id  TEXT        NOT NULL,
    created_by_id    TEXT,
    name             TEXT        NOT NULL DEFAULT '',
    slug             TEXT        NOT NULL DEFAULT '',
    country          TEXT        NOT NULL DEFAULT '',
    currency         TEXT        NOT NULL DEFAULT '',
    timezone         TEXT        NOT NULL DEFAULT '',
    logo             TEXT        NOT NULL DEFAULT '',
    created_at       DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at       DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    deleted_at       DATETIME
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE UNIQUE INDEX IF NOT EXISTS idx_organizations_slug          ON organizations(slug)            WHERE deleted_at IS NULL;
CREATE        INDEX IF NOT EXISTS idx_organizations_org_id        ON organizations(organization_id) WHERE deleted_at IS NULL;
CREATE        INDEX IF NOT EXISTS idx_organizations_deleted_at    ON organizations(deleted_at);
CREATE        INDEX IF NOT EXISTS idx_organizations_created_by_id ON organizations(created_by_id);
-- +goose StatementEnd
