package inbound

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/modelbase"
)

// record.action, record.transition and record.restore: three ways of taking a
// row to any state the model declares, without a new action per case. They
// reuse what the model already has (dynamic.Service.ExecAction, the stage
// machine behind Service.Update, Service.Restore) and add only the inbound
// walls of record.delete: rows are selected ONLY by match, inside the route's
// organization and scope, capped by max_rows, and gated by DynamicExecutor.Authorize.

// maxActionParamKeys bounds the payload one record.action can build.
const maxActionParamKeys = 64

// reasonParam is the one payload key every reason_required action reads even
// when its Fields do not declare it (dynamic.ActionReason).
const reasonParam = "reason"

// ingestable reports whether an action may be run from an inbound event: it
// must be opted in (`ingest: true`) and need nothing a signed event cannot give
// (approval, supervisor PIN, typed confirmation) and have a record to act on.
// The v3 manifest already refuses those combinations; this is the second line
// for a host that builds its ActionDefs another way.
func ingestable(def *manifest.ActionDef) bool {
	if def == nil || !def.Ingest {
		return false
	}
	if def.Approval != nil || def.SupervisorPolicy != "" || def.ConfirmTyped != nil {
		return false
	}
	return def.Placement != "table" && def.Placement != "create"
}

// actionFieldKeys lists the payload keys the action declares (Fields and the
// fields of every wizard step).
func actionFieldKeys(def *manifest.ActionDef) map[string]bool {
	keys := map[string]bool{}
	add := func(fs []manifest.FieldDef) {
		for _, f := range fs {
			k := f.Key
			if k == "" {
				k = f.Name
			}
			if k != "" {
				keys[k] = true
			}
		}
	}
	add(def.Fields)
	for _, s := range def.Steps {
		add(s.Fields)
	}
	return keys
}

// checkStateOpModel is the registration check of record.action,
// record.transition and record.restore against the model.
func (d *DynamicExecutor) checkStateOpModel(ctx context.Context, who string, a Action) error {
	st, err := d.st()
	if err != nil {
		return err
	}
	if a.MaxRows > d.maxDeleteRows() {
		return fmt.Errorf("%w: %s: max_rows %d exceeds the executor cap %d", ErrInvalidContract, who, a.MaxRows, d.maxDeleteRows())
	}
	switch a.Do {
	case ActionRecordAction:
		def, ok := st.ActionDef(ctx, a.Model, a.ActionKey)
		if !ok || !ingestable(def) {
			return fmt.Errorf("%w: %s: %s: action %q of model %q does not exist or is not declared ingest: true (without approval, supervisor_policy, confirm_typed or a table/create placement)",
				ErrInvalidContract, who, CodeActionNotIngestable, a.ActionKey, a.Model)
		}
		keys := actionFieldKeys(def)
		keyField := ""
		if def.Idempotency != nil {
			keyField = def.Idempotency.KeyField
		}
		if len(a.Params) > maxActionParamKeys {
			return fmt.Errorf("%w: %s: params has more than %d keys", ErrInvalidContract, who, maxActionParamKeys)
		}
		for k := range a.Params {
			if keyField != "" && k == keyField {
				return fmt.Errorf("%w: %s: params %q is the idempotency key of the action and is set by the kernel", ErrInvalidContract, who, k)
			}
			if !keys[k] && k != reasonParam {
				return fmt.Errorf("%w: %s: params %q is not a field of action %q", ErrInvalidContract, who, k, a.ActionKey)
			}
		}
		if a.MaxRows > 1 && keyField == "" {
			return fmt.Errorf("%w: %s: max_rows > 1 needs an action with idempotency (a retry after a partial failure would run it twice)", ErrInvalidContract, who)
		}
	case ActionRecordTransition:
		sm, ok := st.ActiveStageMachine(ctx, a.Model)
		if !ok || sm == nil {
			return fmt.Errorf("%w: %s: model %q has no active stage machine", ErrInvalidContract, who, a.Model)
		}
		known := false
		for _, s := range sm.Stages {
			if s.Key == a.To {
				known = true
				break
			}
		}
		if !known {
			return fmt.Errorf("%w: %s: to %q is not a state of the stage machine of model %q", ErrInvalidContract, who, a.To, a.Model)
		}
	case ActionRecordRestore:
		meta, err := st.TableMetadata(ctx, a.Model)
		if err != nil || !supportsSoftDelete(meta) {
			return fmt.Errorf("%w: %s: model %q does not support soft delete (deleted_at), there is nothing to restore", ErrInvalidContract, who, a.Model)
		}
		if meta.AppendOnly {
			return fmt.Errorf("%w: %s: model %q is append-only and cannot be restored", ErrInvalidContract, who, a.Model)
		}
		if d.tombstoner(st) == nil {
			return fmt.Errorf("%w: %s: record.restore needs a TombstoneFinder (DynamicExecutor.Tombstones)", ErrInvalidContract, who)
		}
	}
	return nil
}

