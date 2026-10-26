package wasm

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// source_unavailable (DISENO-DEGRADACION §B.1, phase DG1.1).
//
// A data_query / data_mutate / data_batch whose target table (or one of its
// columns) does not exist physically used to surface as a generic `db_error`
// carrying Postgres' text, so every addon re-implemented the detection with
// strings.Contains(msg, "42P01"). The host owns the typed SQLSTATE, so it
// classifies the structural absence once and hands the guest a stable code:
//
//	{ "code": "source_unavailable", "legacy_code": "db_error",
//	  "message": "<original driver text, untouched>",
//	  "details": { "source", "reason", "owners", "installed_owners",
//	               "sqlstate", "degradable" } }
//
// Backward compatibility: `message` is byte-for-byte the text the guest got as
// `db_error` (SQLSTATE included), so a published guest that greps "42P01" keeps
// working, and `legacy_code` carries the code an older client matched on.
//
// Classification is by SQLSTATE (pgconn.PgError.Code), never by message text.
// Only STRUCTURAL absence is classified: 42P01 (undefined_table) and 42703
// (undefined_column). Timeouts, connection loss, permission denied, constraint
// violations and every other driver error stay plain `db_error` — a transient
// failure must never look like "the addon is not installed".

// CodeSourceUnavailable is the host error code for a structurally absent data
// source.
const CodeSourceUnavailable = "source_unavailable"

// Reasons carried in error.details.reason.
const (
	// ReasonAddonNotInstalled: no owner of the table is installed in this org.
	ReasonAddonNotInstalled = "addon_not_installed"
	// ReasonAddonDisabled: an owner is installed in this org but disabled.
	ReasonAddonDisabled = "addon_disabled"
	// ReasonTableMissing: 42P01 and the owner is unknown (uncatalogued
	// third-party table) or IS installed (half-done install: an anomaly).
	ReasonTableMissing = "table_missing"
	// ReasonSchemaMismatch: 42703 — the table exists but lacks a column the
	// request names (the owner runs a version without it).
	ReasonSchemaMismatch = "schema_mismatch"
)

// SQLSTATEs the classifier recognises. Everything else is not an absence.
const (
	sqlStateUndefinedTable  = "42P01"
	sqlStateUndefinedColumn = "42703"
)

// SourceInfo is what the embedder knows about who owns a logical table in one
// organization. All lists are addon keys.
type SourceInfo struct {
	// Owners are every addon known to declare the table (models[].table).
	// Empty = uncatalogued.
	Owners []string
	// Installed are the owners installed AND enabled in the org.
	Installed []string
	// Disabled are the owners installed in the org but disabled.
	Disabled []string
}

// SourceOwnersFn resolves SourceInfo for (org, logical table). It runs only on
// the failure path, at most once per table per invocation (the host memoises
// the result on the invocation), so an implementation backed by
// installer.InstalledSet costs one query per invocation that hits an absence.
type SourceOwnersFn func(orgID uuid.UUID, table string) SourceInfo

// WithSourceOwners injects the embedder's table→owners resolution, the same
// pattern as WithTableResolver: the kernel does not know where the embedder
// keeps its manifests. Without it the host can only report `table_missing` /
// `schema_mismatch`, a valid minimum for any embedder.
func (h *Host) WithSourceOwners(fn SourceOwnersFn) *Host {
	h.sourceOwners = fn
	return h
}

// sourceAbsence is a classified structural absence.
type sourceAbsence struct {
	Source   string
	Reason   string
	SQLState string
	Info     SourceInfo
}

