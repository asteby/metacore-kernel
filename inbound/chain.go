package inbound

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

// checkChain verifies at startup that every port a chain needs is wired.
func (h *Handler) checkChain(c *compiled) []error {
	var errs []error
	add := func(f string, a ...any) {
		errs = append(errs, fmt.Errorf("contract %s v%d: "+f, append([]any{c.Key, c.Version}, a...)...))
	}
	for i, a := range c.chain {
		switch a.Do {
		case ActionRecordCreate, ActionRecordUpdate, ActionRecordUpsert, ActionRecordDelete,
			ActionRecordAction, ActionRecordTransition, ActionRecordRestore:
			if h.cfg.Executor == nil {
				add("actions[%d] %s: no Executor", i, a.Do)
			} else if op, gated := gatedOp(a.Do); gated && !executorSupports(h.cfg.Executor, op) {
				add("actions[%d] %s: the Executor does not declare Op %q (implement OpSupporter)", i, a.Do, op)
			}
			if a.Guard != nil && h.cfg.Executor != nil && !executorGuards(h.cfg.Executor) {
				add("actions[%d] %s: the Executor does not declare guard support (implement GuardSupporter)", i, a.Do)
			}
		case ActionCommandRun:
			if _, ok := h.cfg.Commands.get(a.Command); !ok {
				add("actions[%d]: command %q not registered", i, a.Command)
			}
		case ActionEventEmit:
			if h.cfg.Emitter == nil {
				add("actions[%d] %s: no Emitter", i, a.Do)
			}
		case ActionNotify:
			if h.cfg.Notifier == nil {
				add("actions[%d] %s: no Notifier", i, a.Do)
			}
		case ActionAutomationEnqueue:
			if h.cfg.Automation == nil {
				add("actions[%d] %s: no Automation ingester", i, a.Do)
			}
		}
	}
	return errs
}

// serveChain runs the actions of a claimed receipt, resuming after the last
// finished one, and answers the request.
func (h *Handler) serveChain(w http.ResponseWriter, ctx context.Context, c *compiled, org uuid.UUID, evKey string, payload any, hop int, cur Receipt) {
	steps := append([]ActionStep(nil), cur.Progress...)
	recID, failedAt, err := h.runChain(ctx, c, org, evKey, payload, hop, cur, &steps, c.DryRun)
	if err != nil {
		h.failChain(w, ctx, cur, err, failedAt)
		return
	}
	h.finish(ctx, cur, StatusApplied, recID, "")
	if c.DryRun {
		writeJSON(w, http.StatusOK, SuccessBody{Status: StatusWouldApply, ReceiptID: cur.ID.String(), RecordID: recID, Actions: steps, Shadow: true})
		return
	}
	writeJSON(w, http.StatusOK, SuccessBody{Status: "applied", ReceiptID: cur.ID.String(), RecordID: recID, Actions: steps})
}

func resultOf(s ActionStep) map[string]any {
	m := map[string]any{}
	if s.RecordID != "" {
		m["id"] = s.RecordID
	}
	if s.Do != ActionEventEmit && s.Do != ActionNotify && s.Do != ActionAutomationEnqueue {
		m["created"] = s.Created
	}
	if matchOnly(s.Do) {
		m["affected"] = int64(s.Affected)
	}
	if isStateOp(s.Do) {
		m["noop"] = s.Noop
	}
	return m
}

