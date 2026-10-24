-- +goose Up
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS metacore_addon_settings (
    org_id      UUID         NOT NULL,
    addon_key   TEXT         NOT NULL,
    scope_kind  TEXT         NOT NULL CHECK (scope_kind IN ('org', 'branch')),
    scope_id    TEXT         NOT NULL DEFAULT '',
    "values"    JSONB        NOT NULL DEFAULT '{}',
    rev         BIGINT       NOT NULL DEFAULT 1,
    updated_by  UUID,
    updated_at  TIMESTAMPTZ  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (org_id, addon_key, scope_kind, scope_id)
);
-- +goose StatementEnd
