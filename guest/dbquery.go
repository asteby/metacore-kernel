package guest

import (
	"encoding/json"
	"fmt"
)

// ColumnMeta describes one result column. Matches the host's column
// metadata shape from runtime/wasm/dbquery.go:176-185.
type ColumnMeta struct {
	// Name is the column name as the driver reported it.
	Name string
	// Type is the lowercase driver type name (e.g. "int8", "uuid",
	// "text"). Empty when the driver does not surface a type.
	Type string
}

// QueryResult is the success payload returned by DbQuery. It mirrors
// the `data` block of the host envelope documented in
// docs/wasm-abi.md § 9.4.
type QueryResult struct {
	// Rows is the decoded result set. Each row is a map keyed by
	// column name; values match the JSON types the driver produced
	// (numbers as float64, etc — see encoding/json defaults).
	Rows []map[string]any
	// Columns is the column metadata (name + type).
	Columns []ColumnMeta
	// RowCount mirrors data.rowCount in the envelope. For a normal
	// SELECT it equals len(Rows); for a query that hit the row cap
	// the host emits a `row_limit_exceeded` error, so by the time
	// the helper returns this field is always === len(Rows).
	RowCount int
	// Meta carries host-side bookkeeping (schema, durationMs,
	// truncated). New meta fields land here additively.
	Meta QueryMeta
}

// QueryMeta is the deserialised `meta` block of the host envelope.
type QueryMeta struct {
	// Schema is the canonical Postgres schema the call ran against
	// (`addon_<key>`). Informational.
	Schema string
	// DurationMs is the host-side wall time the query consumed.
	DurationMs int64
	// Truncated reports whether the host capped the result set.
	// Today the host always returns false on success — over-cap
	// queries surface as the `row_limit_exceeded` error — but the
	// field is preserved for forward-compat.
	Truncated bool
}

// DbQueryError is the typed error DbQuery returns when the host
// publishes `{success:false}`. Codes are documented in
// docs/wasm-abi.md § 9.4.
//
// Use `errors.As` to branch:
//
//	var qErr *guest.DbQueryError
//	if errors.As(err, &qErr) {
//	    switch qErr.Code {
//	    case "forbidden":          /* capability missing */
//	    case "row_limit_exceeded": /* paginate */
//	    case "query_timeout":      /* simplify */
//	    }
//	}
type DbQueryError struct {
	// Code is the machine-readable error code.
	Code string
	// Message is the human-readable detail.
	Message string
	// Meta is populated even on errors — durationMs / schema are
	// usable for telemetry.
	Meta QueryMeta
}

// Error implements the error interface.
func (e *DbQueryError) Error() string {
	if e == nil {
		return "guest: <nil db_query error>"
	}
	if e.Message == "" {
		return fmt.Sprintf("db_query: %s", e.Code)
	}
	return fmt.Sprintf("db_query: %s: %s", e.Code, e.Message)
}

// dbQueryEnvelope mirrors docs/wasm-abi.md § 9.4 verbatim.
type dbQueryEnvelope struct {
	Success bool             `json:"success"`
	Data    *dbQueryWireData `json:"data,omitempty"`
	Error   *dbWireError     `json:"error,omitempty"`
	Meta    *dbWireMeta      `json:"meta,omitempty"`
}

type dbQueryWireData struct {
	Rows     []map[string]any `json:"rows"`
	RowCount int              `json:"rowCount"`
	Columns  []dbWireColumn   `json:"columns"`
}

// dbWireError / dbWireMeta / dbWireColumn are shared with db_exec —
// the host envelopes use the same shape for both imports.
type dbWireError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type dbWireMeta struct {
	Schema     string `json:"schema"`
	DurationMs int64  `json:"durationMs"`
	Truncated  bool   `json:"truncated"`
}

type dbWireColumn struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

func (m *dbWireMeta) toQueryMeta() QueryMeta {
	if m == nil {
		return QueryMeta{}
	}
	return QueryMeta{
		Schema:     m.Schema,
		DurationMs: m.DurationMs,
		Truncated:  m.Truncated,
	}
}

func wireColumnsToMeta(cols []dbWireColumn) []ColumnMeta {
	if len(cols) == 0 {
		return nil
	}
	out := make([]ColumnMeta, len(cols))
	for i, c := range cols {
		out[i] = ColumnMeta{Name: c.Name, Type: c.Type}
	}
	return out
}

// decodeDbQueryEnvelope parses raw envelope bytes into a QueryResult
// or a typed DbQueryError. Exposed for host-side tests.
func decodeDbQueryEnvelope(buf []byte) (QueryResult, error) {
	if len(buf) == 0 {
		// The host always allocates an envelope for db_query (see
		// abi.md § 9.1 — "A return of 0 is reserved and currently
		// never produced"). Treat empty as a wire-level anomaly.
		return QueryResult{}, &DbQueryError{
			Code:    "db_error",
			Message: "host returned empty envelope",
		}
	}
	var env dbQueryEnvelope
	if err := json.Unmarshal(buf, &env); err != nil {
		return QueryResult{}, fmt.Errorf("guest: decode db_query envelope: %w", err)
	}
	meta := env.Meta.toQueryMeta()
	if !env.Success {
		qErr := &DbQueryError{Meta: meta}
		if env.Error != nil {
			qErr.Code = env.Error.Code
			qErr.Message = env.Error.Message
		}
		if qErr.Code == "" {
			qErr.Code = "unknown"
		}
		return QueryResult{Meta: meta}, qErr
	}
	res := QueryResult{Meta: meta}
	if env.Data != nil {
		res.Rows = env.Data.Rows
		res.RowCount = env.Data.RowCount
		res.Columns = wireColumnsToMeta(env.Data.Columns)
	}
	return res, nil
}

// encodeQueryArgs marshals positional args into the JSON-array shape
// the host expects (docs/wasm-abi.md § 9.6). Exposed for tests; the
// wasm caller is the production consumer.
//
// `nil` args (or zero-length) returns `nil` so the helper can pass
// (0, 0) to the host — saves a 2-byte `[]` allocation when the
// statement has no placeholders.
func encodeQueryArgs(args []any) ([]byte, error) {
	if len(args) == 0 {
		return nil, nil
	}
	buf, err := json.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("guest: marshal db_query args: %w", err)
	}
	return buf, nil
}
