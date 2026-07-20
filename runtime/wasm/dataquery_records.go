package wasm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// data_query host import limits. Request/deadline/response mirror
// data_mutate (docs/wasm-abi.md § 15.5) per the contract; the row cap is
// the import's own `limit` knob (default 50, hard max 200) — deliberately
// far below db_query's dbQueryMaxRows because data_query is a lookup
// primitive, not an export pipe.
const (
	dataQueryMaxReqBytes  = 64 * 1024
	dataQueryMaxRespBytes = 8 * 1024 * 1024
	dataQueryDeadline     = 5 * time.Second
	dataQueryDefaultLimit = 50
	dataQueryMaxLimit     = 200
)

// dataQueryRequest is the guest-supplied JSON request for
// `metacore_host.data_query` — the read-only sibling of data_mutate.
// `organization_id` is deliberately absent: tenant scope ALWAYS comes from
// the invocation context, and the host injects the `organization_id = ?`
// predicate on every query.
type dataQueryRequest struct {
	Table string                     `json:"table"` // logical, unqualified
	Where map[string]json.RawMessage `json:"where"` // equality-only filters
	Limit int                        `json:"limit"` // default 50, max 200
}

// dataQueryBlockedWhereCols are predicates the guest may never supply:
// `organization_id` is the tenant boundary the host injects itself, and
// `deleted_at` is host-managed (the import appends `deleted_at IS NULL`
// automatically when the table is soft-deletable).
var dataQueryBlockedWhereCols = map[string]bool{
	"organization_id": true,
	"deleted_at":      true,
}

