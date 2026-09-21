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
	Where map[string]json.RawMessage `json:"where"` // equality filters; object form = operators
	Limit int                        `json:"limit"` // default 50, max 200
	// OrderBy/OrderDir give a stable order so a guest can page with a cursor
	// (`where: {id: {gt: last}}`, order_by "id"). OrderDir is "asc" (default)
	// or "desc". Without OrderBy the row order is unspecified, as before.
	OrderBy  string `json:"order_by"`
	OrderDir string `json:"order_dir"`
}

// dataQueryOps are the operators accepted in the object form of a where value,
// e.g. {"id": {"gt": "…"}} or {"state": {"in": ["a","b"]}}. Plain scalars stay
// equality, so every existing guest keeps working.
var dataQueryOps = map[string]string{"gt": ">", "gte": ">=", "lt": "<", "lte": "<=", "ne": "<>"}

// dataQueryMaxInList bounds an `in` list (parameters per query).
const dataQueryMaxInList = 200

// dataQueryPred is one compiled predicate of a guest filter.
type dataQueryPred struct {
	col  string
	op   string // "=", "IS NULL", ">", …, "IN"
	vals []any
}

// decodeDataQueryScalar validates a scalar filter value.
func decodeDataQueryScalar(col string, rv json.RawMessage) (any, error) {
	dec := json.NewDecoder(strings.NewReader(string(rv)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("where.%s: %v", col, err)
	}
	switch t := v.(type) {
	case nil, bool, string:
		return t, nil
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i, nil
		}
		if f, err := t.Float64(); err == nil {
			return f, nil
		}
		return nil, fmt.Errorf("where.%s: invalid number %q", col, t)
	}
	return nil, fmt.Errorf("where.%s must be a scalar (string/number/bool/null)", col)
}

// compileDataQueryWhere turns one where entry into predicates: a scalar is
// equality (null → IS NULL); an object holds operators gt/gte/lt/lte/ne (scalar)
// and in (non-empty scalar array).
func compileDataQueryWhere(col string, rv json.RawMessage) ([]dataQueryPred, error) {
	trimmed := strings.TrimSpace(string(rv))
	if !strings.HasPrefix(trimmed, "{") {
		v, err := decodeDataQueryScalar(col, rv)
		if err != nil {
			return nil, err
		}
		if v == nil {
			return []dataQueryPred{{col: col, op: "IS NULL"}}, nil
		}
		return []dataQueryPred{{col: col, op: "=", vals: []any{v}}}, nil
	}
	var ops map[string]json.RawMessage
	if err := json.Unmarshal(rv, &ops); err != nil || len(ops) == 0 {
		return nil, fmt.Errorf("where.%s: operator object must be non-empty", col)
	}
	names := make([]string, 0, len(ops))
	for k := range ops {
		names = append(names, k)
	}
	sort.Strings(names)
	var preds []dataQueryPred
	for _, name := range names {
		if name == "in" {
			var arr []json.RawMessage
			if err := json.Unmarshal(ops[name], &arr); err != nil || len(arr) == 0 || len(arr) > dataQueryMaxInList {
				return nil, fmt.Errorf("where.%s.in must be an array of 1..%d scalars", col, dataQueryMaxInList)
			}
			vals := make([]any, 0, len(arr))
			for _, e := range arr {
				v, err := decodeDataQueryScalar(col, e)
				if err != nil || v == nil {
					return nil, fmt.Errorf("where.%s.in: elements must be non-null scalars", col)
				}
				vals = append(vals, v)
			}
			preds = append(preds, dataQueryPred{col: col, op: "IN", vals: vals})
			continue
		}
		sqlOp, ok := dataQueryOps[name]
		if !ok {
			return nil, fmt.Errorf("where.%s: unknown operator %q", col, name)
		}
		v, err := decodeDataQueryScalar(col, ops[name])
		if err != nil || v == nil {
			return nil, fmt.Errorf("where.%s.%s: value must be a non-null scalar", col, name)
		}
		preds = append(preds, dataQueryPred{col: col, op: sqlOp, vals: []any{v}})
	}
	return preds, nil
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

	// Decode + validate the filters. A scalar is equality (null compiles to
	// `col IS NULL`; `= NULL` never matches in SQL); an object carries the
	// operators of dataQueryOps plus `in`.
	whereCols := make([]string, 0, len(req.Where))
	for col := range req.Where {
		whereCols = append(whereCols, col)
	}
	sort.Strings(whereCols)
	var preds []dataQueryPred
	for _, col := range whereCols {
		if !dataMutateIdentRe.MatchString(col) {
			return fail("invalid_request", fmt.Sprintf("invalid where column %q", col))
		}
		if dataQueryBlockedWhereCols[col] {
			return fail("invalid_request",
				fmt.Sprintf("where column %q is host-managed and cannot be filtered by the guest", col))
		}
		ps, err := compileDataQueryWhere(col, req.Where[col])
		if err != nil {
			return fail("invalid_request", err.Error())
		}
		preds = append(preds, ps...)
	}
	orderSQL := ""
	if req.OrderBy != "" {
		if !dataMutateIdentRe.MatchString(req.OrderBy) || dataQueryBlockedWhereCols[req.OrderBy] {
			return fail("invalid_request", fmt.Sprintf("invalid order_by %q", req.OrderBy))
		}
		dir := strings.ToLower(req.OrderDir)
		switch dir {
		case "", "asc":
			dir = "ASC"
		case "desc":
			dir = "DESC"
		default:
			return fail("invalid_request", "order_dir must be asc or desc")
		}
		orderSQL = " ORDER BY " + quoteIdent(req.OrderBy) + " " + dir
	}

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
	for _, p := range preds {
		switch p.op {
		case "IS NULL":
			conds = append(conds, fmt.Sprintf("%s IS NULL", quoteIdent(p.col)))
		case "IN":
			ph := make([]string, 0, len(p.vals))
			for _, v := range p.vals {
				n++
				ph = append(ph, fmt.Sprintf("$%d", n))
				args = append(args, v)
			}
			conds = append(conds, fmt.Sprintf("%s IN (%s)", quoteIdent(p.col), strings.Join(ph, ", ")))
		default:
			n++
			conds = append(conds, fmt.Sprintf("%s %s $%d", quoteIdent(p.col), p.op, n))
			args = append(args, p.vals[0])
		}
	}
	if softDelete {
		conds = append(conds, "deleted_at IS NULL")
	}
	stmt := fmt.Sprintf("SELECT * FROM %s WHERE %s%s LIMIT %d",
		tbl, strings.Join(conds, " AND "), orderSQL, limit)

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
