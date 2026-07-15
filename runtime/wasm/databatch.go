package wasm

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/google/uuid"
)

// data_batch host import limits (ABI v1.7). The request cap is the same 64 KiB
// as data_mutate — the ABI's frozen request ceiling — so a batch trades per-row
// headroom for atomicity: many small rows, not a few huge ones. A batch that
// would exceed the cap must be split by the guest. dataBatchMaxMutations bounds
// the row count independently so a pathological all-tiny-rows payload still can
// not open an unbounded transaction. See docs/wasm-abi.md § 16.
const (
	dataBatchMaxReqBytes     = 64 * 1024
	dataBatchMaxRespBytes    = 8 * 1024 * 1024
	dataBatchMaxMutations    = 100
	dataBatchDeadline        = 10 * time.Second
	DataBatchEnvelopeVersion = 1
)

// dataBatchRequest is the guest-supplied JSON request: an ordered list of
// mutations executed under ONE org-scoped transaction. `organization_id` is
// deliberately absent everywhere — tenant scope ALWAYS comes from the
// invocation context, never from the guest (same rule as data_mutate).
type dataBatchRequest struct {
	Mutations []dataMutateRequest `json:"mutations"`
}

// dataBatchRow is one committed result in the response `results` array, in the
// same order as the request's mutations.
type dataBatchRow struct {
	ID     string         `json:"id"`
	Model  string         `json:"model"`
	Action string         `json:"action"`
	Before map[string]any `json:"before,omitempty"`
	After  map[string]any `json:"after,omitempty"`
}

// preparedMutation is one validated+decoded mutation ready to run under the
// shared transaction, holding everything applyMutation needs so no manifest /
// resolver work happens inside the transaction window.
type preparedMutation struct {
	req  *dataMutateRequest
	data map[string]any
	inc  map[string]any
	tbl  string // already quoted physical table
}

