package inbound

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/modelbase"
)

// RFC 0003 §2.4: record.delete retires rows selected only by match.

// ----------------------------------------------------------- executor ---

func seedRow(f *fakeDyn, org uuid.UUID, sku, wh string) string {
	id := uuid.NewString()
	f.rows = append(f.rows, map[string]any{"id": id, "organization_id": org.String(), "sku": sku, "warehouse": wh})
	return id
}

func delOp(org uuid.UUID, mode string, max int, sku string) Operation {
	return Operation{
		Org: org, Route: Route{Model: "inv", Op: OpDelete, Match: []string{"sku"}},
		Values: map[string]any{"sku": sku}, DeleteMode: mode, MaxRows: max,
	}
}

func execErr(t *testing.T, err error) *ExecError {
	t.Helper()
	var ee *ExecError
	if !errors.As(err, &ee) {
		t.Fatalf("not an ExecError: %v", err)
	}
	return ee
}

func deleted(f *fakeDyn, id string) bool {
	for _, r := range f.rows {
		if r["id"] == id {
			return r["deleted_at"] != nil
		}
	}
	return true // physically gone
}

type fakeHard struct {
	f     *fakeDyn
	calls int32
}

func (h *fakeHard) HardDelete(_ context.Context, _ string, u modelbase.AuthUser, id uuid.UUID) error {
	atomic.AddInt32(&h.calls, 1)
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	for i, r := range h.f.rows {
		if r["id"] == id.String() && r["organization_id"] == u.GetOrganizationID().String() {
			h.f.rows = append(h.f.rows[:i], h.f.rows[i+1:]...)
			return nil
		}
	}
	return nil
}

func TestDeleteSoftSuccess(t *testing.T) {
	org := uuid.New()
	f := newFakeDyn()
	id := seedRow(f, org, "S1", "W1")
	other := seedRow(f, org, "S2", "W1")
	d := &DynamicExecutor{store: f}
	out, err := d.Execute(context.Background(), delOp(org, "", 0, "S1"))
	if err != nil {
		t.Fatal(err)
	}
	if out.Affected != 1 || out.RecordID != id || out.NotFound || out.Created {
		t.Fatalf("outcome %+v", out)
	}
	if !deleted(f, id) || deleted(f, other) {
		t.Fatalf("only the matched row must be tombstoned: %v", f.rows)
	}
	if len(f.rows) != 2 {
		t.Fatal("soft delete must keep the row")
	}
}