// stateOpAuth is the action string the host gate sees: "action:<key>",
// "transition:<to>" or "restore".
func stateOpAuth(op Operation) string {
	switch op.Route.Op {
	case OpAction:
		return "action:" + op.ActionKey
	case OpTransition:
		return "transition:" + op.To
	}
	return "restore"
}

// stateOp runs OpAction, OpTransition and OpRestore. The shared walls come
// first and in the order of record.delete: match required, max_rows within the
// executor cap, then the host gate, then the per-key lock.
func (d *DynamicExecutor) stateOp(ctx context.Context, st dynStore, user modelbase.AuthUser, op Operation, sc *dynamic.EntityScope) (Outcome, error) {
	limit := op.MaxRows
	if limit <= 0 {
		limit = 1
	}
	if limit > d.maxDeleteRows() {
		return Outcome{}, &ExecError{Kind: KindInvalid, Code: "max_rows_exceeds_limit"}
	}
	if len(op.Route.Match) == 0 {
		return Outcome{}, &ExecError{Kind: KindInvalid, Code: "match_required"}
	}
	if err := d.allow(ctx, op, stateOpAuth(op)); err != nil {
		return Outcome{}, err
	}
	unlock := d.lock(op)
	defer unlock()
	switch op.Route.Op {
	case OpAction:
		return d.doAction(ctx, st, user, op, sc, limit)
	case OpTransition:
		return d.doTransition(ctx, st, user, op, sc, limit)
	}
	return d.doRestore(ctx, st, user, op, sc, limit)
}

// selectLive returns the live rows record.action and record.transition act on:
// none is a 404 (an event that arrives before its record must not be
// acknowledged as applied), more than limit a 409 that touches nothing.
func selectLive(ctx context.Context, st dynStore, user modelbase.AuthUser, op Operation, sc *dynamic.EntityScope, limit int) ([]map[string]any, error) {
	hits, err := matchRows(ctx, st, op.Route.Model, user, op.Route.Match, op.Values, sc, limit+1)
	if err != nil {
		return nil, err
	}
	if len(hits) == 0 {
		return nil, &ExecError{Kind: KindNotFound, Code: "record_not_found"}
	}
	if len(hits) > limit {
		return nil, &ExecError{Kind: KindConflict, Code: "too_many_matches"}
	}
	return hits, nil
}

// lifecycleState mirrors the state a RequiresState gate reads: the stage field
// of the machine when there is one, else status, else state.
func lifecycleState(row map[string]any, field string) string {
	if field != "" {
		return dynamic.StageValue(canonValue(row[field]))
	}
	if s := dynamic.StageValue(canonValue(row["status"])); s != "" {
		return s
	}
	return dynamic.StageValue(canonValue(row["state"]))
}

func (d *DynamicExecutor) doAction(ctx context.Context, st dynStore, user modelbase.AuthUser, op Operation, sc *dynamic.EntityScope, limit int) (Outcome, error) {
	model := op.Route.Model
	def, ok := st.ActionDef(ctx, model, op.ActionKey)
	if !ok || !ingestable(def) {
		return Outcome{}, &ExecError{Kind: KindInvalid, Code: CodeActionNotIngestable}
	}
	policy := op.OnIneligible
	if policy == "" {
		policy = OnIneligibleNoop
	}
	if policy != OnIneligibleNoop && policy != OnIneligibleFail {
		return Outcome{}, &ExecError{Kind: KindInvalid, Code: "bad_on_ineligible"}
	}
	hits, err := selectLive(ctx, st, user, op, sc, limit)
	if err != nil {
		return Outcome{}, err
	}
	ids := make([]uuid.UUID, len(hits))
	for i, h := range hits {
		if ids[i], err = rowID(h); err != nil || ids[i] == uuid.Nil {
			return Outcome{}, &ExecError{Kind: KindInternal, Code: "bad_record_id", Err: err}
		}
	}
	if op.DryRun {
		out := dryRunAction(ctx, st, def, op, hits, ids)
		out.Would = &WouldResult{Would: "action", Target: op.ActionKey, Rows: len(ids),
			State: out.WouldDispatch.State, ParamKeys: sortedKeys(op.Params)}
		if !out.WouldDispatch.Eligible {
			// The same answer the real run gives for an ineligible row.
			if policy == OnIneligibleFail {
				return Outcome{}, &ExecError{Kind: KindConflict, Code: CodeStateNotEligible}
			}
			out.Noop, out.Reason = true, CodeStateNotEligible
		}
		return out, nil
	}
	affected, noops := 0, 0
	for i, id := range ids {
		payload := make(map[string]any, len(op.Params)+1)
		for k, v := range op.Params {
			payload[k] = v
		}
		if def.Idempotency != nil && def.Idempotency.KeyField != "" && op.IdempotencyKey != "" {
			// One key per receipt, step and row: a retry replays the stored
			// response instead of running the action again.
			payload[def.Idempotency.KeyField] = fmt.Sprintf("%s:%d", op.IdempotencyKey, i)
		}
		res, aerr := st.ExecAction(ctx, model, user, id, op.ActionKey, payload)
		if aerr != nil {
			if errors.Is(aerr, dynamic.ErrInvalidState) {
				if policy == OnIneligibleFail {
					return Outcome{}, &ExecError{Kind: KindConflict, Code: CodeStateNotEligible, Err: aerr}
				}
				noops++
				continue
			}
			return Outcome{}, mapActionErr(aerr)
		}
		if !res.Success {
			return Outcome{}, declinedErr(res)
		}
		affected++
	}
	out := Outcome{RecordID: ids[0].String(), Affected: affected}
	if affected == 0 && noops > 0 {
		out.Noop, out.Reason = true, CodeStateNotEligible
	}
	return out, nil
}