// executeDataQueryRecords is the inner pure-Go path the
// `metacore_host.data_query` import calls into. It runs ONE org-scoped,
// equality-filtered SELECT against the logical table named by the guest,
// resolved through the SAME embedder-injected TableResolver data_mutate
// uses — NOT the addon-schema search_path of db_query, whose shadow schemas
// exist but hold no live rows in embedding hosts like ops. All failures
// surface inside the JSON envelope (`{success, data, meta}` v1, the same
// shape and error codes as data_mutate); the function never returns an
// error. No events are published — this is a pure read.
//
// Soft-delete awareness: when the table carries a `deleted_at` column the
// host appends `deleted_at IS NULL` so guests only ever see live rows —
// the mirror of data_mutate's soft delete. Detection runs through a
// zero-row probe (`SELECT * FROM <tbl> LIMIT 0`) whose result-set metadata
// yields the column set. The probe was chosen over try-filter-and-retry on
// a 42703 (undefined column) error because it is deterministic, needs no
// error-string sniffing across drivers, and mocks cleanly in tests; the
// extra round-trip is noise next to the import's 5s budget.
//
// No transaction: unlike db_query (which needs a tx to scope SET LOCAL
// search_path) data_query issues plain reads on the standalone handle —
// there is no session state to contain.
func executeDataQueryRecords(ctx context.Context, inv *invocation, reqJSON []byte) []byte {
	start := time.Now()
	addonKey := ""
	orgID := uuid.Nil
	if inv != nil {
		addonKey = inv.addonKey
		orgID = inv.orgID
	}
	fail := func(code, msg string) []byte {
		return dataMutateErr(addonKey, code, msg, orgID, start)
	}

	if inv == nil {
		return fail("invalid_request", "invocation context missing")
	}
	if len(reqJSON) == 0 {
		return fail("invalid_request", "empty request")
	}
	if len(reqJSON) > dataQueryMaxReqBytes {
		return fail("invalid_request",
			fmt.Sprintf("request exceeds %d byte cap", dataQueryMaxReqBytes))
	}

	var req dataQueryRequest
	if err := json.Unmarshal(reqJSON, &req); err != nil {
		return fail("invalid_request", "malformed request JSON: "+err.Error())
	}
	if !dataMutateIdentRe.MatchString(req.Table) {
		return fail("invalid_request",
			fmt.Sprintf("table must be a logical unqualified identifier (got %q)", req.Table))
	}
	if req.Limit < 0 {
		return fail("invalid_request", "limit must be >= 0")
	}
	limit := req.Limit
	if limit == 0 {
		limit = dataQueryDefaultLimit
	}
	if limit > dataQueryMaxLimit {
		limit = dataQueryMaxLimit
	}

	if orgID == uuid.Nil {
		return fail("invalid_request", "invocation has no bound orgID")
	}

	// Capability gate: db:read on the LOGICAL table name, same enforcer as
	// data_mutate / db_exec. A declared db:write also satisfies db:read
	// (writers can read what they write — security.CanReadModel semantics).
	if inv.enforcer != nil {
		if err := inv.enforcer.CheckCapability(addonKey, "db:read", req.Table); err != nil {
			return fail("forbidden", err.Error())
		}
	}

	// Decode + validate the equality filters. Scalars only per the
	// contract: string / number / bool / null. A null value compiles to
	// `col IS NULL` (an `= NULL` predicate never matches in SQL).
	whereCols := make([]string, 0, len(req.Where))
	whereVals := make(map[string]any, len(req.Where))
	for col, rv := range req.Where {
		if !dataMutateIdentRe.MatchString(col) {
			return fail("invalid_request", fmt.Sprintf("invalid where column %q", col))
		}
		if dataQueryBlockedWhereCols[col] {
			return fail("invalid_request",
				fmt.Sprintf("where column %q is host-managed and cannot be filtered by the guest", col))
		}
		dec := json.NewDecoder(strings.NewReader(string(rv)))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			return fail("invalid_request", fmt.Sprintf("where.%s: %v", col, err))
		}
		switch t := v.(type) {
		case nil, bool, string:
			whereVals[col] = t
		case json.Number:
			if i, err := t.Int64(); err == nil {
				whereVals[col] = i
			} else if f, err := t.Float64(); err == nil {
				whereVals[col] = f
			} else {
				return fail("invalid_request", fmt.Sprintf("where.%s: invalid number %q", col, t))
			}
		default:
			return fail("invalid_request",
				fmt.Sprintf("where.%s must be a scalar (string/number/bool/null)", col))
		}
		whereCols = append(whereCols, col)
	}
	sort.Strings(whereCols)

	if inv.db == nil {
		return fail("db_error", "host has no *gorm.DB configured")
	}

	// Resolve logical → physical via the embedder's resolver; identity when
	// none was injected. Same hook as data_mutate (Host.WithTableResolver).
	physical := req.Table
	if inv.resolveTable != nil {
		physical = inv.resolveTable(req.Table)
	}
	tbl, err := quoteQualifiedTable(physical)
	if err != nil {
		return fail("invalid_request", err.Error())
	}

	execCtx, cancel := context.WithTimeout(ctx, dataQueryDeadline)
	defer cancel()
	work := inv.db.WithContext(execCtx)

	// Zero-row probe: result-set metadata yields the column set, which drives
	// both the soft-delete filter (see the function doc) and the choice of
	// tenant-scoping strategy below.
	tblCols, err := tableColumns(work, tbl)
	if err != nil {
		return fail("db_error", err.Error())
	}
	softDelete := tblCols["deleted_at"]

	// Tenant scope. A table with `organization_id` gets the plain predicate; a
	// child table without one is scoped through its FK to an org-scoped parent,
	// and refused outright if it has no such parent (dataquery_orgscope.go).
	// Both forms reference $1 only, so the guest filters keep numbering from $2.
	orgConds, err := orgScopeClauses(work, tbl, tblCols, "$1")
	if err != nil {
		var noPath *errNoOrgScopePath
		if errors.As(err, &noPath) {
			return fail("forbidden", err.Error())
		}
		return fail("db_error", err.Error())
	}

	// Predicate order is deterministic: the host-injected org scope first,
	// then the guest filters sorted alphabetically, then the host-injected
	// soft-delete filter. LIMIT is an int the host clamped — never guest
	// text.
	conds := append([]string{}, orgConds...)
	args := []any{orgID}
	n := 1
	for _, col := range whereCols {
		v := whereVals[col]
		if v == nil {
			conds = append(conds, fmt.Sprintf("%s IS NULL", quoteIdent(col)))
			continue
		}
		n++
		conds = append(conds, fmt.Sprintf("%s = $%d", quoteIdent(col), n))
		args = append(args, v)
	}
	if softDelete {
		conds = append(conds, "deleted_at IS NULL")
	}
	stmt := fmt.Sprintf("SELECT * FROM %s WHERE %s LIMIT %d",
		tbl, strings.Join(conds, " AND "), limit)

	rows, err := work.Raw(stmt, args...).Rows()
	if err != nil {
		return fail("db_error", err.Error())
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		return fail("db_error", err.Error())
	}
	rowsOut := make([]map[string]any, 0)
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return fail("db_error", err.Error())
		}
		row := make(map[string]any, len(cols))
		for i, c := range cols {
			row[c] = jsonifyDBVal(vals[i])
		}
		rowsOut = append(rowsOut, row)
	}
	if err := rows.Err(); err != nil {
		return fail("db_error", err.Error())
	}

	env, _ := json.Marshal(map[string]any{
		"success": true,
		"data": map[string]any{
			"rows": rowsOut,
		},
		"meta": dataMutateMeta(addonKey, orgID, start),
	})
	if len(env) > dataQueryMaxRespBytes {
		return fail("db_error", "response exceeds size cap")
	}
	return env
}

// tableHasColumn probes the physical table with a zero-row SELECT and
// reports whether the named column exists in its result-set metadata.
// `tbl` is already quote-qualified by the caller; col is a host-owned
// literal, never guest input.
func tableHasColumn(work *gorm.DB, tbl, col string) (bool, error) {
	cols, err := tableColumns(work, tbl)
	if err != nil {
		return false, err
	}
	return cols[strings.ToLower(col)], nil
}