// runChain executes the chain. steps holds the progress found in the receipt
// and is updated (and persisted) after every action. On failure it returns the
// index of the action that stopped the chain.
func (h *Handler) runChain(ctx context.Context, c *compiled, org uuid.UUID, evKey string, payload any, hop int, cur Receipt, steps *[]ActionStep, dry bool) (recID string, failedAt int, err error) {
	origin := OriginPrefix + c.Key
	ctx = WithOrigin(ctx, origin)
	env := &evalEnv{payload: payload, event: map[string]any{
		"key": evKey, "contract": c.Key, "version": int64(c.Version), "org": org.String()}}

	rid := cur.ID
	// save persists the progress fenced by this attempt; losing ownership
	// (another worker re-claimed the event after the lease) aborts the chain
	// before any further effect.
	save := func() error {
		if rid == uuid.Nil {
			return nil // ephemeral run (Handler.DryRun): there is no receipt to keep
		}
		owned, e := h.cfg.Receipts.SaveProgress(context.WithoutCancel(ctx), rid, cur.Attempts, *steps)
		if e != nil {
			h.cfg.Logger.Error("inbound: save progress", "receipt", rid, "err_type", fmt.Sprintf("%T", e))
			return &ExecError{Kind: KindInternal, Code: "progress_unavailable", Err: e}
		}
		if !owned {
			h.cfg.Logger.Warn("inbound: chain no longer owned by this attempt", "receipt", rid, "attempt", cur.Attempts)
			return &ExecError{Kind: KindConflict, Code: CodeLeaseLost}
		}
		return nil
	}
	for i, a := range c.chain {
		if i < len(*steps) && (*steps)[i].Status != StepFailed {
			// Already applied in an earlier attempt: adopt, never repeat.
			s := (*steps)[i]
			if s.Do != a.Do {
				return "", i, &ExecError{Kind: KindConflict, Code: "contract_changed"}
			}
			switch s.Status {
			case StepDone:
				if a.isRecordLike() {
					env.result = resultOf(s)
				}
				if s.RecordID != "" && a.isRecordLike() {
					recID = s.RecordID
				}
			case StepError:
				env.result = map[string]any{}
			}
			continue
		}
		if i < len(*steps) { // a failed step is re-run
			*steps = (*steps)[:i]
		}
		step := ActionStep{Index: i, Do: a.Do}
		// Renew the lease and verify ownership BEFORE the effect.
		if e := save(); e != nil {
			return "", i, e
		}
		if a.when != nil && !a.when.holds(env) {
			step.Status = StepSkipped
			*steps = append(*steps, step)
			if e := save(); e != nil {
				return "", i, e
			}
			continue
		}
		out, aerr := h.runAction(ctx, c, a, env, org, evKey, rid, i, origin, hop, dry)
		if aerr != nil {
			step.ErrorCode = errCodeOf(aerr)
			if a.onErr == OnErrorContinue {
				step.Status = StepError
				*steps = append(*steps, step)
				env.result = map[string]any{}
				if e := save(); e != nil {
					return "", i, e
				}
				continue
			}
			step.Status = StepFailed
			*steps = append(*steps, step)
			if e := save(); e != nil {
				h.cfg.Logger.Error("inbound: could not record failed step", "receipt", rid)
			}
			return "", i, aerr
		}
		if out.Stale {
			// The guard found a newer row or tombstone: nothing was written and
			// the receipt still ends applied. Like a false `when`, it leaves
			// result and the chain's record id untouched.
			step.Status, step.ErrorCode = StepSkipped, CodeStaleEvent
			*steps = append(*steps, step)
			if e := save(); e != nil {
				return "", i, e
			}
			continue
		}
		step.applyOutcome(out)
		*steps = append(*steps, step)
		if a.isRecordLike() {
			env.result = resultOf(step)
			if dry && step.Created && step.RecordID == "" {
				// A row that would be created has no id yet: a later step that reads
				// {{result.id}} must still resolve, as it does in the real run.
				env.result["id"] = fmt.Sprintf("dry-run:%d", i)
			}
		}
		if out.RecordID != "" && a.isRecordLike() {
			recID = out.RecordID
		}
		if e := save(); e != nil {
			return "", i, e
		}
	}
	return recID, 0, nil
}

func (a *cAction) isRecordLike() bool {
	switch a.Do {
	case ActionRecordCreate, ActionRecordUpdate, ActionRecordUpsert, ActionRecordDelete,
		ActionRecordAction, ActionRecordTransition, ActionRecordRestore, ActionCommandRun:
		return true
	}
	return false
}

// gatedOp returns the Op an Executor must declare through OpSupporter to run
// the action: the ones that are not a plain create|update|upsert.
func gatedOp(do string) (Op, bool) {
	switch do {
	case ActionRecordDelete:
		return OpDelete, true
	case ActionRecordAction:
		return OpAction, true
	case ActionRecordTransition:
		return OpTransition, true
	case ActionRecordRestore:
		return OpRestore, true
	}
	return "", false
}

func errCodeOf(err error) string {
	var ee *ExecError
	if errors.As(err, &ee) && ee.Kind != KindInternal {
		return ee.Code
	}
	return "internal"
}

