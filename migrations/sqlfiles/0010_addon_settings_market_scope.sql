-- +goose Up
-- metacore_addon_settings: scope_kind admite 'market' (scope_id = codigo de mercado,
-- p. ej. 'mx'/'us'). Un CHECK no se puede alterar en SQLite, asi que se recrea la tabla
-- (portable Postgres/SQLite); los datos existentes se copian tal cual.
-- +goose StatementBegin
CREATE TABLE metacore_addon_settings_new (
    org_id      UUID         NOT NULL,
    addon_key   TEXT         NOT NULL,
    scope_kind  TEXT         NOT NULL CHECK (scope_kind IN ('org', 'branch', 'market')),
    scope_id    TEXT         NOT NULL DEFAULT '',
    "values"    JSONB        NOT NULL DEFAULT '{}',
    rev         BIGINT       NOT NULL DEFAULT 1,
    updated_by  UUID,
    updated_at  TIMESTAMPTZ  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (org_id, addon_key, scope_kind, scope_id)
);
-- +goose StatementEnd
-- +goose StatementBegin
INSERT INTO metacore_addon_settings_new (org_id, addon_key, scope_kind, scope_id, "values", rev, updated_by, updated_at)
SELECT org_id, addon_key, scope_kind, scope_id, "values", rev, updated_by, updated_at FROM metacore_addon_settings;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE metacore_addon_settings;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE metacore_addon_settings_new RENAME TO metacore_addon_settings;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE TABLE metacore_addon_settings_old (
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
-- +goose StatementBegin
INSERT INTO metacore_addon_settings_old (org_id, addon_key, scope_kind, scope_id, "values", rev, updated_by, updated_at)
SELECT org_id, addon_key, scope_kind, scope_id, "values", rev, updated_by, updated_at FROM metacore_addon_settings
WHERE scope_kind IN ('org', 'branch');
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE metacore_addon_settings;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE metacore_addon_settings_old RENAME TO metacore_addon_settings;
-- +goose StatementEnd
