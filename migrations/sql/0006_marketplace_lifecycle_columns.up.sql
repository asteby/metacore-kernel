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
-- Lifecycle columns:
--   previous_version  — the version before the latest upgrade (drives rollback,
--                       capped to the last 3 versions via previous_versions JSON).
--   previous_versions — JSON-encoded array of versions installed in order,
--                       capped at 3 entries. Newest last.
--   upgraded_at       — timestamp of the most recent successful upgrade.
--   uninstalled_at    — timestamp when status transitioned to "uninstalled".
--   purge_at          — when retain_30d data policy is set, the moment a
--                       background sweeper may drop the addon's schema.
--   data_policy       — drop_now | retain_30d | retain_forever (default
--                       retain_30d, set at uninstall time).
ALTER TABLE marketplace_installations ADD COLUMN previous_version TEXT;
ALTER TABLE marketplace_installations ADD COLUMN previous_versions TEXT;
ALTER TABLE marketplace_installations ADD COLUMN upgraded_at DATETIME;
ALTER TABLE marketplace_installations ADD COLUMN uninstalled_at DATETIME;
ALTER TABLE marketplace_installations ADD COLUMN purge_at DATETIME;
ALTER TABLE marketplace_installations ADD COLUMN data_policy TEXT;
-- +goose StatementEnd
