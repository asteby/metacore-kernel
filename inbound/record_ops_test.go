package inbound

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
	"github.com/asteby/metacore-kernel/modelbase"
)

// record.action | record.transition | record.restore (RFC 0003 §2.4).

// ------------------------------------------------------------- the fake ---

type execCall struct {
	Model   string
	Key     string
	ID      uuid.UUID
	Payload map[string]any
}

// ExecAction fakes dynamic.Service.ExecAction: it resolves the action, finds
// the row in the caller's organization, applies the RequiresState gate and
// only then "dispatches" (records the call).
func (f *fakeDyn) ExecAction(_ context.Context, model string, u modelbase.AuthUser, id uuid.UUID, key string, payload map[string]any) (dynamic.ActionResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	def := f.actions[key]
	if def == nil {
		return dynamic.ActionResult{}, dynamic.ErrActionNotFound
	}
	var row map[string]any
	for _, r := range f.rows {
		if r["id"] == id.String() && r["organization_id"] == u.GetOrganizationID().String() && r["deleted_at"] == nil {
			row = r
		}
	}
	if row == nil {
		return dynamic.ActionResult{}, dynamic.ErrRecordNotFound
	}
	if len(def.RequiresState) > 0 {
		cur, allowed := dynamic.StageValue(row["status"]), false
		for _, s := range def.RequiresState {
			if s == cur {
				allowed = true
			}
		}
		if !allowed {
			return dynamic.ActionResult{}, fmt.Errorf("%w: status %q", dynamic.ErrInvalidState, cur)
		}
	}
	cp := make(map[string]any, len(payload))
	for k, v := range payload {
		cp[k] = v
	}
	call := execCall{Model: model, Key: key, ID: id, Payload: cp}
	f.execs = append(f.execs, call)
	if f.execFn != nil {
		return f.execFn(call)
	}
	return dynamic.ActionResult{Success: true, HTTPStatus: 200}, nil
}

// Restore fakes dynamic.Service.Restore: tenant scoped, tombstones only.
func (f *fakeDyn) Restore(_ context.Context, _ string, u modelbase.AuthUser, id uuid.UUID) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rows {
		if r["id"] == id.String() && r["organization_id"] == u.GetOrganizationID().String() && r["deleted_at"] != nil {
			r["deleted_at"] = nil
			return r, nil
		}
	}
	return nil, dynamic.ErrRecordNotFound
}

func (f *fakeDyn) ActionDef(_ context.Context, _, key string) (*manifest.ActionDef, bool) {
	def, ok := f.actions[key]
	return def, ok && def != nil
}

func (f *fakeDyn) ActiveStageMachine(context.Context, string) (*dynamic.StageMachine, bool) {
	return f.sm, f.sm != nil
}

func (f *fakeDyn) execCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.execs)
}

// opsFake is a model with a stage machine new -> paid -> shipped and a set of
// actions: mark_paid (ingest, needs state new), bulk (ingest, idempotent) and
// the ones that must never be reachable.
func opsFake() *fakeDyn {
	f := newFakeDyn()
	f.cols = append(f.cols, "status")
	f.sm = &dynamic.StageMachine{
		Field:  "status",
		Stages: []manifest.StageDef{{Key: "new"}, {Key: "paid"}, {Key: "shipped"}},
		Transitions: []manifest.TransitionDef{
			{From: "new", To: "paid"}, {From: "paid", To: "shipped"},
		},
	}
	f.actions = map[string]*manifest.ActionDef{
		"mark_paid": {Key: "mark_paid", Ingest: true, RequiresState: []string{"new"},
			Fields: []manifest.FieldDef{{Key: "reference", Name: "reference"}}},
		"bulk": {Key: "bulk", Ingest: true, Idempotency: &manifest.IdempotencyDef{KeyField: "request_id"},
			Fields: []manifest.FieldDef{{Key: "reference", Name: "reference"}, {Key: "request_id", Name: "request_id"}}},
		"capture":  {Key: "capture"}, // declared but not opted in
		"refund":   {Key: "refund", Ingest: true, SupervisorPolicy: "refund"},
		"approve":  {Key: "approve", Ingest: true, Approval: &manifest.ApprovalDef{Roles: []string{"manager"}}},
		"confirm":  {Key: "confirm", Ingest: true, ConfirmTyped: &v3.ConfirmTyped{Field: "sku"}},
		"toolbar":  {Key: "toolbar", Ingest: true, Placement: "table"},
		"creation": {Key: "creation", Ingest: true, Placement: "create"},
	}
	return f
}

func opsRow(f *fakeDyn, org uuid.UUID, sku, wh, status string) string {
	id := uuid.NewString()
	f.rows = append(f.rows, map[string]any{"id": id, "organization_id": org.String(), "sku": sku, "warehouse": wh, "status": status})
	return id
}

func rowOf(f *fakeDyn, id string) map[string]any {
	for _, r := range f.rows {
		if r["id"] == id {
			return r
		}
	}
	return nil
}

func stateOpOf(org uuid.UUID, kind Op, sku string) Operation {
	return Operation{Org: org, Route: Route{Model: "inv", Op: kind, Match: []string{"sku"}}, Values: map[string]any{"sku": sku}}
}

func actionOp(org uuid.UUID, key, sku string, params map[string]any) Operation {
	op := stateOpOf(org, OpAction, sku)
	op.ActionKey, op.Params = key, params
	return op
}

func transitionOp(org uuid.UUID, to, sku string) Operation {
	op := stateOpOf(org, OpTransition, sku)
	op.To = to
	return op
}

// ----------------------------------------------------------- record.action ---