func (h *Handler) runAction(ctx context.Context, c *compiled, a *cAction, env *evalEnv, org uuid.UUID, evKey string, rid uuid.UUID, idx int, origin string, hop int, dry bool) (out Outcome, err error) {
	ctx, cancel := context.WithTimeout(ctx, h.cfg.ExecTimeout)
	defer cancel()
	defer func() {
		if rec := recover(); rec != nil {
			h.cfg.Logger.Error("inbound: action panic", "contract", c.Key, "action", idx, "panic_type", fmt.Sprintf("%T", rec))
			out, err = Outcome{}, &ExecError{Kind: KindInternal, Code: "internal"}
		}
	}()
	idem := fmt.Sprintf("%s:%d", rid, idx)
	switch a.Do {
	case ActionRecordCreate, ActionRecordUpdate, ActionRecordUpsert, ActionRecordDelete,
		ActionRecordAction, ActionRecordTransition, ActionRecordRestore, ActionCommandRun:
		vals, merr := evalMap(a.mapping, env)
		if merr != nil {
			return Outcome{}, merr
		}
		op := Operation{Org: org, Contract: c.Key, EventKey: evKey, Values: vals, Origin: origin, Hop: hop, IdempotencyKey: idem}
		if a.Do == ActionCommandRun {
			if dry {
				return Outcome{Would: &WouldResult{Would: "would_command", Target: a.Command}}, nil
			}
			fn, ok := h.cfg.Commands.get(a.Command)
			if !ok {
				return Outcome{}, &ExecError{Kind: KindInternal, Code: "command_missing"}
			}
			op.Route = Route{Command: a.Command, Op: a.Op}
			if op.Route.Op == "" {
				op.Route.Op = OpCreate
			}
			return fn(ctx, op)
		}
		if h.cfg.Executor == nil {
			return Outcome{}, &ExecError{Kind: KindInternal, Code: "executor_unavailable"}
		}
		if dry && !executorDryRuns(h.cfg.Executor) {
			return Outcome{}, &ExecError{Kind: KindInternal, Code: "dry_run_unsupported"}
		}
		op.DryRun = dry
		// Defaults apply only when a row is created, never on update.
		op.CreateDefaults = a.Defaults
		op.Route = Route{Model: a.Model, Match: a.Match, Op: Op(a.Do[len("record."):])}
		if a.Do == ActionRecordCreate {
			// A create whose outcome is unknown (commit then a failed progress
			// write, a timeout after commit) must not duplicate on retry: it
			// runs as an upsert on a column that stores "<receipt>:<action>".
			op.Values[a.IdempotencyField] = idem
			op.Route.Op, op.Route.Match = OpUpsert, []string{a.IdempotencyField}
		}
		if matchOnly(a.Do) {
			op.DeleteMode, op.MaxRows = a.Mode, a.MaxRows
		}
		if a.Do == ActionRecordAction {
			params, perr := evalMap(a.params, env)
			if perr != nil {
				return Outcome{}, perr
			}
			op.ActionKey, op.Params, op.OnIneligible = a.ActionKey, params, a.OnIneligible
		}
		if a.Do == ActionRecordTransition {
			op.To = a.To
		}
		g, gerr := evalGuard(a, env.payload)
		if gerr != nil {
			return Outcome{}, gerr
		}
		op.Guard = g
		if a.Scope != nil {
			op.Branch = scopeValue(env.payload, a.Scope.Branch)
			op.Entity = scopeValue(env.payload, a.Scope.Entity)
			op.RequireBranch, op.RequireEntity = a.Scope.Branch != "", a.Scope.Entity != ""
			if (op.RequireBranch && op.Branch == "") || (op.RequireEntity && op.Entity == "") {
				return Outcome{}, &ExecError{Kind: KindInvalid, Code: CodeScopeRequired}
			}
		}
		out, err = h.cfg.Executor.Execute(ctx, op)
		if dry && err == nil && a.Do == ActionRecordCreate && out.Would != nil {
			// A chain create runs as an upsert on its idempotency column; it is
			// still a create to whoever reads the report.
			out.Would.Would = "create"
		}
		return out, err
	case ActionEventEmit, ActionAutomationEnqueue, ActionNotify:
		body, perr := evalMap(a.payload, env)
		if perr != nil {
			return Outcome{}, perr
		}
		if dry {
			// The effect is reported, never run: nothing is emitted, notified or
			// enqueued.
			return Outcome{Would: &WouldResult{Would: wouldEffect(a.Do), Target: effectName(a)}}, nil
		}
		var e error
		switch a.Do {
		case ActionEventEmit:
			if h.cfg.Emitter == nil {
				return Outcome{}, &ExecError{Kind: KindInternal, Code: "emitter_unavailable"}
			}
			body[OriginField], body[HopField] = origin, int64(hop+1)
			e = h.cfg.Emitter.Emit(ctx, EmitRequest{Org: org, Name: a.Name, Payload: body, Origin: origin, Hop: hop + 1, IdempotencyKey: idem})
		case ActionAutomationEnqueue:
			if h.cfg.Automation == nil {
				return Outcome{}, &ExecError{Kind: KindInternal, Code: "automation_unavailable"}
			}
			e = h.cfg.Automation.IngestEvent(ctx, AutomationEvent{ID: idem, Org: org, Name: a.Name, Payload: body, Origin: origin, Hop: hop + 1})
		default:
			if h.cfg.Notifier == nil {
				return Outcome{}, &ExecError{Kind: KindInternal, Code: "notifier_unavailable"}
			}
			e = h.cfg.Notifier.Notify(ctx, NotifyRequest{Org: org, Template: a.Template, Params: body, Origin: origin, IdempotencyKey: idem})
		}
		if e != nil {
			var ee *ExecError
			if errors.As(e, &ee) {
				return Outcome{}, e
			}
			return Outcome{}, &ExecError{Kind: KindInternal, Code: "internal", Err: e}
		}
		return Outcome{}, nil
	}
	return Outcome{}, &ExecError{Kind: KindInternal, Code: "bad_action"}
}

