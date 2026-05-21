-- +goose Down
-- +goose StatementBegin
-- SQLite does not support DROP COLUMN before 3.35; PG does. We drop only the
-- columns the up-migration added, leaving the base table for the previous
-- migration to manage. Operators rolling back two steps will see the table
-- removed when 0006's predecessor runs Down.
ALTER TABLE marketplace_installations DROP COLUMN data_policy;
ALTER TABLE marketplace_installations DROP COLUMN purge_at;
ALTER TABLE marketplace_installations DROP COLUMN uninstalled_at;
ALTER TABLE marketplace_installations DROP COLUMN upgraded_at;
ALTER TABLE marketplace_installations DROP COLUMN previous_versions;
ALTER TABLE marketplace_installations DROP COLUMN previous_version;
DROP TABLE IF EXISTS marketplace_installations;
-- +goose StatementEnd