func TestRecordActionSuccess(t *testing.T) {
	org := uuid.New()
	f := opsFake()
	id := opsRow(f, org, "S1", "W1", "new")
	d := &DynamicExecutor{store: f}
	out, err := d.Execute(context.Background(), actionOp(org, "mark_paid", "S1", map[string]any{"reference": "R-1"}))
	if err != nil {
		t.Fatal(err)
	}
	if out.RecordID != id || out.Affected != 1 || out.Noop || out.NotFound {
		t.Fatalf("outcome %+v", out)
	}
	if len(f.execs) != 1 || f.execs[0].Key != "mark_paid" || f.execs[0].ID.String() != id || f.execs[0].Payload["reference"] != "R-1" {
		t.Fatalf("dispatch: %+v", f.execs)
	}
}

func TestRecordActionIneligibleStateNoopOrFail(t *testing.T) {
	org := uuid.New()
	f := opsFake()
	opsRow(f, org, "S1", "W1", "shipped") // mark_paid needs "new"
	d := &DynamicExecutor{store: f}
	out, err := d.Execute(context.Background(), actionOp(org, "mark_paid", "S1", nil))
	if err != nil || !out.Noop || out.Reason != CodeStateNotEligible || out.Affected != 0 {
		t.Fatalf("default policy must be a noop: out=%+v err=%v", out, err)
	}
	op := actionOp(org, "mark_paid", "S1", nil)
	op.OnIneligible = OnIneligibleNoop
	if out, err = d.Execute(context.Background(), op); err != nil || !out.Noop {
		t.Fatalf("explicit noop: out=%+v err=%v", out, err)
	}
	op.OnIneligible = OnIneligibleFail
	_, err = d.Execute(context.Background(), op)
	if ee := execErr(t, err); ee.Kind != KindConflict || ee.Code != CodeStateNotEligible {
		t.Fatalf("fail policy: %+v", ee)
	}
	op.OnIneligible = "maybe"
	_, err = d.Execute(context.Background(), op)
	if ee := execErr(t, err); ee.Kind != KindInvalid || ee.Code != "bad_on_ineligible" {
		t.Fatalf("unknown policy: %+v", ee)
	}
	if f.execCount() != 0 {
		t.Fatal("an ineligible row must never be dispatched")
	}
}

func TestRecordActionOnlyIngestActionsRunAtRuntime(t *testing.T) {
	org := uuid.New()
	f := opsFake()
	opsRow(f, org, "S1", "W1", "new")
	d := &DynamicExecutor{store: f}
	for _, key := range []string{"capture", "refund", "approve", "confirm", "toolbar", "creation", "ghost"} {
		_, err := d.Execute(context.Background(), actionOp(org, key, "S1", nil))
		if ee := execErr(t, err); ee.Kind != KindInvalid || ee.Code != CodeActionNotIngestable {
			t.Fatalf("%s: %+v", key, ee)
		}
	}
	if f.execCount() != 0 {
		t.Fatal("a refused action must never be dispatched")
	}
}

func TestRecordActionMaxRowsMissingAndOtherOrg(t *testing.T) {
	org, other := uuid.New(), uuid.New()
	f := opsFake()
	a, b := opsRow(f, org, "S1", "W1", "new"), opsRow(f, org, "S1", "W2", "new")
	opsRow(f, other, "S9", "W1", "new")
	d := &DynamicExecutor{store: f}

	_, err := d.Execute(context.Background(), actionOp(org, "mark_paid", "S1", nil))
	if ee := execErr(t, err); ee.Kind != KindConflict || ee.Code != "too_many_matches" {
		t.Fatalf("over the default cap of 1: %+v", ee)
	}
	if f.execCount() != 0 {
		t.Fatal("an over-cap match must not dispatch anything")
	}
	op := actionOp(org, "mark_paid", "S1", nil)
	op.MaxRows = 2
	out, err := d.Execute(context.Background(), op)
	if err != nil || out.Affected != 2 || f.execCount() != 2 {
		t.Fatalf("cap 2: out=%+v err=%v execs=%d", out, err, f.execCount())
	}
	if ids := []string{f.execs[0].ID.String(), f.execs[1].ID.String()}; !(ids[0] == a && ids[1] == b || ids[0] == b && ids[1] == a) {
		t.Fatalf("dispatched on the wrong rows: %v", ids)
	}
	op.MaxRows = d.maxDeleteRows() + 1
	if _, err = d.Execute(context.Background(), op); execErr(t, err).Code != "max_rows_exceeds_limit" {
		t.Fatalf("executor cap: %v", err)
	}

	// no match: an event ahead of its record is a 404, not a silent success
	_, err = d.Execute(context.Background(), actionOp(org, "mark_paid", "NOPE", nil))
	if ee := execErr(t, err); ee.Kind != KindNotFound || ee.Code != "record_not_found" {
		t.Fatalf("no match: %+v", ee)
	}
	// another organization's row is invisible
	n := f.execCount()
	_, err = d.Execute(context.Background(), actionOp(org, "mark_paid", "S9", nil))
	if ee := execErr(t, err); ee.Kind != KindNotFound {
		t.Fatalf("other org: %+v", ee)
	}
	if f.execCount() != n {
		t.Fatal("another organization's row was dispatched")
	}
}

func TestStateOpsMatchRequiredAndValuesUsable(t *testing.T) {
	org := uuid.New()
	f := opsFake()
	opsRow(f, org, "S1", "W1", "new")
	d := &DynamicExecutor{store: f}
	for _, kind := range []Op{OpAction, OpTransition, OpRestore} {
		mk := func() Operation {
			op := stateOpOf(org, kind, "S1")
			op.ActionKey, op.To = "mark_paid", "paid"
			return op
		}
		op := mk()
		op.Route.Match = nil
		if _, err := d.Execute(context.Background(), op); execErr(t, err).Code != "match_required" {
			t.Fatalf("%s: no match must never run", kind)
		}
		for name, vals := range map[string]map[string]any{
			"absent": {}, "null": {"sku": nil}, "empty": {"sku": ""}, "blank": {"sku": "   "},
		} {
			op = mk()
			op.Values = vals
			_, err := d.Execute(context.Background(), op)
			if ee := execErr(t, err); ee.Kind != KindInvalid || (ee.Code != "match_value_missing" && ee.Code != "match_value_empty") {
				t.Fatalf("%s %s: %+v", kind, name, ee)
			}
		}
	}
	if f.execCount() != 0 || f.updates != 0 {
		t.Fatal("a rejected match must not touch rows")
	}
}