// evalGuard reads the event's guard value. The guard is opt-in by data: an
// ABSENT or explicit null value applies the event (nil guard, nil error). A
// PRESENT value that is not an RFC 3339 instant or a whole number (""
// included, "N/A", "2024-01-01", "5", 1.5, a bool, an object) is a producer
// bug and fails with 422 guard_value_invalid, before any effect: it must never
// skip the guard silently.
func evalGuard(a *cAction, payload any) (*OperationGuard, error) {
	if a.Guard == nil {
		return nil, nil
	}
	v, ok := lookup(payload, a.guardPath)
	if !ok {
		return nil, nil
	}
	v = normalize(v)
	cv, err := guardNormalize(v, false)
	if err != nil || cv == nil {
		return nil, &ExecError{Kind: KindInvalid, Code: CodeGuardValueInvalid, Err: err}
	}
	if s, isStr := v.(string); isStr {
		v = strings.TrimSpace(s)
	} else {
		v = cv // a whole number: int64
	}
	return &OperationGuard{Field: a.Guard.Field, Op: a.Guard.Op, Value: v,
		Match: a.Match, Create: a.Do == ActionRecordCreate}, nil
}

func (h *Handler) failChain(w http.ResponseWriter, ctx context.Context, cur Receipt, err error, at int) {
	id := cur.ID
	var ee *ExecError
	if !errors.As(err, &ee) {
		ee = &ExecError{Kind: KindInternal, Code: "internal", Err: err}
	}
	code := ee.Code
	if ee.Kind == KindInternal {
		code = "internal"
		h.cfg.Logger.Error("inbound: action failed", "receipt", id, "action", at, "exec_code", ee.Code, "err_type", fmt.Sprintf("%T", ee.Err))
	}
	h.finish(ctx, cur, StatusFailed, "", code)
	status, ecode, msg := http.StatusInternalServerError, CodeInternal, "internal error"
	switch ee.Kind {
	case KindInvalid:
		status, ecode, msg = http.StatusUnprocessableEntity, ee.Code, "the event was rejected by an action"
	case KindConflict:
		status, ecode, msg = http.StatusConflict, ee.Code, "the event conflicts with the current state"
	case KindNotFound:
		status, ecode, msg = http.StatusNotFound, ee.Code, "target record not found"
	case KindForbidden:
		status, ecode, msg = http.StatusForbidden, ee.Code, "the event is outside the allowed scope"
	}
	writeJSON(w, status, ErrorBody{Error: ErrorDetail{Code: ecode, Message: msg, FailedAction: &at}})
}

// chainScopeOK reports whether the event names the branch/entity every
// record action of the chain requires (checked before any effect and before
// the nonce is consumed, like the route case).
func chainScopeOK(c *compiled, payload any) bool {
	for _, a := range c.chain {
		if a.Scope == nil {
			continue
		}
		if (a.Scope.Branch != "" && scopeValue(payload, a.Scope.Branch) == "") ||
			(a.Scope.Entity != "" && scopeValue(payload, a.Scope.Entity) == "") {
			return false
		}
	}
	return true
}

// executorSupports reports whether ex declared op through OpSupporter; an
// Executor without it supports no destructive op.
func executorSupports(ex Executor, op Op) bool {
	s, ok := ex.(OpSupporter)
	return ok && s.SupportsOp(op)
}