// classifySourceAbsence returns the absence an error represents, or nil when it
// is anything else (transient failure, permission, constraint, ...). `table`
// is the LOGICAL table the guest asked for.
func classifySourceAbsence(inv *invocation, table string, err error) *sourceAbsence {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return nil
	}
	switch pgErr.Code {
	case sqlStateUndefinedTable:
		// Only the zero-row probe of the REQUESTED table proves that table is
		// absent. A 42P01 raised later in the same call (extension table in an
		// EXISTS / LEFT JOIN, a parent probe, a trigger) names ANOTHER relation:
		// blaming the requested table for it would invent an absence (and, on
		// a read, a degradable one). Those keep the plain db_error of before.
		var probe *baseProbeError
		if !errors.As(err, &probe) {
			return nil
		}
		info := inv.sourceInfo(table)
		return &sourceAbsence{Source: table, SQLState: pgErr.Code, Info: info, Reason: reasonForMissingTable(info)}
	case sqlStateUndefinedColumn:
		return &sourceAbsence{Source: table, SQLState: pgErr.Code, Info: inv.sourceInfo(table), Reason: ReasonSchemaMismatch}
	}
	return nil
}

// baseProbeError marks an error raised by the zero-row probe of the REQUESTED
// table (tableColumns on the resolved physical name), the first statement every
// data_query / data_mutate / data_batch op runs against it. Error() is the
// wrapped text, untouched, so the message the guest sees does not change.
type baseProbeError struct{ err error }

func (e *baseProbeError) Error() string { return e.err.Error() }
func (e *baseProbeError) Unwrap() error { return e.err }

func baseProbeErr(err error) error {
	if err == nil {
		return nil
	}
	return &baseProbeError{err: err}
}

// reasonForMissingTable decides the reason of a 42P01. An owner that IS
// installed and enabled while its table is missing is a broken install, not a
// missing addon, so it stays `table_missing` (an anomaly the embedder counts).
func reasonForMissingTable(info SourceInfo) string {
	if len(info.Owners) == 0 || len(info.Installed) > 0 {
		return ReasonTableMissing
	}
	if len(info.Disabled) > 0 {
		return ReasonAddonDisabled
	}
	return ReasonAddonNotInstalled
}