func TestStateOpsAuthorizeStrings(t *testing.T) {
	org := uuid.New()
	f := opsFake()
	opsRow(f, org, "S1", "W1", "new")
	deleted := opsRow(f, org, "S2", "W1", "new")
	f.rows[1]["deleted_at"] = "then"
	var asked []string
	gate := func(allow bool) func(context.Context, uuid.UUID, string, string) bool {
		return func(_ context.Context, o uuid.UUID, model, action string) bool {
			if o != org || model != "inv" {
				t.Errorf("gate got org %s model %s", o, model)
			}
			asked = append(asked, action)
			return allow
		}
	}
	ops := []Operation{actionOp(org, "mark_paid", "S1", nil), transitionOp(org, "paid", "S1"), stateOpOf(org, OpRestore, "S2")}
	want := []string{"action:mark_paid", "transition:paid", "restore"}
	denied := &DynamicExecutor{store: f, Authorize: gate(false)}
	for i, op := range ops {
		_, err := denied.Execute(context.Background(), op)
		if ee := execErr(t, err); ee.Kind != KindForbidden || ee.Code != "forbidden" {
			t.Fatalf("%s: %+v", want[i], ee)
		}
	}
	if fmt.Sprint(asked) != fmt.Sprint(want) {
		t.Fatalf("gate asked %v want %v", asked, want)
	}
	if f.execCount() != 0 || f.updates != 0 || rowOf(f, deleted)["deleted_at"] == nil {
		t.Fatal("a gated operation must not run")
	}
	allowed := &DynamicExecutor{store: f, Authorize: gate(true)}
	for i, op := range ops {
		if _, err := allowed.Execute(context.Background(), op); err != nil {
			t.Fatalf("%s: %v", want[i], err)
		}
	}
}

