package wasm

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// data_mutate host import limits. Request/deadline/response mirror db_exec
// (docs/wasm-abi.md § 10.5) per the frozen contract — request cap is 64 KiB
// because the payload carries column data, not just SQL text.
const (
	dataMutateMaxReqBytes  = 64 * 1024
	dataMutateMaxRespBytes = 8 * 1024 * 1024
	dataMutateDeadline     = 5 * time.Second
)

// DataMutateEnvelopeVersion is the wire-format version of the JSON envelope
// returned by the `metacore_host.data_mutate` import — the canonical
// `{success, data, meta}` shape documented in docs/wasm-abi.md § 14.
const DataMutateEnvelopeVersion = 1

// dataMutateRequest is the guest-supplied JSON request. `organization_id`
// is deliberately absent: tenant scope ALWAYS comes from the invocation
// context (same rule as event_emit), never from the guest.
type dataMutateRequest struct {
	Op        string                     `json:"op"`    // create | update | delete
	Table     string                     `json:"table"` // logical, unqualified
	Model     string                     `json:"model"` // canonical ModelKey → CanonicalEvent.Model
	ID        string                     `json:"id"`    // required for update/delete; optional on create
	Data      map[string]json.RawMessage `json:"data"`  // create: columns; update: absolute SETs
	Inc       map[string]json.RawMessage `json:"inc"`   // update only: SET col = col + delta (atomic)
	Returning *bool                      `json:"returning"`
}

// dataMutateIdentRe is the shape every logical table / column name must
// match before it is interpolated (quoted) into SQL. Postgres identifiers
// are capped at 63 bytes; lowercase snake_case is the kernel-wide column
// convention (manifest v3 enforces it at publish time — this is the
// runtime twin, defense in depth like SeedForOrg's declared-column gate).
var dataMutateIdentRe = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// dataMutateReservedCols are host-stamped columns a guest may never supply
// through data/inc: `organization_id` is the tenant boundary, `id` rides on
// the top-level request field, and the timestamps are stamped by the host.
var dataMutateReservedCols = map[string]bool{
	"id":              true,
	"organization_id": true,
	"created_at":      true,
	"updated_at":      true,
	"deleted_at":      true,
}

