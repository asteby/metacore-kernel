-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS addon_diagnostic_events;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS addon_diagnostic_groups;
-- +goose StatementEnd
