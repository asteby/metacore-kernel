-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_metacore_addon_i18n_lang;
-- +goose StatementEnd

-- +goose StatementBegin
DROP INDEX IF EXISTS idx_metacore_addon_i18n_addon_key;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE IF EXISTS metacore_addon_i18n;
-- +goose StatementEnd
