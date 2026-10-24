-- +goose Up
-- +goose StatementBegin
-- metacore_addon_settings: ajustes NO secretos de un addon, con alcance y
-- revision (ADR docs/adr/0002-addon-settings-scoped-store.md, fase F1).
--
--   * scope_kind: 'org' (scope_id = '') o 'branch' (scope_id = id de sucursal).
--   * rev: concurrencia optimista; sube en cada cambio real (>= 1).
--   * values: SOLO lo que alguien fijo. Los defaults del manifest nunca se
--     materializan aqui; se resuelven al leer.
--   * Sin claves foraneas: no depende de tablas legacy (organizations,
--     metacore_installations), asi sobrevive a su retiro.
--
-- Tipos portables: en Postgres UUID/JSONB/TIMESTAMPTZ son nativos; en SQLite
-- (tests) se guardan como texto.
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

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS metacore_addon_settings;
-- +goose StatementEnd
