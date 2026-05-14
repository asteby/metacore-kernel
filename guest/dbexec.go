package guest

import (
	"encoding/json"
	"fmt"
)

// ExecResult is the success payload returned by DbExec. It mirrors
// the `data` block of the host envelope documented in
// docs/wasm-abi.md § 10.4 (post-v0.11 — includes RETURNING rows).
type ExecResult struct {
	// RowsAffected is the canonical mutation row count. For
	// RETURNING statements equals len(Rows); for plain
	// INSERT/UPDATE/DELETE it's the driver-reported count.
	RowsAffected int64
	// Rows is the RETURNING projection, when the statement
	// included one. Nil otherwise (statements without RETURNING
	// keep the legacy envelope shape — `rowsAffected` only — for
	// back-compat with consumers written before kernel v0.11.0).
	Rows []map[string]any
	// Columns is the column metadata for the RETURNING projection.
	// Nil when no RETURNING clause was present.
	Columns []ColumnMeta
	// Meta carries host-side bookkeeping (schema, durationMs).
	Meta ExecMeta
}

// ExecMeta is the deserialised `meta` block of the host envelope.
type ExecMeta struct {
	// Schema is the canonical Postgres schema the call ran against
	// (`addon_<key>`).
	Schema string
	// DurationMs is the host-side wall time the statement consumed.
	DurationMs int64
}

// DbExecError is the typed error DbExec returns. Codes listed in
// docs/wasm-abi.md § 10.4 — superset of db_query's codes plus the
// mutation-specific ones (`missing_org_filter`,
// `constraint_violation`, `serialization_failure`, `no_active_tx`).
//
// Use `errors.As` to branch:
//
//	var eErr *guest.DbExecError
//	if errors.As(err, &eErr) {
//	    switch eErr.Code {
//	    case "constraint_violation":   /* surface uniqueness etc */
//	    case "serialization_failure":  /* retry whole action */
//	    case "missing_org_filter":     /* developer bug */
//	    case "no_active_tx":           /* host wiring bug — caller
//	                                       used Invoke instead of
//	                                       InvokeInTx */
//	    }
//	}
type DbExecError struct {
	// Code is the machine-readable error code.
	Code string
	// Message is the human-readable detail.
	Message string
	// SQLState is the Postgres SQLSTATE preserved by the host for
	// constraint and serialization failures (§ 10.4 — "SQLSTATE
	// preserved in error.sqlstate"). Empty for non-driver errors.
	SQLState string
	// Meta is populated even on errors — durationMs / schema are
	// usable for telemetry.
	Meta ExecMeta
}

// Error implements the error interface.
func (e *DbExecError) Error() string {
	if e == nil {
		return "guest: <nil db_exec error>"
	}
	if e.Message == "" {
		return fmt.Sprintf("db_exec: %s", e.Code)
	}
	return fmt.Sprintf("db_exec: %s: %s", e.Code, e.Message)
}

// dbExecEnvelope mirrors docs/wasm-abi.md § 10.4 verbatim.
type dbExecEnvelope struct {
	Success bool            `json:"success"`
	Data    *dbExecWireData `json:"data,omitempty"`
	Error   *dbExecWireErr  `json:"error,omitempty"`
	Meta    *dbExecWireMeta `json:"meta,omitempty"`
}

type dbExecWireData struct {
	RowsAffected int64            `json:"rowsAffected"`
	Rows         []map[string]any `json:"rows,omitempty"`
	Columns      []dbWireColumn   `json:"columns,omitempty"`
}

// dbExecWireErr extends dbWireError with the SQLSTATE field the host
// preserves on driver errors (§ 10.4).
type dbExecWireErr struct {
	Code     string `json:"code"`
	Message  string `json:"message"`
	SQLState string `json:"sqlstate,omitempty"`
}

// dbExecWireMeta is a separate type from dbWireMeta because db_exec
// does NOT populate the `truncated` field — only schema + durationMs.
// Future host fields will land here without touching dbWireMeta.
type dbExecWireMeta struct {
	Schema     string `json:"schema"`
	DurationMs int64  `json:"durationMs"`
	// TxID is reserved — § 10.4's error envelope sample shows a
	// `txId` field; the host does not emit it today but the slot
	// is here so a future host can populate it without bumping the
	// wire type.
	TxID string `json:"txId,omitempty"`
}

func (m *dbExecWireMeta) toExecMeta() ExecMeta {
	if m == nil {
		return ExecMeta{}
	}
	return ExecMeta{Schema: m.Schema, DurationMs: m.DurationMs}
}

// decodeDbExecEnvelope parses raw envelope bytes into an ExecResult
// or a typed DbExecError. Exposed for host-side tests.
func decodeDbExecEnvelope(buf []byte) (ExecResult, error) {
	if len(buf) == 0 {
		// Host always allocates an envelope for db_exec (§ 10.1 —
		// "A return of 0 is reserved and currently never produced").
		return ExecResult{}, &DbExecError{
			Code:    "db_error",
			Message: "host returned empty envelope",
		}
	}
	var env dbExecEnvelope
	if err := json.Unmarshal(buf, &env); err != nil {
		return ExecResult{}, fmt.Errorf("guest: decode db_exec envelope: %w", err)
	}
	meta := env.Meta.toExecMeta()
	if !env.Success {
		eErr := &DbExecError{Meta: meta}
		if env.Error != nil {
			eErr.Code = env.Error.Code
			eErr.Message = env.Error.Message
			eErr.SQLState = env.Error.SQLState
		}
		if eErr.Code == "" {
			eErr.Code = "unknown"
		}
		return ExecResult{Meta: meta}, eErr
	}
	res := ExecResult{Meta: meta}
	if env.Data != nil {
		res.RowsAffected = env.Data.RowsAffected
		res.Rows = env.Data.Rows
		res.Columns = wireColumnsToMeta(env.Data.Columns)
	}
	return res, nil
}
