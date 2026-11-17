package inbound

import (
	"context"
	"errors"
	"fmt"
	"net/http"

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
		case ActionRecordCreate, ActionRecordUpdate, ActionRecordUpsert:
			if h.cfg.Executor == nil {
				add("actions[%d] %s: no Executor", i, a.Do)
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
	recID, failedAt, err := h.runChain(ctx, c, org, evKey, payload, hop, cur.ID, &steps)
	if err != nil {
		h.failChain(w, ctx, cur.ID, err, failedAt)
		return
	}
	if e := h.cfg.Receipts.Finish(context.WithoutCancel(ctx), cur.ID, StatusApplied, recID, ""); e != nil {
		h.cfg.Logger.Error("inbound: receipt finish", "err_type", fmt.Sprintf("%T", e))
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
	return m
}

// runChain executes the chain. steps holds the progress found in the receipt
// and is updated (and persisted) after every action. On failure it returns the
// index of the action that stopped the chain.
func (h *Handler) runChain(ctx context.Context, c *compiled, org uuid.UUID, evKey string, payload any, hop int, rid uuid.UUID, steps *[]ActionStep) (recID string, failedAt int, err error) {
	origin := OriginPrefix + c.Key
	ctx = WithOrigin(ctx, origin)
	env := &evalEnv{payload: payload, event: map[string]any{
		"key": evKey, "contract": c.Key, "version": int64(c.Version), "org": org.String()}}

	save := func() error {
		if e := h.cfg.Receipts.SaveProgress(context.WithoutCancel(ctx), rid, *steps); e != nil {
			h.cfg.Logger.Error("inbound: save progress", "receipt", rid, "err_type", fmt.Sprintf("%T", e))
			return &ExecError{Kind: KindInternal, Code: "progress_unavailable", Err: e}
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
		if a.when != nil && !a.when.holds(env) {
			step.Status = StepSkipped
			*steps = append(*steps, step)
			if e := save(); e != nil {
				return "", i, e
			}
			continue
		}
		out, aerr := h.runAction(ctx, c, a, env, org, evKey, rid, i, origin, hop)
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
		step.Status, step.RecordID, step.Created = StepDone, out.RecordID, out.Created
		*steps = append(*steps, step)
		if a.isRecordLike() {
			env.result = resultOf(step)
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
	case ActionRecordCreate, ActionRecordUpdate, ActionRecordUpsert, ActionCommandRun:
		return true
	}
	return false
}

func errCodeOf(err error) string {
	var ee *ExecError
	if errors.As(err, &ee) && ee.Kind != KindInternal {
		return ee.Code
	}
	return "internal"
}

func (h *Handler) runAction(ctx context.Context, c *compiled, a *cAction, env *evalEnv, org uuid.UUID, evKey string, rid uuid.UUID, idx int, origin string, hop int) (out Outcome, err error) {
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
	case ActionRecordCreate, ActionRecordUpdate, ActionRecordUpsert, ActionCommandRun:
		vals, merr := evalMap(a.mapping, env)
		if merr != nil {
			return Outcome{}, merr
		}
		op := Operation{Org: org, Contract: c.Key, EventKey: evKey, Values: vals, Origin: origin, Hop: hop, IdempotencyKey: idem}
		if a.Do == ActionCommandRun {
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
		if a.Scope != nil {
			op.Branch = scopeValue(env.payload, a.Scope.Branch)
			op.Entity = scopeValue(env.payload, a.Scope.Entity)
		}
		return h.cfg.Executor.Execute(ctx, op)
	case ActionEventEmit, ActionAutomationEnqueue, ActionNotify:
		body, perr := evalMap(a.payload, env)
		if perr != nil {
			return Outcome{}, perr
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

func (h *Handler) failChain(w http.ResponseWriter, ctx context.Context, id uuid.UUID, err error, at int) {
	var ee *ExecError
	if !errors.As(err, &ee) {
		ee = &ExecError{Kind: KindInternal, Code: "internal", Err: err}
	}
	code := ee.Code
	if ee.Kind == KindInternal {
		code = "internal"
		h.cfg.Logger.Error("inbound: action failed", "receipt", id, "action", at, "exec_code", ee.Code, "err_type", fmt.Sprintf("%T", ee.Err))
	}
	if e := h.cfg.Receipts.Finish(context.WithoutCancel(ctx), id, StatusFailed, "", code); e != nil {
		h.cfg.Logger.Error("inbound: receipt finish", "err_type", fmt.Sprintf("%T", e))
	}
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