// dryRunAction resolves what record.action would do and dispatches nothing:
// it never calls ExecAction. The state is read from the listed row, which a
// column mask could hide; then State is empty and Eligible stays true (the real
// run decides, with the unmasked row).
func dryRunAction(ctx context.Context, st dynStore, def *manifest.ActionDef, op Operation, hits []map[string]any, ids []uuid.UUID) Outcome {
	field := ""
	if sm, ok := st.ActiveStageMachine(ctx, op.Route.Model); ok && sm != nil {
		field = sm.Field
	}
	w := &WouldDispatch{Action: op.ActionKey, RecordID: ids[0].String(), Rows: len(ids), Eligible: true, Params: op.Params}
	if _, has := hits[0][fieldOrStatus(hits[0], field)]; has {
		w.State = lifecycleState(hits[0], field)
	}
	if len(def.RequiresState) > 0 && w.State != "" {
		w.Eligible = false
		for _, s := range def.RequiresState {
			if s == w.State {
				w.Eligible = true
				break
			}
		}
	}
	return Outcome{RecordID: w.RecordID, WouldDispatch: w}
}

// fieldOrStatus names the column lifecycleState reads from row.
func fieldOrStatus(row map[string]any, field string) string {
	if field != "" {
		return field
	}
	if _, has := row["status"]; has {
		return "status"
	}
	return "state"
}

// mapActionErr maps what ExecAction can return for an ingest action. An
// ApprovalRequiredError cannot happen for an ingestable action; if it does it
// is a server error and nothing here parks or retries the action.
func mapActionErr(err error) error {
	var approval *dynamic.ApprovalRequiredError
	switch {
	case errors.As(err, &approval):
		return &ExecError{Kind: KindInternal, Code: "approval_unexpected", Err: err}
	case errors.Is(err, dynamic.ErrActionNotFound):
		return &ExecError{Kind: KindInvalid, Code: CodeActionNotIngestable, Err: err}
	case errors.Is(err, dynamic.ErrInvalidTransition):
		return &ExecError{Kind: KindInvalid, Code: CodeInvalidTransition, Err: err}
	}
	return mapDynamicErr(err)
}

// declinedErr turns an unsuccessful ActionResult into an error without ever
// forwarding the dispatcher's message.
func declinedErr(res dynamic.ActionResult) error {
	if res.Error != nil && res.Error.Code == "idempotency_in_flight" {
		return &ExecError{Kind: KindConflict, Code: "idempotency_in_flight"}
	}
	return &ExecError{Kind: KindInvalid, Code: "action_declined"}
}

