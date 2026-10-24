-- +goose Up
-- +goose StatementBegin
-- metacore_addon_i18n persists translations declared by addon manifests so
-- the kernel can serve them to any host without each app reimplementing the
-- projection (manifest.i18n[lang][key] and the unfolded v3 metadata_i18n).
--
-- Ported from migrations/sql/0007_addon_i18n.up.sql so this directory has no
-- gap at version 7 (a later out-of-order 0007 would break every database
-- already at v8). Idempotent: CREATE ... IF NOT EXISTS.
CREATE TABLE IF NOT EXISTS metacore_addon_i18n (
    addon_key  TEXT NOT NULL,
    lang       TEXT NOT NULL,
    key        TEXT NOT NULL,
    value      TEXT NOT NULL,
    PRIMARY KEY (addon_key, lang, key)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS idx_metacore_addon_i18n_addon_key
    ON metacore_addon_i18n(addon_key);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS idx_metacore_addon_i18n_lang
    ON metacore_addon_i18n(lang);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS metacore_addon_i18n;
-- +goose StatementEnd