func TestRecordActionPassesIdempotencyKeyPerRow(t *testing.T) {
	org := uuid.New()
	f := opsFake()
	opsRow(f, org, "S1", "W1", "new")
	opsRow(f, org, "S1", "W2", "new")
	d := &DynamicExecutor{store: f}
	op := actionOp(org, "bulk", "S1", map[string]any{"reference": "R"})
	op.MaxRows, op.IdempotencyKey = 2, "rcpt-1:3"
	if _, err := d.Execute(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	if len(f.execs) != 2 || f.execs[0].Payload["request_id"] != "rcpt-1:3:0" || f.execs[1].Payload["request_id"] != "rcpt-1:3:1" {
		t.Fatalf("idempotency keys: %+v", f.execs)
	}
	// An action without idempotency gets nothing extra.
	f.execs = nil
	op = actionOp(org, "mark_paid", "S1", nil)
	op.MaxRows, op.IdempotencyKey = 2, "rcpt-2:0"
	if _, err := d.Execute(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.execs {
		if len(c.Payload) != 0 {
			t.Fatalf("unexpected payload %v", c.Payload)
		}
	}
}

func TestRecordActionDryRunNeverDispatches(t *testing.T) {
	org := uuid.New()
	f := opsFake()
	id := opsRow(f, org, "S1", "W1", "new")
	opsRow(f, org, "S2", "W1", "shipped")
	d := &DynamicExecutor{store: f}
	op := actionOp(org, "mark_paid", "S1", map[string]any{"reference": "R"})
	op.DryRun = true
	out, err := d.Execute(context.Background(), op)
	if err != nil || out.WouldDispatch == nil || out.Affected != 0 {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	w := out.WouldDispatch
	if w.Action != "mark_paid" || w.RecordID != id || w.Rows != 1 || w.State != "new" || !w.Eligible || w.Params["reference"] != "R" {
		t.Fatalf("would_dispatch: %+v", w)
	}
	op = actionOp(org, "mark_paid", "S2", nil)
	op.DryRun = true
	if out, err = d.Execute(context.Background(), op); err != nil || out.WouldDispatch == nil || out.WouldDispatch.Eligible || out.WouldDispatch.State != "shipped" {
		t.Fatalf("ineligible dry run: out=%+v err=%v", out, err)
	}
	// the gate and the ingest allow-list apply to a dry run too
	op = actionOp(org, "capture", "S1", nil)
	op.DryRun = true
	if _, err = d.Execute(context.Background(), op); execErr(t, err).Code != CodeActionNotIngestable {
		t.Fatalf("dry run of a closed action: %v", err)
	}
	deny := &DynamicExecutor{store: f, Authorize: func(context.Context, uuid.UUID, string, string) bool { return false }}
	op = actionOp(org, "mark_paid", "S1", nil)
	op.DryRun = true
	if _, err = deny.Execute(context.Background(), op); execErr(t, err).Kind != KindForbidden {
		t.Fatalf("dry run must be gated: %v", err)
	}
	if f.execCount() != 0 {
		t.Fatal("a dry run must never dispatch")
	}
}

func TestRecordActionDispatcherOutcomes(t *testing.T) {
	org := uuid.New()
	f := opsFake()
	opsRow(f, org, "S1", "W1", "new")
	d := &DynamicExecutor{store: f}

	// An approval can not happen for an ingest action; if it does it is a
	// server error, never an applied step.
	f.execFn = func(execCall) (dynamic.ActionResult, error) {
		return dynamic.ActionResult{}, &dynamic.ApprovalRequiredError{}
	}
	_, err := d.Execute(context.Background(), actionOp(org, "mark_paid", "S1", nil))
	if ee := execErr(t, err); ee.Kind != KindInternal || ee.Code != "approval_unexpected" {
		t.Fatalf("approval: %+v", ee)
	}
	// The dispatcher declining is a 422 that never forwards its message.
	f.execFn = func(execCall) (dynamic.ActionResult, error) {
		return dynamic.ActionResult{Success: false, Error: &dynamic.ActionError{Code: "x", Message: "secret detail"}}, nil
	}
	_, err = d.Execute(context.Background(), actionOp(org, "mark_paid", "S1", nil))
	if ee := execErr(t, err); ee.Kind != KindInvalid || ee.Code != "action_declined" || ee.Error() != "action_declined" {
		t.Fatalf("declined: %+v", ee)
	}
	f.execFn = func(execCall) (dynamic.ActionResult, error) {
		return dynamic.ActionResult{Error: &dynamic.ActionError{Code: "idempotency_in_flight"}, HTTPStatus: 409}, nil
	}
	_, err = d.Execute(context.Background(), actionOp(org, "mark_paid", "S1", nil))
	if ee := execErr(t, err); ee.Kind != KindConflict || ee.Code != "idempotency_in_flight" {
		t.Fatalf("in flight: %+v", ee)
	}
	f.execFn = func(execCall) (dynamic.ActionResult, error) {
		return dynamic.ActionResult{}, dynamic.ErrInvalidTransition
	}
	_, err = d.Execute(context.Background(), actionOp(org, "mark_paid", "S1", nil))
	if ee := execErr(t, err); ee.Kind != KindInvalid || ee.Code != CodeInvalidTransition {
		t.Fatalf("transition: %+v", ee)
	}
	f.execFn = func(execCall) (dynamic.ActionResult, error) {
		return dynamic.ActionResult{}, dynamic.ErrActionNotFound
	}
	_, err = d.Execute(context.Background(), actionOp(org, "mark_paid", "S1", nil))
	if ee := execErr(t, err); ee.Kind != KindInvalid || ee.Code != CodeActionNotIngestable {
		t.Fatalf("not found: %+v", ee)
	}
}

// -------------------------------------------------------- record.transition ---

func TestRecordTransition(t *testing.T) {
	org := uuid.New()
	f := opsFake()
	id := opsRow(f, org, "S1", "W1", "new")
	d := &DynamicExecutor{store: f}

	out, err := d.Execute(context.Background(), transitionOp(org, "paid", "S1"))
	if err != nil || out.Affected != 1 || out.Noop || out.RecordID != id {
		t.Fatalf("valid: out=%+v err=%v", out, err)
	}
	if rowOf(f, id)["status"] != "paid" {
		t.Fatalf("stage not written: %v", rowOf(f, id))
	}
	// from == to is a noop and writes nothing
	updates := f.updates
	out, err = d.Execute(context.Background(), transitionOp(org, "paid", "S1"))
	if err != nil || !out.Noop || out.Reason != CodeAlreadyInState || out.Affected != 0 || f.updates != updates {
		t.Fatalf("noop: out=%+v err=%v updates=%d", out, err, f.updates-updates)
	}
	// paid -> new is not a declared move
	_, err = d.Execute(context.Background(), transitionOp(org, "new", "S1"))
	if ee := execErr(t, err); ee.Kind != KindInvalid || ee.Code != CodeInvalidTransition {
		t.Fatalf("invalid: %+v", ee)
	}
	if rowOf(f, id)["status"] != "paid" || f.updates != updates {
		t.Fatal("a refused move must not write")
	}
	// the rest of the walls
	_, err = d.Execute(context.Background(), transitionOp(org, "shipped", "NOPE"))
	if ee := execErr(t, err); ee.Kind != KindNotFound {
		t.Fatalf("no row: %+v", ee)
	}
	other := opsFake()
	opsRow(other, uuid.New(), "S1", "W1", "new")
	_, err = (&DynamicExecutor{store: other}).Execute(context.Background(), transitionOp(org, "paid", "S1"))
	if ee := execErr(t, err); ee.Kind != KindNotFound {
		t.Fatalf("other org: %+v", ee)
	}
	// a model without an active machine cannot transition at runtime either
	f.sm = nil
	_, err = d.Execute(context.Background(), transitionOp(org, "shipped", "S1"))
	if ee := execErr(t, err); ee.Kind != KindInvalid || ee.Code != "no_stage_machine" {
		t.Fatalf("no machine: %+v", ee)
	}
}

func TestRecordTransitionJudgesEveryRowBeforeWriting(t *testing.T) {
	org := uuid.New()
	f := opsFake()
	a := opsRow(f, org, "S1", "W1", "new")
	b := opsRow(f, org, "S1", "W2", "shipped") // shipped -> paid is not declared
	d := &DynamicExecutor{store: f}
	op := transitionOp(org, "paid", "S1")
	op.MaxRows = 2
	_, err := d.Execute(context.Background(), op)
	if ee := execErr(t, err); ee.Code != CodeInvalidTransition {
		t.Fatalf("%+v", ee)
	}
	if rowOf(f, a)["status"] != "new" || rowOf(f, b)["status"] != "shipped" || f.updates != 0 {
		t.Fatal("one refused row must leave the others untouched")
	}
	_, err = d.Execute(context.Background(), transitionOp(org, "paid", "S1"))
	if ee := execErr(t, err); ee.Kind != KindConflict || ee.Code != "too_many_matches" {
		t.Fatalf("cap: %+v", ee)
	}
}

// ---------------------------------------------------------- record.restore ---

func TestRecordRestore(t *testing.T) {
	org, other := uuid.New(), uuid.New()
	f := opsFake()
	gone := opsRow(f, org, "S1", "W1", "new")
	f.rows[0]["deleted_at"] = "then"
	foreign := opsRow(f, other, "S2", "W1", "new")
	f.rows[1]["deleted_at"] = "then"
	d := &DynamicExecutor{store: f}

	out, err := d.Execute(context.Background(), stateOpOf(org, OpRestore, "S1"))
	if err != nil || out.Affected != 1 || out.RecordID != gone || out.Noop || out.NotFound {
		t.Fatalf("restore: out=%+v err=%v", out, err)
	}
	if rowOf(f, gone)["deleted_at"] != nil {
		t.Fatal("the tombstone was not cleared")
	}
	// already live: a retry converges as a noop
	out, err = d.Execute(context.Background(), stateOpOf(org, OpRestore, "S1"))
	if err != nil || !out.Noop || out.Reason != CodeAlreadyLive || out.Affected != 0 {
		t.Fatalf("already live: out=%+v err=%v", out, err)
	}
	// nothing matches: not_found, a successful no-op like record.delete
	out, err = d.Execute(context.Background(), stateOpOf(org, OpRestore, "NOPE"))
	if err != nil || !out.NotFound || out.Affected != 0 {
		t.Fatalf("no match: out=%+v err=%v", out, err)
	}
	// the other organization's tombstone is invisible
	out, err = d.Execute(context.Background(), stateOpOf(org, OpRestore, "S2"))
	if err != nil || !out.NotFound || rowOf(f, foreign)["deleted_at"] == nil {
		t.Fatalf("other org: out=%+v err=%v", out, err)
	}
}

func TestRecordRestoreRefusesToDuplicateAKey(t *testing.T) {
	org := uuid.New()
	f := opsFake()
	old := opsRow(f, org, "S1", "W1", "new")
	f.rows[0]["deleted_at"] = "then"
	opsRow(f, org, "S1", "W1", "new") // recreated after the delete
	d := &DynamicExecutor{store: f}
	_, err := d.Execute(context.Background(), stateOpOf(org, OpRestore, "S1"))
	if ee := execErr(t, err); ee.Kind != KindConflict || ee.Code != "too_many_matches" {
		t.Fatalf("%+v", ee)
	}
	if rowOf(f, old)["deleted_at"] == nil {
		t.Fatal("restoring next to a live row would have duplicated the key")
	}
}

func TestRecordRestoreManyTombstonesNeedAHigherCap(t *testing.T) {
	org := uuid.New()
	f := opsFake()
	opsRow(f, org, "S1", "W1", "new")
	opsRow(f, org, "S1", "W2", "new")
	f.rows[0]["deleted_at"], f.rows[1]["deleted_at"] = "then", "then"
	d := &DynamicExecutor{store: f}
	if _, err := d.Execute(context.Background(), stateOpOf(org, OpRestore, "S1")); execErr(t, err).Code != "too_many_matches" {
		t.Fatalf("default cap: %v", err)
	}
	op := stateOpOf(org, OpRestore, "S1")
	op.MaxRows = 2
	if out, err := d.Execute(context.Background(), op); err != nil || out.Affected != 2 {
		t.Fatalf("cap 2: out=%+v err=%v", out, err)
	}
}

func TestRecordRestoreNeedsTombstonesAndSoftDelete(t *testing.T) {
	org := uuid.New()
	f := opsFake()
	opsRow(f, org, "S1", "W1", "new")
	f.rows[0]["deleted_at"] = "then"
	_, err := (&DynamicExecutor{store: noTomb{f}}).Execute(context.Background(), stateOpOf(org, OpRestore, "S1"))
	if ee := execErr(t, err); ee.Kind != KindInternal || ee.Code != "tombstones_unavailable" {
		t.Fatalf("no finder: %+v", ee)
	}
	g := opsFake()
	g.noSoftDelete = true
	opsRow(g, org, "S1", "W1", "new")
	_, err = (&DynamicExecutor{store: g}).Execute(context.Background(), stateOpOf(org, OpRestore, "S1"))
	if ee := execErr(t, err); ee.Code != "soft_delete_unsupported" {
		t.Fatalf("no deleted_at: %+v", ee)
	}
}

// ------------------------------------------------------ registration checks ---

func stateOpContract(a Action) Contract {
	if a.Model == "" {
		a.Model = "inv"
	}
	if a.Match == nil {
		a.Match = []string{"sku"}
		a.Mapping = map[string]string{"sku": "{{payload.sku}}"}
	}
	return Contract{Key: "order.state", Version: 1, Actions: []Action{a}}
}

func TestCheckContractRecordAction(t *testing.T) {
	ctx := context.Background()
	d := &DynamicExecutor{store: opsFake()}
	do := func(a Action) error {
		a.Do = ActionRecordAction
		return d.CheckContract(ctx, stateOpContract(a))
	}
	if err := do(Action{ActionKey: "mark_paid", Params: map[string]any{"reference": "{{payload.reference}}"}}); err != nil {
		t.Fatalf("valid: %v", err)
	}
	if err := do(Action{ActionKey: "mark_paid", Params: map[string]any{"reason": "late"}}); err != nil {
		t.Fatalf("reason is always allowed: %v", err)
	}
	if err := do(Action{ActionKey: "bulk", MaxRows: 5, Params: map[string]any{"reference": "x"}}); err != nil {
		t.Fatalf("idempotent action may span rows: %v", err)
	}
	bad := map[string]Action{
		"unknown action":        {ActionKey: "ghost"},
		"not opted in":          {ActionKey: "capture"},
		"supervisor policy":     {ActionKey: "refund"},
		"approval":              {ActionKey: "approve"},
		"confirm typed":         {ActionKey: "confirm"},
		"table placement":       {ActionKey: "toolbar"},
		"create placement":      {ActionKey: "creation"},
		"param outside fields":  {ActionKey: "mark_paid", Params: map[string]any{"amount": 1}},
		"approval id param":     {ActionKey: "mark_paid", Params: map[string]any{"approval_id": "x"}},
		"idempotency key param": {ActionKey: "bulk", Params: map[string]any{"request_id": "x"}},
		"many rows no idem":     {ActionKey: "mark_paid", MaxRows: 2},
		"match not a column":    {ActionKey: "mark_paid", Match: []string{"nope"}, Mapping: map[string]string{"nope": "{{payload.sku}}"}},
		"max_rows over the cap": {ActionKey: "bulk", MaxRows: 101},
	}
	for name, a := range bad {
		if err := do(a); !errors.Is(err, ErrInvalidContract) {
			t.Errorf("%s must be rejected at registration, got %v", name, err)
		}
	}
	if err := do(Action{ActionKey: "capture"}); err == nil || !strings.Contains(err.Error(), CodeActionNotIngestable) {
		t.Errorf("the error must name %s: %v", CodeActionNotIngestable, err)
	}
	dc := &DynamicExecutor{store: opsFake(), MaxDeleteRows: 3}
	a := Action{Do: ActionRecordAction, ActionKey: "bulk", MaxRows: 4}
	if err := dc.CheckContract(ctx, stateOpContract(a)); !errors.Is(err, ErrInvalidContract) {
		t.Errorf("executor cap: %v", err)
	}
}

func TestCheckContractRecordTransitionAndRestore(t *testing.T) {
	ctx := context.Background()
	d := &DynamicExecutor{store: opsFake()}
	tr := func(to string) error {
		return d.CheckContract(ctx, stateOpContract(Action{Do: ActionRecordTransition, To: to}))
	}
	for _, to := range []string{"new", "paid", "shipped"} {
		if err := tr(to); err != nil {
			t.Fatalf("%s: %v", to, err)
		}
	}
	if err := tr("lost"); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("a state outside the machine: %v", err)
	}
	noMachine := opsFake()
	noMachine.sm = nil
	if err := (&DynamicExecutor{store: noMachine}).CheckContract(ctx, stateOpContract(Action{Do: ActionRecordTransition, To: "paid"})); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("a model without machine: %v", err)
	}

	rs := Action{Do: ActionRecordRestore}
	if err := d.CheckContract(ctx, stateOpContract(rs)); err != nil {
		t.Fatalf("restore: %v", err)
	}
	soft := opsFake()
	soft.noSoftDelete = true
	ao := opsFake()
	ao.appendOnly = true
	for name, store := range map[string]dynStore{
		"no deleted_at": soft, "append-only": ao, "no tombstone finder": noTomb{opsFake()},
	} {
		if err := (&DynamicExecutor{store: store}).CheckContract(ctx, stateOpContract(rs)); !errors.Is(err, ErrInvalidContract) {
			t.Errorf("restore on %s must be rejected: %v", name, err)
		}
	}
	// the match columns are checked like in record.delete
	bad := stateOpContract(Action{Do: ActionRecordRestore, Match: []string{"nope"}, Mapping: map[string]string{"nope": "{{payload.sku}}"}})
	if err := d.CheckContract(ctx, bad); !errors.Is(err, ErrInvalidContract) {
		t.Errorf("undeclared match column: %v", err)
	}
}

// ---------------------------------------------------------- compile (data) ---

const opsSchema = `{"type":"object","required":["event_id","sku"],"additionalProperties":false,
 "properties":{"event_id":{"type":"string"},"sku":{"type":"string"},"reference":{"type":"string"}}}`

func opsContract(key string, as ...Action) Contract {
	for i := range as {
		if as[i].Model == "" {
			as[i].Model = "inv"
		}
		if as[i].Match == nil {
			as[i].Match = []string{"sku"}
			as[i].Mapping = map[string]string{"sku": "{{payload.sku}}"}
		}
	}
	return Contract{Key: key, Version: 1, JSONSchema: json.RawMessage(opsSchema), Idempotency: "{event_id}",
		Auth: ProfileHMACCanonicalV1, Actions: as}
}

func TestStateOpsContractValidation(t *testing.T) {
	good := []Action{
		{Do: ActionRecordAction, ActionKey: "mark_paid", Params: map[string]any{"reference": "{{payload.reference}}"}, OnIneligible: OnIneligibleFail, MaxRows: 100},
		{Do: ActionRecordTransition, To: "paid"},
		{Do: ActionRecordRestore, MaxRows: 3},
	}
	for _, a := range good {
		if err := NewRegistry().Register(opsContract("c.ok", a)); err != nil {
			t.Fatalf("%s: %v", a.Do, err)
		}
	}
	bad := map[string]Action{
		"action: no key":             {Do: ActionRecordAction},
		"action: bad key":            {Do: ActionRecordAction, ActionKey: "a b"},
		"action: bad policy":         {Do: ActionRecordAction, ActionKey: "x", OnIneligible: "skip"},
		"action: bad param template": {Do: ActionRecordAction, ActionKey: "x", Params: map[string]any{"a": "{{payload.nope}}"}},
		"action: to":                 {Do: ActionRecordAction, ActionKey: "x", To: "paid"},
		"action: mode":               {Do: ActionRecordAction, ActionKey: "x", Mode: DeleteSoft},
		"action: defaults":           {Do: ActionRecordAction, ActionKey: "x", Defaults: map[string]any{"a": 1}},
		"action: guard":              {Do: ActionRecordAction, ActionKey: "x", Guard: &Guard{Field: "a", Path: "payload.sku", Op: GuardGTE}},
		"transition: no to":          {Do: ActionRecordTransition},
		"transition: blank to":       {Do: ActionRecordTransition, To: "  "},
		"transition: action":         {Do: ActionRecordTransition, To: "paid", ActionKey: "x"},
		"transition: params":         {Do: ActionRecordTransition, To: "paid", Params: map[string]any{"a": 1}},
		"transition: on_ineligible":  {Do: ActionRecordTransition, To: "paid", OnIneligible: OnIneligibleFail},
		"restore: to":                {Do: ActionRecordRestore, To: "paid"},
		"restore: params":            {Do: ActionRecordRestore, Params: map[string]any{"a": 1}},
		"restore: max_rows too high": {Do: ActionRecordRestore, MaxRows: 101},
		"negative max_rows":          {Do: ActionRecordTransition, To: "paid", MaxRows: -1},
		"no match":                   {Do: ActionRecordRestore, Match: []string{}, Mapping: map[string]string{"sku": "{{payload.sku}}"}},
		"mapping not in match":       {Do: ActionRecordTransition, To: "paid", Match: []string{"sku"}, Mapping: map[string]string{"sku": "{{payload.sku}}", "status": "{{payload.reference}}"}},
		"no model":                   {Do: ActionRecordRestore, Model: "-"},
	}
	for name, a := range bad {
		c := opsContract("c.bad", a)
		if name == "no model" {
			c.Actions[0].Model = ""
			c.Actions[0].Match, c.Actions[0].Mapping = []string{"sku"}, map[string]string{"sku": "{{payload.sku}}"}
		}
		if name == "no match" {
			c.Actions[0].Match = nil
		}
		if err := NewRegistry().Register(c); !errors.Is(err, ErrInvalidContract) {
			t.Errorf("%s must be rejected, got %v", name, err)
		}
	}
	// result.noop is a valid reference for the steps that follow
	c := opsContract("c.noop", Action{Do: ActionRecordAction, ActionKey: "mark_paid"},
		Action{Do: ActionRecordTransition, To: "paid", When: &Condition{Path: "result.noop", Eq: json.RawMessage(`true`)}})
	if err := NewRegistry().Register(c); err != nil {
		t.Fatalf("result.noop: %v", err)
	}
}

// deleteOnlyExec only declares OpDelete, like an Executor written before the new
// operations existed.
type deleteOnlyExec struct{ *fakeExec }

func (deleteOnlyExec) SupportsOp(op Op) bool { return op == OpDelete }

func TestStateOpsFailClosedWithoutOpSupport(t *testing.T) {
	for _, a := range []Action{
		{Do: ActionRecordAction, ActionKey: "mark_paid"}, {Do: ActionRecordTransition, To: "paid"}, {Do: ActionRecordRestore},
	} {
		reg := NewRegistry()
		if err := reg.Register(opsContract("c.legacy", a)); err != nil {
			t.Fatal(err)
		}
		cfg := Config{Contracts: reg, Verifiers: map[string]Verifier{ProfileHMACCanonicalV1: &HMACCanonicalV1{}},
			Receipts: NewMemoryReceiptStore(), Executor: deleteOnlyExec{newFakeExec()}, Logger: quietLogger(), DefaultAuth: ProfileHMACCanonicalV1}
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: an Executor that does not declare its Op must be refused", a.Do)
		}
		cfg.Executor = newFakeExec() // declares every Op
		if _, err := New(cfg); err != nil {
			t.Errorf("%s: %v", a.Do, err)
		}
	}
}