// executeDataBatch runs a list of create/update/delete mutations across one or
// more models in a SINGLE org-scoped transaction, publishing one
// *dynamic.CanonicalEvent per row AFTER the commit. It is the atomic multi-row
// sibling of executeDataMutate: it reuses the same per-row machinery
// (applyMutation) but owns the commit boundary itself, so "sale decrements
// stock + writes the ledger + updates the weighted cost" either all lands or
// none of it does. All failures surface inside the JSON envelope — the function
// never returns an error (wire shape `{success, data, meta}`, docs/wasm-abi.md
// § 16). A failure names the offending mutation index so the guest can locate
// the bad row.
func executeDataBatch(ctx context.Context, inv *invocation, reqJSON []byte) []byte {
	start := time.Now()
	addonKey := ""
	orgID := uuid.Nil
	if inv != nil {
		addonKey = inv.addonKey
		orgID = inv.orgID
	}
	fail := func(code, msg string) []byte {
		return dataBatchErr(addonKey, code, msg, orgID, start)
	}

	if inv == nil {
		return fail("invalid_request", "invocation context missing")
	}
	if len(reqJSON) == 0 {
		return fail("invalid_request", "empty request")
	}
	if len(reqJSON) > dataBatchMaxReqBytes {
		return fail("invalid_request",
			fmt.Sprintf("request exceeds %d byte cap", dataBatchMaxReqBytes))
	}

	var req dataBatchRequest
	if err := json.Unmarshal(reqJSON, &req); err != nil {
		return fail("invalid_request", "malformed request JSON: "+err.Error())
	}
	if len(req.Mutations) == 0 {
		return fail("invalid_request", "mutations is empty")
	}
	if len(req.Mutations) > dataBatchMaxMutations {
		return fail("invalid_request",
			fmt.Sprintf("batch of %d exceeds %d mutation cap", len(req.Mutations), dataBatchMaxMutations))
	}

	// The canonical events are the reason this import exists — without a bus
	// the batch must not run at all (fail-fast, before any DB work).
	if inv.bus == nil {
		return fail("bus_unavailable", "host has no events.Bus configured")
	}
	if orgID == uuid.Nil {
		return fail("invalid_request", "invocation has no bound orgID")
	}
	if inv.db == nil {
		return fail("db_error", "host has no *gorm.DB configured")
	}

	// Validate + capability-gate + decode EVERY mutation up front so a bad row
	// is rejected before we open the transaction (no half-open tx on a
	// validation error). The capability gate is db:write on each LOGICAL table,
	// the same enforcer as data_mutate — a batch touching N tables needs the
	// grant for all N.
	prepared := make([]preparedMutation, len(req.Mutations))
	for i := range req.Mutations {
		m := req.Mutations[i]
		if err := validateDataMutateRequest(&m); err != nil {
			return fail("invalid_request", fmt.Sprintf("mutations[%d]: %s", i, err.Error()))
		}
		if inv.enforcer != nil {
			if err := inv.enforcer.CheckCapability(addonKey, "db:write", m.Table); err != nil {
				return fail("forbidden", fmt.Sprintf("mutations[%d]: %s", i, err.Error()))
			}
		}
		physical := m.Table
		if inv.resolveTable != nil {
			physical = inv.resolveTable(m.Table)
		}
		tbl, err := quoteQualifiedTable(physical)
		if err != nil {
			return fail("invalid_request", fmt.Sprintf("mutations[%d]: %s", i, err.Error()))
		}
		data, err := decodeDataMutateCols(m.Data)
		if err != nil {
			return fail("invalid_request", fmt.Sprintf("mutations[%d]: data: %s", i, err.Error()))
		}
		inc, err := decodeDataMutateCols(m.Inc)
		if err != nil {
			return fail("invalid_request", fmt.Sprintf("mutations[%d]: inc: %s", i, err.Error()))
		}
		for col, v := range inc {
			switch v.(type) {
			case int64, float64:
			default:
				return fail("invalid_request",
					fmt.Sprintf("mutations[%d]: inc.%s must be a number", i, col))
			}
			if _, dup := data[col]; dup {
				return fail("invalid_request",
					fmt.Sprintf("mutations[%d]: column %q present in both data and inc", i, col))
			}
		}
		prepared[i] = preparedMutation{req: &req.Mutations[i], data: data, inc: inc, tbl: tbl}
	}

	execCtx, cancel := context.WithTimeout(ctx, dataBatchDeadline)
	defer cancel()

	work := inv.db.WithContext(execCtx).Begin()
	if work.Error != nil {
		return fail("db_error", work.Error.Error())
	}

	now := time.Now().UTC()
	results := make([]*mutationResult, len(prepared))
	for i, p := range prepared {
		res, code, mErr := applyMutation(work, p.req, p.data, p.inc, orgID, p.tbl, now, dynamic.ActorIDFromContext(ctx))
		if mErr != nil {
			_ = work.Rollback()
			return fail(code, fmt.Sprintf("mutations[%d]: %s", i, mErr.Error()))
		}
		// Declarative guards (Host.WithMutationGuard) check the post-mutation
		// row inside the shared transaction — one violated constraint rolls the
		// ENTIRE batch back. Deletes are exempt (no resulting row state).
		if inv.mutationGuard != nil && res.action != "deleted" {
			if gErr := inv.mutationGuard(execCtx, p.req.Table, res.after); gErr != nil {
				_ = work.Rollback()
				return fail("constraint_violation", fmt.Sprintf("mutations[%d]: %s", i, gErr.Error()))
			}
		}
		results[i] = res
	}

	if err := work.Commit().Error; err != nil {
		return fail("db_error", err.Error())
	}

	// Post-commit canonical events — the bus sees only committed state. One
	// event per row, under the CALLER's addon namespace, mirroring data_mutate.
	// Publish errors are logged and swallowed: the data is committed, its
	// side-effects must not undo it.
	rows := make([]dataBatchRow, len(results))
	for i, res := range results {
		m := prepared[i].req
		event := fmt.Sprintf("%s.%s.%s", addonKey, m.Model, res.action)
		payload := &dynamic.CanonicalEvent{
			ID:            res.rowID,
			Model:         m.Model,
			Action:        res.action,
			ActorID:       dynamic.ActorIDFromContext(ctx),
			AddonKey:      addonKey,
			CorrelationID: dynamic.CorrelationIDFromContext(ctx),
			Before:        res.before,
			After:         res.after,
		}
		if err := inv.bus.Publish(ctx, "kernel", event, orgID, payload); err != nil && inv.logger != nil {
			inv.logger.Printf("metacore.wasm data_batch publish_error addon=%s event=%s err=%v",
				addonKey, event, err)
		}
		row := dataBatchRow{ID: res.rowID, Model: m.Model, Action: res.action, Before: res.before, After: res.after}
		if m.Returning != nil && !*m.Returning {
			// `returning:false` trims the after row from the RESPONSE only — the
			// canonical event above always carries the full rows.
			row.After = nil
		}
		rows[i] = row
	}

	env, _ := json.Marshal(map[string]any{
		"success": true,
		"data":    map[string]any{"count": len(rows), "results": rows},
		"meta":    dataBatchMeta(addonKey, orgID, start),
	})
	if len(env) > dataBatchMaxRespBytes {
		// The batch is COMMITTED at this point — returning an error here would
		// read as a failure and provoke a retry (double-write). Degrade instead:
		// keep success:true, drop the row bodies (ids/actions survive) and flag
		// the truncation so the guest can re-read what it needs via data_query.
		slim := make([]dataBatchRow, len(rows))
		for i, r := range rows {
			slim[i] = dataBatchRow{ID: r.ID, Model: r.Model, Action: r.Action}
		}
		meta := dataBatchMeta(addonKey, orgID, start)
		meta["truncated"] = true
		env, _ = json.Marshal(map[string]any{
			"success": true,
			"data":    map[string]any{"count": len(slim), "results": slim},
			"meta":    meta,
		})
	}
	return env
}

func dataBatchMeta(addonKey string, orgID uuid.UUID, start time.Time) map[string]any {
	meta := map[string]any{
		"addon":           addonKey,
		"durationMs":      time.Since(start).Milliseconds(),
		"envelopeVersion": DataBatchEnvelopeVersion,
	}
	if orgID != uuid.Nil {
		meta["orgId"] = orgID.String()
	}
	return meta
}

// dataBatchErr builds the failure envelope. `code` is one of: forbidden |
// not_found | invalid_request | bus_unavailable | db_error. On a per-row
// failure the message is prefixed with the offending `mutations[i]` index.
func dataBatchErr(addonKey, code, message string, orgID uuid.UUID, start time.Time) []byte {
	b, _ := json.Marshal(map[string]any{
		"success": false,
		"error":   map[string]any{"code": code, "message": message},
		"meta":    dataBatchMeta(addonKey, orgID, start),
	})
	return b
}
