package inbound

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/modelbase"
)

// Hardening of the dry run after the adversarial review of #626: deny by
// default, no `from` for a producer, a read-only store, a gate against kernels
// that do not know dry_run, an unambiguous shadow key space and an honest
// report of what is not verified.

func hardenMeta() *modelbase.TableMetadata {
	opts := []modelbase.OptionDef{{Value: "new", Label: "New"}, {Value: "paid", Label: "Paid"}}
	return &modelbase.TableMetadata{Columns: []modelbase.ColumnDef{
		{Key: "razon_social", Type: "text"},
		{Key: "contacto", Type: "string"},
		{Key: "ciudad", Type: "text"},
		{Key: "pin", Type: "text"},
		{Key: "api_key", Type: "text"},
		{Key: "zzz", Type: "text"}, // a neutral name is no guarantee
		{Key: "bio", Type: "textarea"},
		{Key: "buzon", Type: "email"},
		{Key: "mystery"}, // no declared type
		{Key: "total", Type: "number"},
		{Key: "active", Type: "boolean"},
		{Key: "due_at", Type: "datetime"},
		{Key: "born_on", Type: "date"},
		{Key: "kind", Type: "select", Options: opts},
		{Key: "parent_ref", Type: "uuid"},
		{Key: "branch_ref", Type: "text", Ref: "branches"},
		{Key: "line_total", Type: "number"},
		{Key: "pin_code", Type: "number"}, // a number, but the name says pin
		{Key: "secret_n", Type: "number", Protected: true},
		{Key: "hidden_n", Type: "number", Hidden: true},
		{Key: "gated_n", Type: "number", ReadPermission: "x.read"},
	}}
}

func TestDryRunDeniesByDefault(t *testing.T) {
	d := &DynamicExecutor{}
	meta := hardenMeta()
	vals := map[string]any{
		"razon_social": "Acme Industrial SA de CV", "contacto": "Juan Perez Lopez", "ciudad": "Monterrey Nuevo Leon",
		"pin": "pin-834721", "api_key": "sk_live_abc123", "zzz": "Calle Falsa 123", "bio": "vive en la calle Sol",
		"buzon": "ana@example.com", "mystery": "dato libre",
		"total": 100, "active": true, "due_at": time.Now(), "born_on": "2020-01-02",
		"kind": "paid", "parent_ref": uuid.NewString(), "branch_ref": uuid.NewString(), "line_total": 3,
		"pin_code": 4321, "secret_n": 1, "hidden_n": 2, "gated_n": 3,
	}
	got := d.dryChanges("m", meta, nil, vals, false, "")
	for _, k := range []string{"razon_social", "contacto", "ciudad", "pin", "api_key", "zzz", "bio", "buzon", "mystery",
		"pin_code", "secret_n", "hidden_n", "gated_n"} {
		if c := got[k]; !c.Redacted || c.To != nil || c.From != nil {
			t.Fatalf("%s must be redacted: %+v", k, c)
		}
	}
	for _, k := range []string{"total", "active", "due_at", "born_on", "kind", "parent_ref", "branch_ref", "line_total"} {
		if c := got[k]; c.Redacted || c.To == nil {
			t.Fatalf("%s is a safe type and must be shown: %+v", k, c)
		}
	}
	raw, _ := json.Marshal(got)
	for _, leak := range []string{"Acme", "Juan", "Monterrey", "pin-834721", "sk_live", "Calle", "ana@example", "dato libre"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("%q leaked: %s", leak, raw)
		}
	}

	// The shape of the VALUE is checked too: free text cannot ride in a safe column.
	bad := d.dryChanges("m", meta, nil, map[string]any{
		"kind": "Ana Perez", "parent_ref": "Ana Perez", "total": "llamame", "active": "si", "due_at": "hola",
	}, false, "")
	for k, c := range bad {
		if !c.Redacted || c.To != nil {
			t.Fatalf("%s: free text in a safe column must be redacted: %+v", k, c)
		}
	}
	if len(bad) != 5 {
		t.Fatalf("every column is reported by name: %+v", bad)
	}
}