func (d *DynamicExecutor) doTransition(ctx context.Context, st dynStore, user modelbase.AuthUser, op Operation, sc *dynamic.EntityScope, limit int) (Outcome, error) {
	model := op.Route.Model
	sm, ok := st.ActiveStageMachine(ctx, model)
	if !ok || sm == nil || strings.TrimSpace(op.To) == "" {
		return Outcome{}, &ExecError{Kind: KindInvalid, Code: "no_stage_machine"}
	}
	hits, err := selectLive(ctx, st, user, op, sc, limit)
	if err != nil {
		return Outcome{}, err
	}
	// Judge every row before writing any, so a refused move leaves the rest
	// untouched.
	type move struct {
		id   uuid.UUID
		from string
		noop bool
	}
	plan := make([]move, len(hits))
	for i, h := range hits {
		id, rerr := rowID(h)
		if rerr != nil || id == uuid.Nil {
			return Outcome{}, &ExecError{Kind: KindInternal, Code: "bad_record_id", Err: rerr}
		}
		from := dynamic.StageValue(canonValue(h[sm.Field]))
		if from == op.To {
			plan[i] = move{id: id, noop: true}
			continue
		}
		if !sm.Allows(from, op.To) {
			return Outcome{}, &ExecError{Kind: KindInvalid, Code: CodeInvalidTransition}
		}
		plan[i] = move{id: id, from: from}
	}
	if op.DryRun {
		return d.wouldTransition(ctx, st, op, sm.Field, len(plan), func(i int) (uuid.UUID, string, bool) {
			return plan[i].id, plan[i].from, plan[i].noop
		}), nil
	}
	affected := 0
	for _, m := range plan {
		if m.noop {
			continue
		}
		// Service.Update re-validates the move (and fires the on_transition
		// hooks) against the row as it is now.
		if _, uerr := st.Update(ctx, model, user, m.id, map[string]any{sm.Field: op.To}); uerr != nil {
			if errors.Is(uerr, dynamic.ErrInvalidTransition) {
				return Outcome{}, &ExecError{Kind: KindInvalid, Code: CodeInvalidTransition, Err: uerr}
			}
			return Outcome{}, mapDynamicErr(uerr)
		}
		affected++
	}
	out := Outcome{RecordID: plan[0].id.String(), Affected: affected}
	if affected == 0 {
		out.Noop, out.Reason = true, CodeAlreadyInState
	}
	return out, nil
}

func (d *DynamicExecutor) doRestore(ctx context.Context, st dynStore, user modelbase.AuthUser, op Operation, sc *dynamic.EntityScope, limit int) (Outcome, error) {
	model := op.Route.Model
	meta, err := st.TableMetadata(ctx, model)
	if err != nil {
		return Outcome{}, mapDynamicErr(err)
	}
	if !supportsSoftDelete(meta) {
		return Outcome{}, &ExecError{Kind: KindInvalid, Code: "soft_delete_unsupported"}
	}
	tombs, err := d.tombstoneRows(ctx, st, op, op.Route.Match, sc, limit+1)
	if err != nil {
		return Outcome{}, err
	}
	live, err := matchRows(ctx, st, model, user, op.Route.Match, op.Values, sc, limit+1)
	if err != nil {
		return Outcome{}, err
	}
	// A live row with the same key next to a tombstone would end in two rows
	// for one key: it counts against the cap, so with the default cap of one
	// the restore is refused instead of duplicating.
	if len(tombs)+len(live) > limit {
		return Outcome{}, &ExecError{Kind: KindConflict, Code: "too_many_matches"}
	}
	if len(tombs) == 0 {
		if len(live) > 0 {
			// Already restored (a retry, or another source got there first).
			id, _ := rowID(live[0])
			out := Outcome{RecordID: id.String(), Noop: true, Reason: CodeAlreadyLive}
			if op.DryRun {
				out.Would = &WouldResult{Would: "restore", Rows: len(live), Noop: true}
			}
			return out, nil
		}
		out := Outcome{NotFound: true}
		if op.DryRun {
			out.Would = &WouldResult{Would: "restore", Noop: true}
		}
		return out, nil
	}
	ids := make([]uuid.UUID, 0, len(tombs))
	for _, t := range tombs {
		id, rerr := rowID(t)
		if rerr != nil || id == uuid.Nil {
			return Outcome{}, &ExecError{Kind: KindInternal, Code: "bad_record_id", Err: rerr}
		}
		ids = append(ids, id)
	}
	if op.DryRun {
		return Outcome{RecordID: ids[0].String(), Affected: len(ids),
			Would: &WouldResult{Would: "restore", Rows: len(ids)}}, nil
	}
	n := 0
	for _, id := range ids {
		if _, rerr := st.Restore(ctx, model, user, id); rerr != nil {
			if errors.Is(rerr, dynamic.ErrRecordNotFound) {
				continue // restored or purged meanwhile
			}
			return Outcome{}, mapDynamicErr(rerr)
		}
		n++
	}
	if n == 0 {
		return Outcome{NotFound: true}, nil
	}
	return Outcome{RecordID: ids[0].String(), Affected: n}, nil
}