// executeDataMutate is the inner pure-Go path the `metacore_host.data_mutate`
// import calls into. It performs ONE org-scoped row mutation (create / update
// / delete) on the logical table named by the guest and — after the commit —
// publishes the matching *dynamic.CanonicalEvent on the host bus. All
// failures surface inside the JSON envelope; the function never returns an
// error (wire shape: `{success, data, meta}` — docs/wasm-abi.md § 14).
//
// Transaction shape: data_mutate ALWAYS opens its own short-lived transaction
// on inv.db — it never piggy-backs on an action handler's open tx (unlike
// db_exec). The canonical event must describe COMMITTED state; publishing
// from inside a caller-owned transaction would emit phantom events when the
// surrounding action rolls back, so the import owns its commit boundary.
//
// Table resolution: the physical table comes from the host-injected
// TableResolver (Host.WithTableResolver) — the same resolution the embedding
// host's dynamic CRUD runtime uses (in ops: unqualified → public.*). It
// deliberately does NOT use the addon-schema search_path that db_exec sets:
// writing into addon_<key>.* would be invisible to the live UI data.
func executeDataMutate(ctx context.Context, inv *invocation, reqJSON []byte) []byte {
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
	if len(reqJSON) > dataMutateMaxReqBytes {
		return fail("invalid_request",
			fmt.Sprintf("request exceeds %d byte cap", dataMutateMaxReqBytes))
	}

	var req dataMutateRequest
	if err := json.Unmarshal(reqJSON, &req); err != nil {
		return fail("invalid_request", "malformed request JSON: "+err.Error())
	}
	// Guests used to put the deterministic row id in data.id. The contract
	// stamps id on the request field (and rejects it in data). Lift it so a
	// create does not die as invalid_request after a marketplace upgrade —
	// that failure used to be marked delivered and left no work order in prod.
	liftGuestCreateID(&req)
	if err := validateDataMutateRequest(&req); err != nil {
		return fail("invalid_request", err.Error())
	}

	// The canonical event is the reason this import exists — without a bus
	// the mutation must not run at all (fail-fast, before any DB work).
	if inv.bus == nil {
		return fail("bus_unavailable", "host has no events.Bus configured")
	}
	if orgID == uuid.Nil {
		return fail("invalid_request", "invocation has no bound orgID")
	}

	// Capability gate: db:write on the LOGICAL table name, same enforcer as
	// db_exec. A nil-lookup (unregistered addon) is denied in enforce mode.
	if inv.enforcer != nil {
		if err := inv.enforcer.CheckCapability(addonKey, "db:write", req.Table); err != nil {
			return fail("forbidden", err.Error())
		}
	}

	// Resolve logical → physical via the embedder's resolver; identity when
	// none was injected.
	physical := req.Table
	if inv.resolveTable != nil {
		physical = inv.resolveTable(req.Table)
	}
	tbl, err := quoteQualifiedTable(physical)
	if err != nil {
		return fail("invalid_request", err.Error())
	}

	// Decode column values with the same driver-friendly conversion db_exec
	// args use (json.Number → int64/float64, $uuid/$ts/$bytes markers).
	data, err := decodeDataMutateCols(req.Data)
	if err != nil {
		return fail("invalid_request", "data: "+err.Error())
	}
	inc, err := decodeDataMutateCols(req.Inc)
	if err != nil {
		return fail("invalid_request", "inc: "+err.Error())
	}
	for col, v := range inc {
		switch v.(type) {
		case int64, float64:
		default:
			return fail("invalid_request",
				fmt.Sprintf("inc.%s must be a number", col))
		}
		if _, dup := data[col]; dup {
			return fail("invalid_request",
				fmt.Sprintf("column %q present in both data and inc", col))
		}
	}

	if inv.db == nil {
		return fail("db_error", "host has no *gorm.DB configured")
	}

	execCtx, cancel := context.WithTimeout(ctx, dataMutateDeadline)
	defer cancel()

	work := inv.db.WithContext(execCtx).Begin()
	if work.Error != nil {
		return fail("db_error", work.Error.Error())
	}
	rollback := func() { _ = work.Rollback() }

	now := time.Now().UTC()
	res, code, mErr := applyMutation(work, &req, data, inc, orgID, tbl, now, dynamic.ActorIDFromContext(ctx))
	if mErr != nil {
		rollback()
		return fail(code, mErr.Error())
	}
	action, rowID, before, after := res.action, res.rowID, res.before, res.after

	// Declarative guards (Host.WithMutationGuard) evaluate the post-mutation
	// row INSIDE the transaction, so a violated constraint (stock.quantity >= 0)
	// rolls the write back exactly like on the Service.Create/Update path.
	// Deletes are exempt: guards predicate over resulting row state.
	if inv.mutationGuard != nil && action != "deleted" {
		if gErr := inv.mutationGuard(execCtx, req.Table, after); gErr != nil {
			rollback()
			return fail("constraint_violation", gErr.Error())
		}
	}

	if err := work.Commit().Error; err != nil {
		return fail("db_error", err.Error())
	}

	// Post-commit canonical event — the bus sees only committed state.
	// Producer is "kernel" (trusted host bookkeeping): the event name is
	// host-constructed under the CALLER's addon namespace, the guest cannot
	// spoof another addon's prefix, and the mutation itself was already
	// gated by db:write — requiring a separate event:emit declaration would
	// silently break the import's whole purpose. Publish errors are logged
	// and swallowed: the data is committed, side-effects must not undo it
	// (same policy as dynamic.Service.publishCanonical).
	event := fmt.Sprintf("%s.%s.%s", addonKey, req.Model, action)
	payload := &dynamic.CanonicalEvent{
		ID:     rowID,
		Model:  req.Model,
		Action: action,
		// ActorID rides in from the delivery ctx (the dispatcher re-attaches
		// the originating event's actor): the human whose action caused this
		// chain, so the audit trail never shows an anonymous system actor for
		// a user-driven side effect.
		ActorID:       dynamic.ActorIDFromContext(ctx),
		AddonKey:      addonKey,
		CorrelationID: dynamic.CorrelationIDFromContext(ctx),
		Before:        before,
		After:         after,
	}
	if err := inv.bus.Publish(ctx, "kernel", event, orgID, payload); err != nil && inv.logger != nil {
		inv.logger.Printf("metacore.wasm data_mutate publish_error addon=%s event=%s err=%v",
			addonKey, event, err)
	}

	dataOut := map[string]any{
		"id":     rowID,
		"before": before,
		"after":  after,
	}
	if req.Returning != nil && !*req.Returning {
		// `returning:false` trims the (potentially large) after row from the
		// RESPONSE only — the canonical event always carries the full rows.
		dataOut["after"] = nil
	}
	env, _ := json.Marshal(map[string]any{
		"success": true,
		"data":    dataOut,
		"meta":    dataMutateMeta(addonKey, orgID, start),
	})
	if len(env) > dataMutateMaxRespBytes {
		return fail("db_error", "response exceeds size cap")
	}
	return env
}