func TestDryRunNameTokensAreNotSubstrings(t *testing.T) {
	for name, want := range map[string]bool{
		"line_total": false, "plate_count": false, "template_id": false, "shipping_cost": false, "capacity": false,
		"lat": true, "lat_deg": true, "pin": true, "api_key": true, "razon_social": true, "ciudad": true,
		"cliente_id": true, "customer": true, "city": true, "contacto": true, "colonia": true, "nss": true,
	} {
		if got := personalColumn(name); got != want {
			t.Errorf("personalColumn(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestDryRunAllowAndVeto(t *testing.T) {
	d := &DynamicExecutor{
		DryRunAllow:  func(_, c string) bool { return c == "contacto" || c == "secret_n" || c == "ciudad" },
		DryRunRedact: func(_, c string) bool { return c == "ciudad" },
	}
	got := d.dryChanges("m", hardenMeta(), nil, map[string]any{
		"contacto": "Juan", "secret_n": 7, "ciudad": "Leon", "razon_social": "Acme",
	}, false, "")
	if c := got["contacto"]; c.Redacted || c.To != "Juan" {
		t.Fatalf("the host allowed it: %+v", c)
	}
	for _, k := range []string{"secret_n", "ciudad", "razon_social"} {
		if c := got[k]; !c.Redacted || c.To != nil {
			t.Fatalf("%s: a protected column, a vetoed one and an unlisted one stay hidden: %+v", k, c)
		}
	}

	// from only with reveal, same rule as to; the stage field is shown.
	d = &DynamicExecutor{}
	before := map[string]any{"total": 1, "razon_social": "Old", "zzz": "old"}
	vals := map[string]any{"total": 2, "razon_social": "New", "zzz": "new"}
	got = d.dryChanges("m", hardenMeta(), before, vals, false, "")
	if c := got["total"]; c.From != nil || c.To == nil {
		t.Fatalf("no reveal, no from: %+v", c)
	}
	got = d.dryChanges("m", hardenMeta(), before, vals, true, "")
	if c := got["total"]; c.From == nil || c.To == nil {
		t.Fatalf("reveal shows from of a safe column: %+v", c)
	}
	if c := got["razon_social"]; !c.Redacted || c.From != nil {
		t.Fatalf("reveal never shows a text column: %+v", c)
	}
	if c := got["zzz"]; !c.Redacted {
		t.Fatalf("zzz: %+v", c)
	}
	got = d.dryChanges("m", hardenMeta(), before, vals, true, "zzz")
	if c := got["zzz"]; c.Redacted || c.From != "old" || c.To != "new" {
		t.Fatalf("the stage field is a lifecycle value: %+v", c)
	}
}

// H2: the current value of a row never reaches the producer, the receipt or a replay.
func TestDryRunNeverReturnsFromToTheProducer(t *testing.T) {
	eachDryDB(t, func(t *testing.T, e *dryEnv) {
		route := Contract{Key: "dry.route", Version: 1, JSONSchema: json.RawMessage(drySchema), Idempotency: "{event_id}",
			Auth: ProfileHMACCanonicalV1, DryRun: true,
			Route: Route{Model: dryModel, Op: OpUpsert, Match: []string{"reference"},
				Mapping: map[string]string{"reference": "ref", "total": "total"}}}
		e.setup(dryContract("dry.up", 1, true, upsertAct()), route)
		e.seed("A", "new", 50, atomAt(0))
		ctx := context.Background()

		for _, key := range []string{"dry.up", "dry.route"} {
			for i, id := range []string{"e1", "e1"} { // the second one is the replay
				r := e.send(key, 0, dryBody(id, "A", 100, atomAt(2)))
				if r.code != 200 || r.body["fidelity"] != FidelityPartial {
					t.Fatalf("%s #%d: %d %s", key, i, r.code, r.raw)
				}
				if strings.Contains(r.raw, `"from"`) {
					t.Fatalf("%s #%d: from must never be answered: %s", key, i, r.raw)
				}
				if c := changeOf(t, stepsOf(r.body)[0], "total"); c["to"] != float64(100) {
					t.Fatalf("%s #%d: %v", key, i, c)
				}
				if i == 1 && r.body["status"] != "replayed" {
					t.Fatalf("%s: second answer must be the replay: %s", key, r.raw)
				}
			}
			recs, err := e.h.Receipts(ctx, e.org, ReceiptFilter{Contract: key})
			if err != nil || len(recs) == 0 {
				t.Fatalf("%s receipts: %v %v", key, recs, err)
			}
			for _, rc := range recs {
				for _, st := range rc.Progress {
					for col, ch := range st.Changes {
						if ch.From != nil {
							t.Fatalf("%s: receipt keeps from of %s: %+v", key, col, ch)
						}
					}
				}
			}
		}
		// A receipt written by the first release (with from) is stripped on replay.
		rid := uuid.New()
		old := ActionStep{Index: 0, Do: "record.upsert", Status: StepDone, Would: "upsert-update",
			Changes: map[string]ColumnChange{"total": {From: float64(50), To: float64(100)}}}
		if got := replayActions(Receipt{ID: rid, Shadow: true, Progress: []ActionStep{old}}); got[0].Changes["total"].From != nil {
			t.Fatalf("replay must strip from: %+v", got)
		}
		if old.Changes["total"].From == nil {
			t.Fatal("withoutFrom must not alter the stored step")
		}
	})
}

// H3: from only for the panel, only with a host read gate.
func TestHandlerDryRunFromNeedsAReadGate(t *testing.T) {
	eachDryDB(t, func(t *testing.T, e *dryEnv) {
		route := Contract{Key: "dry.route", Version: 1, JSONSchema: json.RawMessage(drySchema), Idempotency: "{event_id}",
			Auth: ProfileHMACCanonicalV1,
			Route: Route{Model: dryModel, Op: OpUpsert, Match: []string{"reference"},
				Mapping: map[string]string{"reference": "ref", "total": "total", "email": "email"}}}
		e.setup(dryContract("dry.up", 1, false, upsertAct()), route)
		e.seed("A", "new", 50, atomAt(0))
		ctx := context.Background()
		for _, key := range []string{"dry.up", "dry.route"} {
			req := DryRunRequest{Org: e.org, Contract: key, Body: []byte(dryBody("h1", "A", 100, atomAt(2)))}
			try := func(label string) ColumnChange {
				res, err := e.h.DryRun(ctx, req)
				if err != nil || res.Status != StatusWouldApply || res.Fidelity != FidelityPartial {
					t.Fatalf("%s %s: %+v %v", key, label, res, err)
				}
				if em := res.Actions[0].Changes["email"]; !em.Redacted || em.From != nil || em.To != nil {
					t.Fatalf("%s %s: email: %+v", key, label, em)
				}
				return res.Actions[0].Changes["total"]
			}
			e.ex.Authorize = nil
			if c := try("no Authorize"); c.From != nil || c.To == nil {
				t.Fatalf("%s: without Authorize only names and to: %+v", key, c)
			}
			e.ex.Authorize = func(_ context.Context, _ uuid.UUID, _, action string) bool { return action != "read" }
			if c := try("no read"); c.From != nil {
				t.Fatalf("%s: a host that refuses read shows no from: %+v", key, c)
			}
			e.ex.Authorize = func(context.Context, uuid.UUID, string, string) bool { return true }
			if c := try("read"); c.From == nil || c.To == nil {
				t.Fatalf("%s: the panel with a read gate sees from: %+v", key, c)
			}
		}
		e.ex.Authorize = nil
	})
}

// H4: the shadow key space cannot be spelled by a real key.
func TestShadowKeyHashDoesNotCollide(t *testing.T) {
	org := uuid.New()
	oldShadow := func(contract, key string) string { return HashKey(org, contract+"|shadow", key) }
	// The scheme of the first release collided with a real key `shadow|K` ...
	if oldShadow("c", "K") != HashKey(org, "c", "shadow|K") {
		t.Fatal("the premise of the finding no longer holds")
	}
	// ... the current one does not, whatever the real key looks like.
	for _, realKey := range []string{"shadow|K", "K", "shadow\x00K", "shadow", "0:;shadow"} {
		if shadowKeyHash(org, "c", "K") == HashKey(org, "c", realKey) {
			t.Fatalf("real key %q collides with the shadow key", realKey)
		}
	}
	if shadowKeyHash(org, "c", "K") == HashKey(org, "c|shadow", "K") {
		t.Fatal("the old formula must not be reproduced")
	}
	if shadowKeyHash(org, "a", "bc") == shadowKeyHash(org, "ab", "c") || shadowKeyHash(org, "a|b", "c") == shadowKeyHash(org, "a", "b|c") {
		t.Fatal("the fields must be delimited")
	}
	if shadowKeyHash(org, "c", "K") != shadowKeyHash(org, "c", "K") || shadowKeyHash(org, "c", "K") == shadowKeyHash(uuid.New(), "c", "K") {
		t.Fatal("stable and scoped by organization")
	}
}

// H5: a kernel that does not know dry_run must refuse the contract, not run it for real.
func TestDryRunContractIsRejectedByAnOldKernel(t *testing.T) {
	route := stockContract()
	route.DryRun = true
	for name, c := range map[string]Contract{"route": route, "chain": dryContract("dry.up", 1, true, upsertAct())} {
		t.Run(name, func(t *testing.T) {
			raw, err := json.Marshal(c)
			if err != nil || !strings.Contains(string(raw), `"schema_version":2`) || !strings.Contains(string(raw), contractGate) {
				t.Fatalf("a dry-run contract is written with schema 2 and the gate: %s %v", raw, err)
			}
			// This kernel reads it back as it was.
			reg := NewRegistry()
			if err := reg.LoadJSON(raw); err != nil {
				t.Fatal(err)
			}
			got, ok := reg.get(c.Key, 0)
			if !ok || !got.DryRun || got.OnError != "" || got.Route.Op != c.Route.Op {
				t.Fatalf("round trip: %+v", got)
			}
			// An old kernel ignores the unknown fields (dry_run, schema_version) and
			// validates the rest: the gate makes the contract invalid for it.
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatal(err)
			}
			delete(m, "dry_run")
			delete(m, "schema_version")
			old, _ := json.Marshal(m)
			type legacyContract Contract // no custom decoder: plain struct decoding
			var lc legacyContract
			if err := json.Unmarshal(old, &lc); err != nil {
				t.Fatal(err)
			}
			if lc.DryRun {
				t.Fatal("the simulated old kernel must not see dry_run")
			}
			if _, err := compile(Contract(lc)); err == nil {
				t.Fatal("an old kernel would have registered the shadow contract as a REAL one")
			}
			// A gate cannot be registered as a struct either.
			if _, err := compile(Contract(lc)); !errors.Is(err, ErrInvalidContract) {
				t.Fatalf("%v", err)
			}
		})
	}
	// A newer schema, or a requirement this kernel does not know, is refused too.
	for _, doc := range []string{
		`{"key":"x.y","version":1,"schema_version":3}`,
		`{"key":"x.y","version":1,"on_error":"requires:something-new"}`,
		`{"key":"x.y","version":1,"actions":[{"do":"notify"}],"route":{"op":"requires:something-new"}}`,
	} {
		if err := NewRegistry().LoadJSON([]byte(doc)); !errors.Is(err, ErrInvalidContract) {
			t.Fatalf("%s: %v", doc, err)
		}
	}
}

// H5: flipping dry_run recompiles the contract; the effective mode is observable.
func TestSetDryRunRecompilesTheContract(t *testing.T) {
	eachDryDB(t, func(t *testing.T, e *dryEnv) {
		e.setup(dryContract("dry.up", 1, false, upsertAct()))
		ctx := context.Background()
		if r := e.send("dry.up", 0, dryBody("s1", "Z1", 1, atomAt(1))); r.body["status"] != "applied" || len(e.rows("Z1")) != 1 {
			t.Fatalf("real: %s", r.raw)
		}
		if err := e.h.SetDryRun(ctx, "dry.up", 1, true); err != nil {
			t.Fatal(err)
		}
		if st := e.h.ContractStates(); len(st) != 1 || st[0].Key != "dry.up" || st[0].Version != 1 || !st[0].DryRun {
			t.Fatalf("states: %+v", st)
		}
		r := e.send("dry.up", 0, dryBody("s2", "Z2", 1, atomAt(2)))
		if r.body["status"] != StatusWouldApply || r.hdr.Get(ShadowHeader) != "true" || len(e.rows("Z2")) != 0 {
			t.Fatalf("shadow after the switch: %s rows=%d", r.raw, len(e.rows("Z2")))
		}
		if err := e.h.SetDryRun(ctx, "dry.up", 1, false); err != nil {
			t.Fatal(err)
		}
		if st := e.h.ContractStates(); st[0].DryRun {
			t.Fatalf("states: %+v", st)
		}
		r = e.send("dry.up", 0, dryBody("s3", "Z3", 1, atomAt(3)))
		if r.body["status"] != "applied" || r.hdr.Get(ShadowHeader) != "" || len(e.rows("Z3")) != 1 {
			t.Fatalf("real again: %s", r.raw)
		}
		if err := e.h.SetDryRun(ctx, "dry.up", 0, true); !errors.Is(err, ErrInvalidContract) {
			t.Fatalf("version 0: %v", err)
		}
		var de *DryRunError
		if err := e.h.SetDryRun(ctx, "nope", 1, true); !errors.As(err, &de) || de.Status != 404 {
			t.Fatalf("unknown contract: %v", err)
		}
	})
}

// H6: even code that ignores Operation.DryRun cannot write: the store is read-only.
func TestDryRunStoreIsReadOnlyEvenIfTheCodeIgnoresDryRun(t *testing.T) {
	org := uuid.New()
	f := opsFake()
	id := opsRow(f, org, "S1", "W1", "new")
	user := dynamic.NewSystemCaller(org)
	ctx := context.Background()

	ro := roStore{f}
	if _, err := ro.TableMetadata(ctx, "inv"); err != nil {
		t.Fatalf("reads pass through: %v", err)
	}
	if _, err := ro.Create(ctx, "inv", user, map[string]any{"sku": "X"}); !errors.Is(err, errDryWriteBlocked) {
		t.Fatalf("create: %v", err)
	}
	if _, err := ro.Update(ctx, "inv", user, uuid.New(), nil); !errors.Is(err, errDryWriteBlocked) {
		t.Fatalf("update: %v", err)
	}
	if err := ro.Delete(ctx, "inv", user, uuid.New()); !errors.Is(err, errDryWriteBlocked) {
		t.Fatalf("delete: %v", err)
	}
	if _, err := ro.UpdateGuarded(ctx, "inv", user, uuid.New(), nil, nil); !errors.Is(err, errDryWriteBlocked) {
		t.Fatalf("update guarded: %v", err)
	}
	if err := ro.DeleteSealed(ctx, "inv", user, uuid.New(), nil, nil); !errors.Is(err, errDryWriteBlocked) {
		t.Fatalf("delete sealed: %v", err)
	}
	if _, err := ro.ExecAction(ctx, "inv", user, uuid.New(), "mark_paid", nil); !errors.Is(err, errDryWriteBlocked) {
		t.Fatalf("exec action: %v", err)
	}
	if _, err := ro.Restore(ctx, "inv", user, uuid.New()); !errors.Is(err, errDryWriteBlocked) {
		t.Fatalf("restore: %v", err)
	}

	// A DynamicExecutor whose branches ignore op.DryRun (test seam): the store
	// they reach is the read-only one.
	d := &DynamicExecutor{store: f, ignoreDryRun: true}
	vals := func(sku string) map[string]any { return map[string]any{"sku": sku, "warehouse": "W1", "quantity": 5} }
	route := func(kind Op, sku string) Operation {
		return Operation{Org: org, Route: Route{Model: "inv", Op: kind, Match: []string{"sku"}}, Values: vals(sku)}
	}
	cases := []struct {
		name     string
		op       Operation
		mustFail bool
	}{
		{"create", route(OpCreate, "S9"), true},
		{"upsert-create", route(OpUpsert, "S9"), true},
		{"upsert-update", route(OpUpsert, "S1"), true},
		{"update", route(OpUpdate, "S1"), true},
		{"delete", delOp(org, "", 1, "S1"), true},
		{"transition", transitionOp(org, "paid", "S1"), true},
		{"action", actionOp(org, "mark_paid", "S1", map[string]any{"reference": "R"}), false},
		{"restore", stateOpOf(org, OpRestore, "S1"), false},
	}
	for _, c := range cases {
		op := c.op
		op.DryRun = true
		_, err := d.Execute(ctx, op)
		if c.mustFail && err == nil {
			t.Fatalf("%s: a write got through the read-only store", c.name)
		}
	}
	if f.creates != 0 || f.updates != 0 || f.deletes != 0 || f.execCount() != 0 || len(f.rows) != 1 {
		t.Fatalf("a dry run wrote: creates=%d updates=%d deletes=%d execs=%d rows=%d", f.creates, f.updates, f.deletes, f.execCount(), len(f.rows))
	}
	if r := rowOf(f, id); r["status"] != "new" || r["deleted_at"] != nil {
		t.Fatalf("the row changed: %v", r)
	}
}

// H7: a step that matches on the id of a row a previous step would create is not
// a false failure.
func TestDryRunChainCreateThenUpdateIsNotAFalseFailure(t *testing.T) {
	eachDryDB(t, func(t *testing.T, e *dryEnv) {
		create := Action{Do: ActionRecordCreate, Model: dryModel, IdempotencyField: "idem",
			Mapping: map[string]string{"reference": "{{payload.ref}}", "total": "{{payload.total}}"}}
		upd := Action{Do: ActionRecordUpdate, Model: dryModel, Match: []string{"reference"},
			Mapping: map[string]string{"reference": "{{result.id}}", "total": "{{payload.total}}"}}
		e.setup(dryContract("dry.cu", 1, true, create, upd))
		r := e.send("dry.cu", 0, dryBody("c1", "CU1", 5, atomAt(1)))
		steps := stepsOf(r.body)
		if r.code != 200 || r.body["status"] != StatusWouldApply || len(steps) != 2 {
			t.Fatalf("chain: %d %s", r.code, r.raw)
		}
		if steps[0]["would"] != "create" || steps[1]["would"] != "update" || steps[1]["status"] != StepDone || steps[1]["on_simulated_row"] != true {
			t.Fatalf("steps: %s", r.raw)
		}
		res, err := e.h.DryRun(context.Background(), DryRunRequest{Org: e.org, Contract: "dry.cu", Body: []byte(dryBody("c2", "CU2", 5, atomAt(1)))})
		if err != nil || res.Status != StatusWouldApply || len(res.Actions) != 2 || !res.Actions[1].OnSimulated {
			t.Fatalf("panel: %+v %v", res, err)
		}
		if len(e.rows("CU1")) != 0 || len(e.rows("CU2")) != 0 {
			t.Fatal("a dry run wrote a row")
		}
		if _, err := uuid.Parse(dryPlaceholderID(3)); err != nil {
			t.Fatalf("the placeholder must be a valid uuid: %v", err)
		}
	})
}

// H8: a REAL chain on a receipt store that hands out a zero id still loses its
// lease fence (it aborts) instead of running on unfenced.
type zeroIDStore struct{ *MemoryReceiptStore }

func (s zeroIDStore) Begin(ctx context.Context, r Receipt, lease time.Duration) (Receipt, bool, error) {
	cur, claimed, err := s.MemoryReceiptStore.Begin(ctx, r, lease)
	cur.ID = uuid.Nil
	return cur, claimed, err
}

func TestRealChainWithAZeroIDReceiptStoreAborts(t *testing.T) {
	eachDryDB(t, func(t *testing.T, e *dryEnv) {
		reg := NewRegistry()
		if err := reg.Register(dryContract("dry.up", 1, false, upsertAct())); err != nil {
			t.Fatal(err)
		}
		h, err := New(Config{
			Contracts: reg, Receipts: zeroIDStore{NewMemoryReceiptStore()}, Executor: e.ex, Commands: e.cmds,
			Emitter: e.em, Notifier: e.nt, Automation: e.auto, Logger: quietLogger(),
			Verifiers: map[string]Verifier{ProfileHMACCanonicalV1: VerifierFunc(func(context.Context, VerifyRequest) error { return nil })},
		})
		if err != nil {
			t.Fatal(err)
		}
		e.h = h
		e.srv = httptest.NewServer(h)
		t.Cleanup(e.srv.Close)
		r := e.send("dry.up", 0, dryBody("z1", "ZERO", 5, atomAt(1)))
		if r.code == 200 {
			t.Fatalf("a real run without a usable receipt must abort: %s", r.raw)
		}
		if len(e.rows("ZERO")) != 0 {
			t.Fatal("the chain wrote before the lease check")
		}
	})
}