// sourceInfo memoises the embedder lookup on the invocation.
func (inv *invocation) sourceInfo(table string) SourceInfo {
	if inv == nil || inv.sourceOwners == nil {
		return SourceInfo{}
	}
	if inv.sourceInfoCache == nil {
		inv.sourceInfoCache = map[string]SourceInfo{}
	}
	if info, ok := inv.sourceInfoCache[table]; ok {
		return info
	}
	info := inv.sourceOwners(inv.orgID, table)
	inv.sourceInfoCache[table] = info
	return info
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// sourceUnavailableBody is the `error` object of a source_unavailable failure.
// `message` is the original driver text, untouched. `degradable` tells the
// guest whether serving an empty result is allowed: only a READ of an absent
// source outside an open transaction is; a write is never degraded (the host
// never turns it into a no-op).
func sourceUnavailableBody(message string, a *sourceAbsence, degradable bool, correlationID string) map[string]any {
	errBody := hostErrorBody(CodeSourceUnavailable, message)
	errBody["legacy_code"] = "db_error"
	errBody["details"] = map[string]any{
		"source":           a.Source,
		"reason":           a.Reason,
		"owners":           nonNil(a.Info.Owners),
		"installed_owners": nonNil(a.Info.Installed),
		"sqlstate":         a.SQLState,
		"degradable":       degradable,
	}
	if id := strings.TrimSpace(correlationID); id != "" {
		errBody["correlation_id"] = id
	}
	return errBody
}

// on_absent (DISENO-DEGRADACION §B.2, phase DG1.2): per-call opt-in of
// data_query to serve a degradable structural absence as an empty SUCCESS.
const (
	OnAbsentError = "error" // default: source_unavailable error (same as omitted)
	OnAbsentEmpty = "empty" // degradable absence -> success, empty data, meta.absent
)

// validOnAbsent reports whether v is an accepted on_absent value.
func validOnAbsent(v string) bool { return v == "" || v == OnAbsentError || v == OnAbsentEmpty }

// installRuleAbsence implements the per-org install rule (§B.2, P3), applied
// ONLY with on_absent=empty and before touching the database: when the
// embedder knows owners of the table and none is installed AND enabled in this
// org, the source is absent for this org even if the physical table exists
// (another org installed the addon). Uncatalogued tables (no owners) and
// embedders without WithSourceOwners never match: they fall through to the
// physical behaviour.
func installRuleAbsence(inv *invocation, table string) *sourceAbsence {
	info := inv.sourceInfo(table)
	if len(info.Owners) == 0 || len(info.Installed) > 0 {
		return nil
	}
	return &sourceAbsence{Source: table, SQLState: sqlStateUndefinedTable, Info: info, Reason: reasonForMissingTable(info)}
}

// absentEmpty is the success envelope of an absent source served as empty. The
// data is IDENTICAL with and without `aggregate` ({"rows":[],"truncated":false})
// so a guest needs no second code path.
func absentEmpty(addonKey string, a *sourceAbsence, orgID uuid.UUID, start time.Time, corr string) []byte {
	meta := dataMutateMeta(addonKey, orgID, start, corr)
	meta["absent"] = map[string]any{
		"source": a.Source,
		"reason": a.Reason,
		"owners": nonNil(a.Info.Owners),
	}
	b, _ := json.Marshal(map[string]any{
		"success": true,
		"data":    map[string]any{"rows": []map[string]any{}, "truncated": false},
		"meta":    meta,
	})
	return b
}

// dataQueryFailDB / dataMutateFailDB / dataBatchFailDB are the failure path for
// an error that came from the database while touching `table` (LOGICAL name):
// a structural absence becomes `source_unavailable`, anything else keeps
// `code` ("db_error") and the original message exactly as before.
func sourceEnvelope(addonKey string, errBody map[string]any, meta map[string]any) []byte {
	b, _ := json.Marshal(map[string]any{"success": false, "error": errBody, "meta": meta})
	return b
}

func dataQueryFailDB(inv *invocation, addonKey, table, onAbsent string, err error, orgID uuid.UUID, start time.Time, corr string, fallback func(code, msg string) []byte) []byte {
	if a := classifySourceAbsence(inv, table, err); a != nil {
		// A read that ran on the caller's open transaction (Host.InvokeInTx:
		// data_query has no savepoint of its own) has just ABORTED it: every
		// later statement of the invocation fails with 25P02. Serving "empty"
		// and carrying on cannot work there, so the read is not degradable
		// (42P01 and 42703 alike). `degradable` and the on_absent=empty
		// decision below are the SAME boolean, so they cannot disagree.
		inTx := inv != nil && inv.tx != nil && isTx(inv.tx)
		degradable := !inTx
		if degradable && onAbsent == OnAbsentEmpty {
			return absentEmpty(addonKey, a, orgID, start, corr)
		}
		return sourceEnvelope(addonKey, sourceUnavailableBody(err.Error(), a, degradable, corr), dataMutateMeta(addonKey, orgID, start, corr))
	}
	return fallback("db_error", err.Error())
}

// dataMutateFailDB is the write-side twin: `msgPrefix` keeps data_batch's
// "mutations[i]: " prefix in the message.
func dataMutateFailDB(inv *invocation, addonKey, code, table string, err error, orgID uuid.UUID, start time.Time, corr string, batch bool, msgPrefix string, fallback func(code, msg string) []byte) []byte {
	if code == "db_error" {
		if a := classifySourceAbsence(inv, table, err); a != nil {
			meta := dataMutateMeta(addonKey, orgID, start, corr)
			if batch {
				meta = dataBatchMeta(addonKey, orgID, start, corr)
			}
			return sourceEnvelope(addonKey, sourceUnavailableBody(msgPrefix+err.Error(), a, false, corr), meta)
		}
	}
	return fallback(code, msgPrefix+err.Error())
}