// mutationResult is the committed-state snapshot applyMutation hands back so
// the caller can build the canonical event (data_mutate) or accumulate a
// per-row result set (data_batch).
type mutationResult struct {
	action string // created | updated | deleted
	rowID  string
	before map[string]any
	after  map[string]any
}

// applyMutation executes ONE decoded mutation (create/update/delete) on the
// already-open transaction `work`. It NEVER commits or rolls back — the caller
// owns the transaction boundary, which is what lets data_batch run many
// mutations under a single commit. On failure it returns a stable error code
// (db_error | not_found) alongside the error so the caller can surface it in
// the JSON envelope; `data`/`inc` are the pre-decoded column maps and `tbl` is
// the already-quoted physical table. `actorID` is the acting user carried by
// the invocation context ("" for pure system work): a create stamps it into
// created_by_id when the table has that column, so rows born from a
// user-driven side effect (a transfer's destination stock row) show the human
// behind the chain instead of a permanent "N/A".
func applyMutation(work *gorm.DB, req *dataMutateRequest, data, inc map[string]any, orgID uuid.UUID, tbl string, now time.Time, actorID string) (*mutationResult, string, error) {
	out := &mutationResult{rowID: req.ID}
	switch req.Op {
	case "create":
		out.action = "created"
		if out.rowID == "" {
			out.rowID = uuid.NewString()
		}
		cols := make([]string, 0, len(data)+5)
		vals := map[string]any{
			"id":         out.rowID,
			"created_at": now,
			"updated_at": now,
		}
		// ONE zero-row probe drives both host-stamped columns below. It used to
		// run only when an actor was present (for created_by_id); the tenant
		// column needs it too, so it is hoisted and shared rather than probed
		// twice.
		columns, err := tableColumns(work, tbl)
		if err != nil {
			return nil, "db_error", err
		}

		// Tenant column, when the table has one. A CHILD table (a document's
		// line items) does not: inserting organization_id there died with 42703.
		// Omitting it is only half the fix — a row with no tenant column of its
		// own belongs to an organization solely through its parent, so that link
		// is verified before the INSERT (datamutate_orgscope.go). Tables WITH the
		// column keep exactly the behaviour they had.
		if columns["organization_id"] {
			vals["organization_id"] = orgID
		} else if err := verifyChildCreateTenancy(work, tbl, data, orgID); err != nil {
			var unverifiable *errChildCreateUnverifiable
			if errors.As(err, &unverifiable) {
				return nil, "forbidden", err
			}
			return nil, "db_error", err
		}

		if _, err := uuid.Parse(actorID); err == nil {
			if _, supplied := data["created_by_id"]; !supplied && columns["created_by_id"] {
				vals["created_by_id"] = actorID
			}
		}
		for c, v := range data {
			vals[c] = v
		}
		for c := range vals {
			cols = append(cols, c)
		}
		sort.Strings(cols)
		quoted := make([]string, len(cols))
		marks := make([]string, len(cols))
		args := make([]any, len(cols))
		for i, c := range cols {
			quoted[i] = quoteIdent(c)
			marks[i] = fmt.Sprintf("$%d", i+1)
			args[i] = vals[c]
		}
		stmt := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) RETURNING *",
			tbl, strings.Join(quoted, ", "), strings.Join(marks, ", "))
		after, err := queryOneRow(work, stmt, args...)
		if err != nil {
			return nil, "db_error", err
		}
		if after == nil {
			return nil, "db_error", fmt.Errorf("INSERT returned no row")
		}
		out.after = after

	case "update":
		out.action = "updated"
		// Tenant predicate. A child table has no organization_id of its own —
		// splicing it in died with 42703 — so it is scoped through its parent
		// exactly like the read path (dataquery_orgscope.go). A row whose parent
		// belongs to another organization simply does not match, so it reports
		// not_found: the same answer a foreign row already gets on a table WITH
		// the column, and it leaks no existence.
		columns, err := tableColumns(work, tbl)
		if err != nil {
			return nil, "db_error", err
		}
		scope, err := resolveOrgScope(work, tbl, columns)
		if err != nil {
			return nil, "db_error", err
		}
		before, err := queryOneRow(work,
			fmt.Sprintf("SELECT * FROM %s WHERE id = $1 AND %s", tbl, scope.predicate("$2")),
			out.rowID, orgID)
		if err != nil {
			return nil, "db_error", err
		}
		if before == nil {
			return nil, "not_found",
				fmt.Errorf("row %s not found in %s for this organization", out.rowID, req.Table)
		}
		out.before = before
		sets := make([]string, 0, len(data)+len(inc)+1)
		args := make([]any, 0, len(data)+len(inc)+3)
		n := 0
		for _, c := range sortedCols(data) {
			n++
			sets = append(sets, fmt.Sprintf("%s = $%d", quoteIdent(c), n))
			args = append(args, data[c])
		}
		for _, c := range sortedCols(inc) {
			n++
			q := quoteIdent(c)
			sets = append(sets, fmt.Sprintf("%s = %s + $%d", q, q, n))
			args = append(args, inc[c])
		}
		n++
		sets = append(sets, fmt.Sprintf(`"updated_at" = $%d`, n))
		args = append(args, now)
		stmt := fmt.Sprintf("UPDATE %s SET %s WHERE id = $%d AND %s RETURNING *",
			tbl, strings.Join(sets, ", "), n+1, scope.predicate(fmt.Sprintf("$%d", n+2)))
		args = append(args, out.rowID, orgID)
		after, err := queryOneRow(work, stmt, args...)
		if err != nil {
			return nil, "db_error", err
		}
		if after == nil {
			return nil, "not_found",
				fmt.Errorf("row %s vanished during update in %s", out.rowID, req.Table)
		}
		out.after = after

	case "delete":
		out.action = "deleted"
		// Same tenant predicate as update: plain column when the table has one,
		// parent EXISTS when it does not.
		columns, err := tableColumns(work, tbl)
		if err != nil {
			return nil, "db_error", err
		}
		scope, err := resolveOrgScope(work, tbl, columns)
		if err != nil {
			return nil, "db_error", err
		}
		before, err := queryOneRow(work,
			fmt.Sprintf("SELECT * FROM %s WHERE id = $1 AND %s", tbl, scope.predicate("$2")),
			out.rowID, orgID)
		if err != nil {
			return nil, "db_error", err
		}
		if before == nil {
			return nil, "not_found",
				fmt.Errorf("row %s not found in %s for this organization", out.rowID, req.Table)
		}
		out.before = before
		// Soft delete when the table carries a deleted_at column — detected
		// from the snapshot's column set, so no information_schema round-trip.
		if deletedAt, soft := before["deleted_at"]; soft {
			if deletedAt != nil {
				return nil, "not_found",
					fmt.Errorf("row %s in %s is already deleted", out.rowID, req.Table)
			}
			res := work.Exec(fmt.Sprintf(
				`UPDATE %s SET "deleted_at" = $1, "updated_at" = $2 WHERE id = $3 AND %s`,
				tbl, scope.predicate("$4")), now, now, out.rowID, orgID)
			if res.Error != nil {
				return nil, "db_error", res.Error
			}
			if res.RowsAffected == 0 {
				return nil, "not_found",
					fmt.Errorf("row %s vanished during delete in %s", out.rowID, req.Table)
			}
		} else {
			res := work.Exec(fmt.Sprintf(
				"DELETE FROM %s WHERE id = $1 AND %s", tbl, scope.predicate("$2")),
				out.rowID, orgID)
			if res.Error != nil {
				return nil, "db_error", res.Error
			}
			if res.RowsAffected == 0 {
				return nil, "not_found",
					fmt.Errorf("row %s vanished during delete in %s", out.rowID, req.Table)
			}
		}
	}
	return out, "", nil
}

