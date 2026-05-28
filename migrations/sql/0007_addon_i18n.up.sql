-- +goose Up
-- +goose StatementBegin
-- metacore_addon_i18n persists translations declared by addon manifests so
-- the kernel can serve them to any host (ops, link, future apps) without
-- each app reimplementing the projection. Three sources flow in:
--   * manifest.i18n[lang][key]      (legacy per-app string bundle)
--   * manifest.metadata_i18n[lang]  (v3 marketplace catalog block, unfolded
--                                    to "catalog.name"/"catalog.description"/
--                                    "catalog.features.<i>")
--
-- The composite primary key (addon_key, lang, key) is the natural upsert
-- target the installer uses; (addon_key) on its own indexes uninstall.
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
