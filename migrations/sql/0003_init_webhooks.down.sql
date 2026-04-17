-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS webhook_deliveries;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE IF EXISTS webhooks;
-- +goose StatementEnd