// validateDataMutateRequest applies the structural rules of the contract:
// op enum, identifier-shaped logical table, non-empty model, uuid ids, and
// the data/inc field matrix per op. Reserved (host-stamped) columns are
// rejected so the tenant boundary and the timestamps stay host-owned.
func validateDataMutateRequest(req *dataMutateRequest) error {
	switch req.Op {
	case "create", "update", "delete":
	default:
		return fmt.Errorf("op must be create|update|delete (got %q)", req.Op)
	}
	if !dataMutateIdentRe.MatchString(req.Table) {
		return fmt.Errorf("table must be a logical unqualified identifier (got %q)", req.Table)
	}
	if strings.TrimSpace(req.Model) == "" {
		return fmt.Errorf("model is required")
	}
	switch req.Op {
	case "create":
		if req.ID != "" {
			if _, err := uuid.Parse(req.ID); err != nil {
				return fmt.Errorf("id is not a valid uuid: %v", err)
			}
		}
		if len(req.Inc) > 0 {
			return fmt.Errorf("inc is only valid on update")
		}
	case "update":
		if _, err := uuid.Parse(req.ID); err != nil {
			return fmt.Errorf("id is required and must be a valid uuid: %v", err)
		}
		if len(req.Data) == 0 && len(req.Inc) == 0 {
			return fmt.Errorf("update requires data and/or inc")
		}
	case "delete":
		if _, err := uuid.Parse(req.ID); err != nil {
			return fmt.Errorf("id is required and must be a valid uuid: %v", err)
		}
		if len(req.Data) > 0 || len(req.Inc) > 0 {
			return fmt.Errorf("delete takes no data/inc")
		}
	}
	for col := range req.Data {
		if err := validateDataMutateCol(col); err != nil {
			return err
		}
	}
	for col := range req.Inc {
		if err := validateDataMutateCol(col); err != nil {
			return err
		}
	}
	return nil
}