// ------------------------------------------------------------- over HTTP ---

func opsEnv(t *testing.T, f *fakeDyn, mut func(*DynamicExecutor), cs ...Contract) *env {
	t.Helper()
	reg := NewRegistry()
	for _, c := range cs {
		if err := reg.Register(c); err != nil {
			t.Fatal(err)
		}
	}
	ex := &DynamicExecutor{store: f}
	if mut != nil {
		mut(ex)
	}
	return newEnv(t, func(cfg *Config) { cfg.Contracts, cfg.Executor = reg, ex })
}

func opsBody(id, sku string) string {
	return fmt.Sprintf(`{"event_id":%q,"sku":%q,"reference":"R-1"}`, id, sku)
}

func TestStateOpsChainOverHTTP(t *testing.T) {
	f := opsFake()
	c := opsContract("order.paid",
		Action{Do: ActionRecordAction, ActionKey: "mark_paid", Params: map[string]any{"reference": "{{payload.reference}}"}},
		Action{Do: ActionRecordTransition, To: "paid", When: &Condition{Path: "result.noop", Eq: json.RawMessage(`true`)}},
	)
	e := opsEnv(t, f, nil, c)
	idNew := opsRow(f, e.orgA, "S-NEW", "W", "new")
	idPaid := opsRow(f, e.orgA, "S-PAID", "W", "paid")

	// status new: the action runs; result.noop is false so the transition is skipped
	code, out := e.send(t, e.orgA, c.Key, opsBody("e1", "S-NEW"), nil)
	if code != 200 || out["status"] != "applied" || out["record_id"] != idNew {
		t.Fatalf("%d %v", code, out)
	}
	if s := stepAt(t, out, 0); s["status"] != StepDone || s["noop"] != nil || s["record_id"] != idNew || s["affected"] != float64(1) {
		t.Fatalf("step 0: %v", s)
	}
	if s := stepAt(t, out, 1); s["status"] != StepSkipped {
		t.Fatalf("step 1: %v", s)
	}
	if f.execCount() != 1 || f.execs[0].Payload["reference"] != "R-1" {
		t.Fatalf("execs: %+v", f.execs)
	}

	// status paid: not eligible for mark_paid -> noop; the next step sees result.noop
	code, out = e.send(t, e.orgA, c.Key, opsBody("e2", "S-PAID"), nil)
	if code != 200 || out["status"] != "applied" {
		t.Fatalf("%d %v", code, out)
	}
	s0, s1 := stepAt(t, out, 0), stepAt(t, out, 1)
	if s0["status"] != StepDone || s0["noop"] != true || s0["reason"] != CodeStateNotEligible {
		t.Fatalf("step 0: %v", s0)
	}
	if s1["status"] != StepDone || s1["noop"] != true || s1["reason"] != CodeAlreadyInState || s1["record_id"] != idPaid {
		t.Fatalf("step 1: %v", s1)
	}
	if f.execCount() != 1 {
		t.Fatal("the ineligible row must not be dispatched")
	}

	// the same event again is a replay: nothing runs twice
	code, out = e.send(t, e.orgA, c.Key, opsBody("e1", "S-NEW"), nil)
	if code != 200 || out["status"] != "replayed" || f.execCount() != 1 {
		t.Fatalf("replay: %d %v execs=%d", code, out, f.execCount())
	}
}

