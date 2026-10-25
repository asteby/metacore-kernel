-- +goose Up
-- addon_diagnostic_groups / addon_diagnostic_events: diagnostico saneado de fallos de
-- addon (ADR docs/adr/0003-addon-diagnostics.md, fase F0). Tablas aisladas: sin claves
-- foraneas hacia tablas legacy ni modelo declarativo. El filtro por org vive en la capa
-- de app (diag.Store): la RLS es decorativa en los hosts.
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS addon_diagnostic_groups (
    id            UUID NOT NULL PRIMARY KEY,
    org_id        UUID NOT NULL,
    addon_key     TEXT NOT NULL,
    addon_version TEXT NOT NULL DEFAULT '',
    operation     TEXT NOT NULL DEFAULT '',
    kind          TEXT NOT NULL,
    code          TEXT NOT NULL,
    severity      TEXT NOT NULL DEFAULT 'error',
    http_status   INTEGER NOT NULL DEFAULT 0,
    fingerprint   TEXT NOT NULL,
    count         BIGINT NOT NULL DEFAULT 1,
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_seen_at  TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    status        TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'muted', 'resolved')),
    last_event_id UUID
);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE UNIQUE INDEX IF NOT EXISTS uq_addon_diagnostic_groups_fp ON addon_diagnostic_groups (org_id, fingerprint);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS idx_addon_diagnostic_groups_addon ON addon_diagnostic_groups (org_id, addon_key, last_seen_at);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS idx_addon_diagnostic_groups_code ON addon_diagnostic_groups (org_id, code);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS addon_diagnostic_events (
    id             UUID NOT NULL PRIMARY KEY,
    org_id         UUID NOT NULL,
    group_id       UUID NOT NULL,
    correlation_id TEXT NOT NULL DEFAULT '',
    invocation_id  TEXT NOT NULL DEFAULT '',
    diag_ref       TEXT NOT NULL DEFAULT '',
    subject_type   TEXT NOT NULL DEFAULT '',
    subject_id     TEXT NOT NULL DEFAULT '',
    is_first       BOOLEAN NOT NULL DEFAULT false,
    occurred_at    TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    payload        JSONB NOT NULL DEFAULT '{}'
);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS idx_addon_diagnostic_events_group ON addon_diagnostic_events (org_id, group_id, occurred_at);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS idx_addon_diagnostic_events_corr ON addon_diagnostic_events (org_id, correlation_id);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS idx_addon_diagnostic_events_subject ON addon_diagnostic_events (org_id, subject_type, subject_id);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS idx_addon_diagnostic_events_occ ON addon_diagnostic_events (occurred_at);
-- +goose StatementEnd