// liftGuestCreateID moves data.id onto the top-level request id on create.
// The host still stamps the column; the guest may only propose the uuid.
func liftGuestCreateID(req *dataMutateRequest) {
	if req == nil || req.Op != "create" || req.Data == nil {
		return
	}
	raw, ok := req.Data["id"]
	if !ok {
		return
	}
	delete(req.Data, "id")
	if strings.TrimSpace(req.ID) != "" {
		return
	}
	var id string
	if err := json.Unmarshal(raw, &id); err != nil {
		return
	}
	req.ID = strings.TrimSpace(id)
}

func validateDataMutateCol(col string) error {
	if !dataMutateIdentRe.MatchString(col) {
		return fmt.Errorf("invalid column name %q", col)
	}
	if dataMutateReservedCols[col] {
		return fmt.Errorf("column %q is host-stamped and cannot be set by the guest", col)
	}
	return nil
}

// decodeDataMutateCols converts the raw JSON column map into driver-friendly
// Go values using the same scalar rules as db_exec args (docs/wasm-abi.md
// § 9.6): numbers ride as int64/float64, objects must be one of the $uuid /
// $ts / $bytes markers. Nested objects/arrays are rejected — guests that
// target jsonb columns should pre-serialise to a string.
func decodeDataMutateCols(raw map[string]json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make(map[string]any, len(raw))
	for col, rv := range raw {
		dec := json.NewDecoder(strings.NewReader(string(rv)))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			return nil, fmt.Errorf("column %q: %w", col, err)
		}
		dv, err := decodeOneDBArg(v)
		if err != nil {
			return nil, fmt.Errorf("column %q: %w", col, err)
		}
		out[col] = dv
	}
	return out, nil
}