func TestDeleteNoMatchIsNotFoundNoError(t *testing.T) {
	org := uuid.New()
	f := newFakeDyn()
	seedRow(f, org, "S2", "W1")
	d := &DynamicExecutor{store: f}
	out, err := d.Execute(context.Background(), delOp(org, "", 0, "S1"))
	if err != nil || !out.NotFound || out.Affected != 0 || out.RecordID != "" {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if atomic.LoadInt32(&f.deletes) != 0 {
		t.Fatal("nothing to delete")
	}
}

func TestDeleteMultiMatchOverCapDeletesNothing(t *testing.T) {
	org := uuid.New()
	f := newFakeDyn()
	a, b := seedRow(f, org, "S1", "W1"), seedRow(f, org, "S1", "W2")
	d := &DynamicExecutor{store: f}
	// default cap is 1 row
	_, err := d.Execute(context.Background(), delOp(org, "", 0, "S1"))
	if ee := execErr(t, err); ee.Kind != KindConflict || ee.Code != "too_many_matches" {
		t.Fatalf("%+v", ee)
	}
	if deleted(f, a) || deleted(f, b) || atomic.LoadInt32(&f.deletes) != 0 {
		t.Fatal("an over-cap match must not delete anything")
	}
	// an explicit cap that covers the matches deletes them all
	out, err := d.Execute(context.Background(), delOp(org, "", 2, "S1"))
	if err != nil || out.Affected != 2 || !deleted(f, a) || !deleted(f, b) {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}

func TestDeleteExecutorCapAndMatchRequired(t *testing.T) {
	org := uuid.New()
	f := newFakeDyn()
	seedRow(f, org, "S1", "W1")
	d := &DynamicExecutor{store: f, MaxDeleteRows: 3}
	_, err := d.Execute(context.Background(), delOp(org, "", 4, "S1"))
	if ee := execErr(t, err); ee.Kind != KindInvalid || ee.Code != "max_rows_exceeds_limit" {
		t.Fatalf("%+v", ee)
	}
	// a delete with no match fields never runs (no filter = no delete)
	op := delOp(org, "", 0, "S1")
	op.Route.Match = nil
	_, err = d.Execute(context.Background(), op)
	if ee := execErr(t, err); ee.Code != "match_required" {
		t.Fatalf("%+v", ee)
	}
	// match value missing from the event
	op = delOp(org, "", 0, "S1")
	op.Values = map[string]any{}
	_, err = d.Execute(context.Background(), op)
	if ee := execErr(t, err); ee.Code != "match_value_missing" {
		t.Fatalf("%+v", ee)
	}
	// unknown mode
	_, err = d.Execute(context.Background(), delOp(org, "purge", 0, "S1"))
	if ee := execErr(t, err); ee.Code != "bad_delete_mode" {
		t.Fatalf("%+v", ee)
	}
	if atomic.LoadInt32(&f.deletes) != 0 {
		t.Fatal("rejected deletes must not touch rows")
	}
}

func TestDeleteGate_ModelNotWritableAndNoPermission(t *testing.T) {
	org := uuid.New()
	f := newFakeDyn()
	id := seedRow(f, org, "S1", "W1")
	type ask struct{ model, action string }
	var asked []ask
	deny := func(why string) func(context.Context, uuid.UUID, string, string) bool {
		return func(_ context.Context, o uuid.UUID, model, action string) bool {
			asked = append(asked, ask{model, action})
			if o != org {
				t.Errorf("gate got org %s", o)
			}
			return false
		}
	}
	// model outside the writable allow-list, or entry without <model>.delete:
	// both surface as the host's gate answering false.
	for _, why := range []string{"model not writable", "no permission"} {
		d := &DynamicExecutor{store: f, Authorize: deny(why)}
		_, err := d.Execute(context.Background(), delOp(org, "", 0, "S1"))
		if ee := execErr(t, err); ee.Kind != KindForbidden || ee.Code != "forbidden" {
			t.Fatalf("%s: %+v", why, ee)
		}
	}
	if deleted(f, id) || atomic.LoadInt32(&f.deletes) != 0 {
		t.Fatal("a gated delete must not run")
	}
	if len(asked) != 2 || asked[0] != (ask{"inv", "delete"}) {
		t.Fatalf("gate asked %v", asked)
	}
	// the same gate allows it when granted
	d := &DynamicExecutor{store: f, Authorize: func(_ context.Context, _ uuid.UUID, m, a string) bool { return m == "inv" && a == "delete" }}
	if out, err := d.Execute(context.Background(), delOp(org, "", 0, "S1")); err != nil || out.Affected != 1 {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}

func TestDeleteGateAlsoCoversUpdate(t *testing.T) {
	org := uuid.New()
	f := newFakeDyn()
	seedRow(f, org, "S1", "W1")
	var actions []string
	d := &DynamicExecutor{store: f, Authorize: func(_ context.Context, _ uuid.UUID, _, a string) bool {
		actions = append(actions, a)
		return a != "update"
	}}
	op := Operation{Org: org, Route: Route{Model: "inv", Op: OpUpdate, Match: []string{"sku"}},
		Values: map[string]any{"sku": "S1", "quantity": int64(1)}}
	if _, err := d.Execute(context.Background(), op); execErr(t, err).Code != "forbidden" {
		t.Fatalf("update must pass the same gate: %v", err)
	}
	if len(actions) != 1 || actions[0] != "update" || atomic.LoadInt32(&f.updates) != 0 {
		t.Fatalf("actions %v updates %d", actions, f.updates)
	}
}

func TestDeleteOtherOrganizationUntouched(t *testing.T) {
	orgA, orgB := uuid.New(), uuid.New()
	f := newFakeDyn()
	idB := seedRow(f, orgB, "S1", "W1")
	d := &DynamicExecutor{store: f}
	out, err := d.Execute(context.Background(), delOp(orgA, "", 0, "S1"))
	if err != nil || !out.NotFound {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if deleted(f, idB) {
		t.Fatal("another organization's row must be invisible and untouched")
	}
	// the hard path is tenant scoped too
	h := &fakeHard{f: f}
	dh := &DynamicExecutor{store: f, HardDeleter: h, HardDeleteModels: map[string]bool{"inv": true}}
	out, err = dh.Execute(context.Background(), delOp(orgA, DeleteHard, 0, "S1"))
	if err != nil || !out.NotFound || deleted(f, idB) || atomic.LoadInt32(&h.calls) != 0 {
		t.Fatalf("hard: out=%+v err=%v", out, err)
	}
}

func TestDeleteIsIdempotent(t *testing.T) {
	org := uuid.New()
	f := newFakeDyn()
	id := seedRow(f, org, "S1", "W1")
	d := &DynamicExecutor{store: f}
	first, err := d.Execute(context.Background(), delOp(org, "", 0, "S1"))
	if err != nil || first.Affected != 1 || first.RecordID != id {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	second, err := d.Execute(context.Background(), delOp(org, "", 0, "S1"))
	if err != nil || !second.NotFound || second.Affected != 0 {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	if atomic.LoadInt32(&f.deletes) != 1 {
		t.Fatalf("deletes=%d, the replay must not delete again", f.deletes)
	}
}

func TestDeleteSoftVsHard(t *testing.T) {
	org := uuid.New()
	ctx := context.Background()

	// soft is the default and keeps the row.
	f := newFakeDyn()
	soft := seedRow(f, org, "S1", "W1")
	h := &fakeHard{f: f}
	d := &DynamicExecutor{store: f, HardDeleter: h, HardDeleteModels: map[string]bool{"inv": true}}
	if _, err := d.Execute(ctx, delOp(org, "", 0, "S1")); err != nil {
		t.Fatal(err)
	}
	if !deleted(f, soft) || len(f.rows) != 1 || atomic.LoadInt32(&h.calls) != 0 {
		t.Fatalf("default must be a soft delete: %v", f.rows)
	}

	// hard removes the row when the model is explicitly enabled.
	hard := seedRow(f, org, "S3", "W1")
	if out, err := d.Execute(ctx, delOp(org, DeleteHard, 0, "S3")); err != nil || out.Affected != 1 || out.RecordID != hard {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if atomic.LoadInt32(&h.calls) != 1 {
		t.Fatal("hard delete must go through the HardDeleter")
	}
	for _, r := range f.rows {
		if r["id"] == hard {
			t.Fatal("hard delete must remove the row")
		}
	}

	// hard is refused with no deleter, or when the model is not listed.
	seedRow(f, org, "S4", "W1")
	for name, ex := range map[string]*DynamicExecutor{
		"no deleter":   {store: f, HardDeleteModels: map[string]bool{"inv": true}},
		"not listed":   {store: f, HardDeleter: h},
		"other listed": {store: f, HardDeleter: h, HardDeleteModels: map[string]bool{"other": true}},
	} {
		_, err := ex.Execute(ctx, delOp(org, DeleteHard, 0, "S4"))
		if ee := execErr(t, err); ee.Kind != KindForbidden || ee.Code != "hard_delete_disabled" {
			t.Fatalf("%s: %+v", name, ee)
		}
	}

	// a model without deleted_at never falls back to a physical delete.
	g := newFakeDyn()
	g.noSoftDelete = true
	gid := seedRow(g, org, "S1", "W1")
	dg := &DynamicExecutor{store: g}
	_, err := dg.Execute(ctx, delOp(org, "", 0, "S1"))
	if ee := execErr(t, err); ee.Kind != KindInvalid || ee.Code != "soft_delete_unsupported" {
		t.Fatalf("%+v", ee)
	}
	if deleted(g, gid) || atomic.LoadInt32(&g.deletes) != 0 {
		t.Fatal("unsupported soft delete must not delete")
	}
}

func TestDeleteCheckContract(t *testing.T) {
	mk := func(a Action) Contract {
		a.Do, a.Model = ActionRecordDelete, "inv"
		if a.Mapping == nil {
			a.Match = []string{"sku"}
			a.Mapping = map[string]string{"sku": "{{payload.sku}}"}
		}
		return Contract{Key: "stock.deleted", Version: 1, Actions: []Action{a}}
	}
	ctx := context.Background()
	d := &DynamicExecutor{store: newFakeDyn()}
	if err := d.CheckContract(ctx, mk(Action{})); err != nil {
		t.Fatalf("soft on a soft-delete model must register: %v", err)
	}
	// match must be a declared column.
	if err := d.CheckContract(ctx, mk(Action{Match: []string{"nope"}, Mapping: map[string]string{"nope": "{{payload.sku}}"}})); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("undeclared match column: %v", err)
	}
	// soft on a model without deleted_at fails at startup.
	g := newFakeDyn()
	g.noSoftDelete = true
	if err := (&DynamicExecutor{store: g}).CheckContract(ctx, mk(Action{})); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("soft unsupported: %v", err)
	}
	// hard needs the deleter and the model opt-in.
	if err := d.CheckContract(ctx, mk(Action{Mode: DeleteHard})); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("hard disabled: %v", err)
	}
	dh := &DynamicExecutor{store: newFakeDyn(), HardDeleter: &fakeHard{}, HardDeleteModels: map[string]bool{"inv": true}}
	if err := dh.CheckContract(ctx, mk(Action{Mode: DeleteHard})); err != nil {
		t.Fatalf("hard enabled: %v", err)
	}
	// max_rows above the executor cap.
	dc := &DynamicExecutor{store: newFakeDyn(), MaxDeleteRows: 5}
	if err := dc.CheckContract(ctx, mk(Action{MaxRows: 6})); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("cap: %v", err)
	}
}

// -------------------------------------------------------- compilation ---

const delSchema = `{"type":"object","required":["id","kind"],"additionalProperties":false,
 "properties":{"id":{"type":"string"},"kind":{"type":"string"},"sku":{"type":"string"}}}`

func delContract(mut func(*Action)) Contract {
	a := Action{Do: ActionRecordDelete, Model: "customers", Match: []string{"external_ref"},
		Mapping: map[string]string{"external_ref": "{{payload.id}}"}}
	if mut != nil {
		mut(&a)
	}
	return Contract{Key: "crm-cliente-borrado", Version: 1, JSONSchema: json.RawMessage(delSchema),
		Idempotency: "{id}:{kind}", Auth: ProfileHMACCanonicalV1, Actions: []Action{a}}
}

func TestDeleteContractValidation(t *testing.T) {
	if err := NewRegistry().Register(delContract(nil)); err != nil {
		t.Fatalf("valid: %v", err)
	}
	if err := NewRegistry().Register(delContract(func(a *Action) { a.Mode, a.MaxRows = DeleteHard, 100 })); err != nil {
		t.Fatalf("valid hard/max_rows: %v", err)
	}
	bad := map[string]func(*Action){
		"no match":             func(a *Action) { a.Match = nil },
		"mapping not in match": func(a *Action) { a.Mapping["name"] = "{{payload.sku}}" },
		"match not in mapping": func(a *Action) { a.Match = []string{"external_ref", "other"} },
		"unknown mode":         func(a *Action) { a.Mode = "purge" },
		"negative max_rows":    func(a *Action) { a.MaxRows = -1 },
		"max_rows too high":    func(a *Action) { a.MaxRows = 101 },
		"defaults":             func(a *Action) { a.Defaults = map[string]any{"status": "x"} },
		"idempotency_field":    func(a *Action) { a.IdempotencyField = "idem" },
		"no model":             func(a *Action) { a.Model = "" },
		"no mapping":           func(a *Action) { a.Mapping = nil },
		"bad reference":        func(a *Action) { a.Mapping["external_ref"] = "{{payload.nope}}" },
	}
	for name, mut := range bad {
		if err := NewRegistry().Register(delContract(mut)); !errors.Is(err, ErrInvalidContract) {
			t.Errorf("%s must be rejected, got %v", name, err)
		}
	}
	// mode / max_rows belong to record.delete only.
	for _, do := range []string{ActionRecordUpdate, ActionRecordUpsert} {
		c := delContract(func(a *Action) { a.Do, a.Mode = do, DeleteSoft })
		if err := NewRegistry().Register(c); !errors.Is(err, ErrInvalidContract) {
			t.Errorf("mode on %s must be rejected, got %v", do, err)
		}
		c = delContract(func(a *Action) { a.Do, a.MaxRows = do, 2 })
		if err := NewRegistry().Register(c); !errors.Is(err, ErrInvalidContract) {
			t.Errorf("max_rows on %s must be rejected, got %v", do, err)
		}
	}
	// a route (no chain) cannot be a delete.
	rc := stockContract()
	rc.Route.Op = OpDelete
	if err := NewRegistry().Register(rc); !errors.Is(err, ErrInvalidContract) {
		t.Errorf("route delete must be rejected, got %v", err)
	}
}

// ------------------------------------------------------------- chain ---

func delBody(id, kind string) string {
	return `{"id":"` + id + `","kind":"` + kind + `"}`
}

func seedCustomer(ce *chainEnv, org uuid.UUID, ref string) string {
	id := uuid.NewString()
	ce.exec.rows[org] = append(ce.exec.rows[org], map[string]any{"id": id, "external_ref": ref})
	return id
}

func stepAt(t *testing.T, out map[string]any, i int) map[string]any {
	t.Helper()
	raw, _ := out["actions"].([]any)
	if i >= len(raw) {
		t.Fatalf("no step %d in %v", i, out)
	}
	return raw[i].(map[string]any)
}

func TestDeleteChainAppliedThenNotFoundIsStillApplied(t *testing.T) {
	c := delContract(nil)
	ce := newChainEnv(t, c)
	id := seedCustomer(ce, ce.orgA, "c-1")
	other := seedCustomer(ce, ce.orgA, "c-2")

	code, out := ce.send(t, ce.orgA, c.Key, delBody("c-1", "deleted"), nil)
	if code != 200 || out["status"] != "applied" || out["record_id"] != id {
		t.Fatalf("%d %v", code, out)
	}
	if s := stepAt(t, out, 0); s["status"] != "done" || s["affected"] != float64(1) || s["not_found"] != nil {
		t.Fatalf("step %v", s)
	}
	if rows := ce.exec.rows[ce.orgA]; len(rows) != 1 || rows[0]["id"] != other {
		t.Fatalf("rows %v", rows)
	}

	// a second, distinct event for the same record: nothing left, still applied.
	code, out = ce.send(t, ce.orgA, c.Key, delBody("c-1", "deleted-again"), nil)
	if code != 200 || out["status"] != "applied" {
		t.Fatalf("%d %v", code, out)
	}
	if s := stepAt(t, out, 0); s["status"] != "done" || s["not_found"] != true || s["affected"] != nil {
		t.Fatalf("step %v", s)
	}
	if len(ce.exec.rows[ce.orgA]) != 1 {
		t.Fatal("the other row must stay")
	}

	// the exact same event is a replay, not a second delete.
	code, out = ce.send(t, ce.orgA, c.Key, delBody("c-1", "deleted"), nil)
	if code != 200 || out["status"] != "replayed" {
		t.Fatalf("%d %v", code, out)
	}
}

func TestDeleteChainMultiMatchFailsWithoutDeleting(t *testing.T) {
	c := delContract(nil)
	ce := newChainEnv(t, c)
	seedCustomer(ce, ce.orgA, "dup")
	seedCustomer(ce, ce.orgA, "dup")
	code, out := ce.send(t, ce.orgA, c.Key, delBody("dup", "deleted"), nil)
	if code != 409 || errCode(out) != "too_many_matches" {
		t.Fatalf("%d %v", code, out)
	}
	if len(ce.exec.rows[ce.orgA]) != 2 {
		t.Fatal("nothing may be deleted")
	}
	// raising max_rows in the contract lets it through
	c2 := delContract(func(a *Action) { a.MaxRows = 2 })
	c2.Key = "crm-cliente-borrado-2"
	ce2 := newChainEnv(t, c2)
	seedCustomer(ce2, ce2.orgA, "dup")
	seedCustomer(ce2, ce2.orgA, "dup")
	code, out = ce2.send(t, ce2.orgA, c2.Key, delBody("dup", "deleted"), nil)
	if code != 200 || stepAt(t, out, 0)["affected"] != float64(2) || len(ce2.exec.rows[ce2.orgA]) != 0 {
		t.Fatalf("%d %v", code, out)
	}
}

func TestDeleteChainOtherOrganizationUntouched(t *testing.T) {
	c := delContract(nil)
	ce := newChainEnv(t, c)
	seedCustomer(ce, ce.orgB, "c-1")
	code, out := ce.send(t, ce.orgA, c.Key, delBody("c-1", "deleted"), nil)
	if code != 200 || stepAt(t, out, 0)["not_found"] != true {
		t.Fatalf("%d %v", code, out)
	}
	if len(ce.exec.rows[ce.orgB]) != 1 {
		t.Fatal("orgB row must survive an orgA event")
	}
}

func TestDeleteChainRespectsWhen(t *testing.T) {
	c := delContract(func(a *Action) {
		a.When = &Condition{Path: "payload.kind", Eq: json.RawMessage(`"deleted"`)}
	})
	ce := newChainEnv(t, c)
	seedCustomer(ce, ce.orgA, "c-1")

	code, out := ce.send(t, ce.orgA, c.Key, delBody("c-1", "updated"), nil)
	if code != 200 || stepAt(t, out, 0)["status"] != "skipped" || len(ce.exec.rows[ce.orgA]) != 1 {
		t.Fatalf("a false when must skip the delete: %d %v", code, out)
	}
	code, out = ce.send(t, ce.orgA, c.Key, delBody("c-1", "deleted"), nil)
	if code != 200 || stepAt(t, out, 0)["status"] != "done" || len(ce.exec.rows[ce.orgA]) != 0 {
		t.Fatalf("%d %v", code, out)
	}
}

func TestDeleteChainFeedsLaterStepsAndPassesModeToExecutor(t *testing.T) {
	c := delContract(func(a *Action) { a.Mode, a.MaxRows = DeleteSoft, 1 })
	c.Actions = append(c.Actions, Action{Do: ActionEventEmit, Name: "customers.customer.retired",
		When:    &Condition{Path: "result.affected", Eq: json.RawMessage(`1`)},
		Payload: map[string]any{"id": "{{result.id}}"}})
	ce := newChainEnv(t, c)
	id := seedCustomer(ce, ce.orgA, "c-1")
	code, out := ce.send(t, ce.orgA, c.Key, delBody("c-1", "deleted"), nil)
	if code != 200 || strings.Join(steps(t, out), ",") != "done,done" {
		t.Fatalf("%d %v", code, out)
	}
	if len(ce.em.reqs) != 1 || ce.em.reqs[0].Payload["id"] != id {
		t.Fatalf("emits %v", ce.em.reqs)
	}
	if op := ce.exec.last; op.Route.Op != OpDelete || op.DeleteMode != DeleteSoft || op.MaxRows != 1 || op.Route.Model != "customers" {
		t.Fatalf("op %+v", op)
	}
	// a not-found delete reports affected 0, so the emit is skipped.
	code, out = ce.send(t, ce.orgA, c.Key, delBody("c-1", "again"), nil)
	if code != 200 || strings.Join(steps(t, out), ",") != "done,skipped" || len(ce.em.reqs) != 1 {
		t.Fatalf("%d %v", code, out)
	}
}