func TestStateOpsHTTPErrors(t *testing.T) {
	f := opsFake()
	transition := opsContract("order.ship", Action{Do: ActionRecordTransition, To: "shipped"})
	paid := opsContract("order.pay", Action{Do: ActionRecordAction, ActionKey: "mark_paid", OnIneligible: OnIneligibleFail})
	var deny bool
	e := opsEnv(t, f, func(d *DynamicExecutor) {
		d.Authorize = func(_ context.Context, _ uuid.UUID, _, action string) bool { return !deny || action == "never" }
	}, transition, paid)
	opsRow(f, e.orgA, "S1", "W", "new")

	// new -> shipped is not declared: 422 invalid_transition
	code, out := e.send(t, e.orgA, transition.Key, opsBody("t1", "S1"), nil)
	if code != 422 || errCode(out) != CodeInvalidTransition {
		t.Fatalf("%d %v", code, out)
	}
	// no row in this organization: 404, and the event can be retried later
	code, out = e.send(t, e.orgB, transition.Key, opsBody("t2", "S1"), nil)
	if code != 404 || errCode(out) != "record_not_found" {
		t.Fatalf("other org: %d %v", code, out)
	}
	// ineligible with on_ineligible: fail -> 409
	opsRow(f, e.orgA, "S2", "W", "shipped")
	code, out = e.send(t, e.orgA, paid.Key, opsBody("p1", "S2"), nil)
	if code != 409 || errCode(out) != CodeStateNotEligible {
		t.Fatalf("fail policy: %d %v", code, out)
	}
	// the host gate denies: 403 forbidden
	deny = true
	code, out = e.send(t, e.orgA, paid.Key, opsBody("p2", "S1"), nil)
	if code != 403 || errCode(out) != "forbidden" {
		t.Fatalf("gate: %d %v", code, out)
	}
	if f.execCount() != 0 {
		t.Fatal("nothing may be dispatched")
	}
}

