-- +goose Up
-- +goose StatementBegin
-- marketplace_installations gains the lifecycle columns the uninstall /
-- upgrade / rollback flow needs. The table is also AutoMigrated by
-- marketplace.NewHandler on first boot — this migration exists for hosts
-- that drive their schema via the SQL runner instead of gorm.
CREATE TABLE IF NOT EXISTS marketplace_installations (
    id               TEXT        NOT NULL PRIMARY KEY,
    organization_id  TEXT        NOT NULL,
    addon_key        TEXT        NOT NULL,
    name             TEXT,
    category         TEXT,
    version          TEXT        NOT NULL DEFAULT '',
    bundle_url       TEXT,
    status           TEXT        NOT NULL DEFAULT 'requested',
    error_message    TEXT,
    requested_by_id  TEXT        NOT NULL,
    requested_at     DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    completed_at     DATETIME
);
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE marketplace_installations ADD COLUMN previous_version TEXT;
ALTER TABLE marketplace_installations ADD COLUMN previous_versions TEXT;
ALTER TABLE marketplace_installations ADD COLUMN upgraded_at DATETIME;
ALTER TABLE marketplace_installations ADD COLUMN uninstalled_at DATETIME;
ALTER TABLE marketplace_installations ADD COLUMN purge_at DATETIME;
ALTER TABLE marketplace_installations ADD COLUMN data_policy TEXT;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE marketplace_installations DROP COLUMN data_policy;
ALTER TABLE marketplace_installations DROP COLUMN purge_at;
ALTER TABLE marketplace_installations DROP COLUMN uninstalled_at;
ALTER TABLE marketplace_installations DROP COLUMN upgraded_at;
ALTER TABLE marketplace_installations DROP COLUMN previous_versions;
ALTER TABLE marketplace_installations DROP COLUMN previous_version;
DROP TABLE IF EXISTS marketplace_installations;
-- +goose StatementEnd