// quoteQualifiedTable quotes a resolver-produced physical table name. The
// resolver may return either an unqualified name ("stock") or a
// schema-qualified one ("public.stock"); each part is quoted independently.
func quoteQualifiedTable(physical string) (string, error) {
	physical = strings.TrimSpace(physical)
	if physical == "" {
		return "", fmt.Errorf("table resolver returned an empty physical name")
	}
	parts := strings.Split(physical, ".")
	if len(parts) > 2 {
		return "", fmt.Errorf("invalid physical table name %q", physical)
	}
	for i, p := range parts {
		if p == "" {
			return "", fmt.Errorf("invalid physical table name %q", physical)
		}
		parts[i] = quoteIdent(p)
	}
	return strings.Join(parts, "."), nil
}

// queryOneRow runs stmt expecting zero or one row and returns it as a JSON
// friendly map (nil when no row matched). More than one row is a programming
// error upstream (ids are primary keys) — the extra rows are ignored.
func queryOneRow(work *gorm.DB, stmt string, args ...any) (map[string]any, error) {
	rows, err := work.Raw(stmt, args...).Rows()
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	row, err := scanSingleRow(rows)
	if err != nil {
		return nil, err
	}
	return row, rows.Err()
}

func scanSingleRow(rows *sql.Rows) (map[string]any, error) {
	if !rows.Next() {
		return nil, nil
	}
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return nil, err
	}
	out := make(map[string]any, len(cols))
	for i, c := range cols {
		out[c] = jsonifyDBVal(vals[i])
	}
	return out, nil
}

func sortedCols(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for c := range m {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

func dataMutateMeta(addonKey string, orgID uuid.UUID, start time.Time) map[string]any {
	meta := map[string]any{
		"addon":           addonKey,
		"durationMs":      time.Since(start).Milliseconds(),
		"envelopeVersion": DataMutateEnvelopeVersion,
	}
	if orgID != uuid.Nil {
		meta["orgId"] = orgID.String()
	}
	return meta
}

// dataMutateErr builds the failure envelope per docs/wasm-abi.md § 14.5.
// `code` is one of: forbidden | not_found | invalid_request |
// bus_unavailable | db_error | constraint_violation.
func dataMutateErr(addonKey, code, message string, orgID uuid.UUID, start time.Time) []byte {
	b, _ := json.Marshal(map[string]any{
		"success": false,
		"error":   map[string]any{"code": code, "message": message},
		"meta":    dataMutateMeta(addonKey, orgID, start),
	})
	return b
}