// ---------------------------------------------------------------- targets ---

func TestIngestActionsProjection(t *testing.T) {
	defs := []manifest.ActionDef{
		{Key: "reserve", Label: "Reservar", Ingest: true, RequiresState: []string{"active"},
			Idempotency: &manifest.IdempotencyDef{KeyField: "request_id"},
			Fields: []manifest.FieldDef{
				{Key: "quantity", Label: "Cantidad", Type: "number", Required: true},
				{Name: "note", Type: "text"},
				{Key: "request_id"},
			},
			Steps: []manifest.ActionStepDef{{Title: "s", Fields: []manifest.FieldDef{{Key: "extra"}}}}},
		{Key: "capture", Label: "Cobrar"},
		{Key: "refund", Ingest: true, SupervisorPolicy: "refund"},
		{Key: "approve", Ingest: true, Approval: &manifest.ApprovalDef{Roles: []string{"m"}}},
		{Key: "typed", Ingest: true, ConfirmTyped: &v3.ConfirmTyped{Field: "sku"}},
		{Key: "bar", Ingest: true, Placement: "table"},
	}
	got := IngestActions("inventory.items", defs)
	if len(got) != 1 || got[0].Key != "reserve" || got[0].Label != "Reservar" || got[0].Permission != "inventory.items.action:reserve" ||
		len(got[0].RequiresState) != 1 {
		t.Fatalf("only ingest actions are executable: %+v", got)
	}
	names := []string{}
	for _, p := range got[0].Params {
		names = append(names, p.Name)
	}
	if fmt.Sprint(names) != "[quantity note extra]" || !got[0].Params[0].Required {
		t.Fatalf("params (the idempotency key is the kernel's): %+v", got[0].Params)
	}
	if got := IngestActions("m", nil); got == nil || len(got) != 0 {
		t.Fatalf("never nil: %#v", got)
	}
}

func TestStageOfAndRestorable(t *testing.T) {
	if StageOf(nil) != nil || StageOf(&dynamic.StageMachine{Field: "status"}) != nil || StageOf(&dynamic.StageMachine{Stages: []manifest.StageDef{{Key: "a"}}}) != nil {
		t.Fatal("an inert machine is null")
	}
	st := StageOf(opsFake().sm)
	if st == nil || st.Field != "status" || len(st.Stages) != 3 || st.Stages[1].Key != "paid" || len(st.Transitions) != 2 || st.Transitions[0] != (TargetTransition{From: "new", To: "paid"}) {
		t.Fatalf("%+v", st)
	}
	soft := &modelbase.TableMetadata{Audit: &modelbase.AuditMeta{DeletedAt: "deleted_at"}}
	ao := &modelbase.TableMetadata{Audit: &modelbase.AuditMeta{DeletedAt: "deleted_at"}, AppendOnly: true}
	if !Restorable(soft) || Restorable(ao) || Restorable(&modelbase.TableMetadata{}) || Restorable(nil) {
		t.Fatal("restorable = soft delete and not append-only")
	}
}
