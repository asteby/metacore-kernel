package inbound

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Order guard (guard {field, path, op}): a stale or retried event must not
// overwrite a newer change nor resurrect a soft-deleted row.

const (
	gT1 = "2026-03-01T10:00:00Z"
	gT2 = "2026-03-01T11:00:00Z"
	gT3 = "2026-03-01T12:00:00Z"
	gT4 = "2026-03-01T13:00:00Z"
	gT5 = "2026-03-01T15:00:00Z"
	gT6 = "2026-03-01T16:00:00Z"
)

const guardSchema = `{"type":"object","required":["event_id","sku"],"additionalProperties":false,
 "properties":{"event_id":{"type":"string"},"sku":{"type":"string"},"quantity":{"type":"integer"},
 "source_at":{"type":"string"},"version":{"type":"integer"}}}`

// Tombstones makes the fake a TombstoneFinder: soft-deleted rows of the org
// that match, filtered by branch and scope BEFORE the limit (like the SQL
// one), the most recently deleted (last in the slice) first.
func (f *fakeDyn) Tombstones(_ context.Context, q TombstoneQuery) ([]map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	for i := len(f.rows) - 1; i >= 0 && len(out) < q.Limit; i-- {
		r := f.rows[i]
		if r["organization_id"] != q.Org.String() || r["deleted_at"] == nil {
			continue
		}
		ok := true
		for k, v := range q.Where {
			if !valuesEqual(r[k], v) {
				ok = false
			}
		}
		if q.BranchColumn != "" && q.Branch != "" && !valuesEqual(r[q.BranchColumn], q.Branch) {
			ok = false
		}
		if q.Scope != nil && !scopeAllows(r, q.Scope) {
			ok = false
		}
		if ok {
			cp := map[string]any{}
			for k, v := range r {
				cp[k] = v
			}
			out = append(out, cp)
		}
	}
	return out, nil
}

// noTomb is a store that cannot read tombstones.
type noTomb struct{ dynStore }

func guardFake() *fakeDyn {
	return &fakeDyn{cols: []string{"sku", "quantity", "source_at", "version", "idem", "branch_id"}}
}

func guardContract(key, do, op, field string) Contract {
	a := Action{Do: do, Model: "inv", Match: []string{"sku"},
		Mapping: map[string]string{"sku": "{{payload.sku}}"},
		Guard:   &Guard{Field: field, Path: "payload." + field, Op: op}}
	if do != ActionRecordDelete {
		a.Mapping["quantity"] = "{{payload.quantity}}"
		a.Mapping[field] = "{{payload." + field + "}}"
	}
	if do == ActionRecordCreate {
		a.IdempotencyField = "idem"
	}
	return Contract{Key: key, Version: 1, JSONSchema: json.RawMessage(guardSchema), Idempotency: "{event_id}",
		Auth: ProfileHMACCanonicalV1, Actions: []Action{a}}
}

func guardEnv(t *testing.T, f *fakeDyn, cs ...Contract) *env {
	t.Helper()
	reg := NewRegistry()
	for _, c := range cs {
		if err := reg.Register(c); err != nil {
			t.Fatal(err)
		}
	}
	return newEnv(t, func(cfg *Config) { cfg.Contracts, cfg.Executor = reg, &DynamicExecutor{store: f} })
}

func gBody(id, sku, field, val string, qty int) string {
	b := fmt.Sprintf(`{"event_id":%q,"sku":%q,"quantity":%d`, id, sku, qty)
	if val != "" {
		if field == "version" {
			b += `,"version":` + val
		} else {
			b += `,"` + field + `":"` + val + `"`
		}
	}
	return b + "}"
}

func gSeed(f *fakeDyn, org uuid.UUID, sku string, field string, val any) string {
	id := uuid.NewString()
	f.rows = append(f.rows, map[string]any{"id": id, "organization_id": org.String(), "sku": sku, "quantity": int64(5), field: val})
	return id
}

func gRow(f *fakeDyn, id string) map[string]any {
	for _, r := range f.rows {
		if r["id"] == id {
			return r
		}
	}
	return nil
}

func assertStale(t *testing.T, code int, out map[string]any) {
	t.Helper()
	s := stepAt(t, out, 0)
	if code != 200 || out["status"] != "applied" || s["status"] != "skipped" || s["error_code"] != "stale_event" {
		t.Fatalf("want applied receipt with a stale_event step: %d %v", code, out)
	}
}

func assertDone(t *testing.T, code int, out map[string]any) {
	t.Helper()
	if s := stepAt(t, out, 0); code != 200 || out["status"] != "applied" || s["status"] != "done" {
		t.Fatalf("want a done step: %d %v", code, out)
	}
}

func TestGuardStaleUpdateDoesNotOverwrite(t *testing.T) {
	f := guardFake()
	c := guardContract("inv.up", ActionRecordUpsert, GuardGTE, "source_at")
	e := guardEnv(t, f, c)
	id := gSeed(f, e.orgA, "S1", "source_at", gT3)

	code, out := e.send(t, e.orgA, c.Key, gBody("e1", "S1", "source_at", gT1, 1), nil)
	assertStale(t, code, out)
	if r := gRow(f, id); r["quantity"] != int64(5) || r["source_at"] != gT3 || atomic.LoadInt32(&f.updates) != 0 {
		t.Fatalf("a stale event wrote: %v", r)
	}

	code, out = e.send(t, e.orgA, c.Key, gBody("e2", "S1", "source_at", gT4, 7), nil)
	assertDone(t, code, out)
	if r := gRow(f, id); r["quantity"] != int64(7) || r["source_at"] != gT4 {
		t.Fatalf("a newer event must apply: %v", r)
	}

	// The value is absent: the guard is opt-in by data and the event applies.
	code, out = e.send(t, e.orgA, c.Key, gBody("e3", "S1", "source_at", "", 8), nil)
	assertDone(t, code, out)
	// The value does not parse: it is a producer bug, never a silent skip.
	code, out = e.send(t, e.orgA, c.Key, gBody("e4", "S1", "source_at", "not-a-date", 9), nil)
	if code != 422 || errCode(out) != CodeGuardValueInvalid {
		t.Fatalf("a present but unparseable value must fail: %d %v", code, out)
	}
	if r := gRow(f, id); r["quantity"] != int64(8) {
		t.Fatalf("absent must apply, unparseable must not: %v", r)
	}
}

func TestGuardIntegerVersionOnChain(t *testing.T) {
	f := guardFake()
	c := guardContract("inv.ver", ActionRecordUpsert, GuardGT, "version")
	e := guardEnv(t, f, c)
	id := gSeed(f, e.orgA, "S1", "version", int64(5))

	for i, v := range []string{"4", "5"} { // older and equal: gt does not apply
		code, out := e.send(t, e.orgA, c.Key, gBody(fmt.Sprintf("o%d", i), "S1", "version", v, 1), nil)
		assertStale(t, code, out)
	}
	code, out := e.send(t, e.orgA, c.Key, gBody("n1", "S1", "version", "6", 2), nil)
	assertDone(t, code, out)
	if r := gRow(f, id); r["quantity"] != int64(2) || fmt.Sprint(r["version"]) != "6" {
		t.Fatalf("row %v", r)
	}
}

func TestGuardEqualGteAppliesGtDoesNot(t *testing.T) {
	run := func(op string) Outcome {
		f := guardFake()
		org := uuid.New()
		gSeed(f, org, "S1", "source_at", gT2)
		out, err := (&DynamicExecutor{store: f}).Execute(context.Background(), Operation{Org: org,
			Route:  Route{Model: "inv", Op: OpUpsert, Match: []string{"sku"}},
			Values: map[string]any{"sku": "S1", "source_at": gT2, "quantity": int64(1)},
			Guard:  &OperationGuard{Field: "source_at", Op: op, Value: gT2, Match: []string{"sku"}}})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if out := run(GuardGTE); out.Stale {
		t.Fatalf("gte with an equal value must apply: %+v", out)
	}
	if out := run(GuardGT); !out.Stale {
		t.Fatalf("gt with an equal value must not apply: %+v", out)
	}
}

func TestGuardTombstoneBlocksResurrection(t *testing.T) {
	f := guardFake()
	up := guardContract("inv.up", ActionRecordUpsert, GuardGTE, "source_at")
	del := guardContract("inv.del", ActionRecordDelete, GuardGTE, "source_at")
	e := guardEnv(t, f, up, del)
	id := gSeed(f, e.orgA, "S1", "source_at", gT1)

	// A newer delete seals the tombstone with its own date.
	code, out := e.send(t, e.orgA, del.Key, gBody("d1", "S1", "source_at", gT3, 0), nil)
	assertDone(t, code, out)
	if !deleted(f, id) || gRow(f, id)["source_at"] != gT3 {
		t.Fatalf("the tombstone must be sealed with the delete date: %v", gRow(f, id))
	}

	// A late update (older than the delete) cannot resurrect the row.
	code, out = e.send(t, e.orgA, up.Key, gBody("u1", "S1", "source_at", gT2, 9), nil)
	assertStale(t, code, out)
	if len(f.rows) != 1 || atomic.LoadInt32(&f.creates) != 0 {
		t.Fatalf("resurrected: %v", f.rows)
	}

	// A newer one is a legitimate recreation.
	code, out = e.send(t, e.orgA, up.Key, gBody("u2", "S1", "source_at", gT4, 9), nil)
	assertDone(t, code, out)
	if len(f.rows) != 2 || atomic.LoadInt32(&f.creates) != 1 {
		t.Fatalf("recreation: %v", f.rows)
	}

	// An old delete cannot remove the recreated (newer) row.
	code, out = e.send(t, e.orgA, del.Key, gBody("d2", "S1", "source_at", gT3, 0), nil)
	assertStale(t, code, out)
	if deleted(f, f.rows[1]["id"].(string)) {
		t.Fatal("an old delete removed the recreated row")
	}
}

func TestGuardDeleteStaleKeepsRowAndNewerSeals(t *testing.T) {
	f := guardFake()
	del := guardContract("inv.del", ActionRecordDelete, GuardGTE, "source_at")
	e := guardEnv(t, f, del)
	id := gSeed(f, e.orgA, "S1", "source_at", gT5)

	code, out := e.send(t, e.orgA, del.Key, gBody("d1", "S1", "source_at", gT3, 0), nil)
	assertStale(t, code, out)
	if deleted(f, id) || atomic.LoadInt32(&f.deletes) != 0 || atomic.LoadInt32(&f.updates) != 0 {
		t.Fatal("a stale delete must not touch the row")
	}

	code, out = e.send(t, e.orgA, del.Key, gBody("d2", "S1", "source_at", gT6, 0), nil)
	assertDone(t, code, out)
	if !deleted(f, id) || gRow(f, id)["source_at"] != gT6 {
		t.Fatalf("delete + seal: %v", gRow(f, id))
	}

	// A delete without the guard value applies (opt-in by data).
	id2 := gSeed(f, e.orgA, "S2", "source_at", gT6)
	code, out = e.send(t, e.orgA, del.Key, gBody("d3", "S2", "source_at", "", 0), nil)
	assertDone(t, code, out)
	if !deleted(f, id2) {
		t.Fatal("an unguarded value must delete")
	}
}

func TestGuardTombstoneOtherOrgDoesNotInterfere(t *testing.T) {
	f := guardFake()
	up := guardContract("inv.up", ActionRecordUpsert, GuardGTE, "source_at")
	e := guardEnv(t, f, up)
	other := gSeed(f, e.orgB, "S1", "source_at", gT6)
	f.rows[0]["deleted_at"] = "x" // org B's S1 is a newer tombstone

	code, out := e.send(t, e.orgA, up.Key, gBody("u1", "S1", "source_at", gT1, 3), nil)
	assertDone(t, code, out)
	if len(f.rows) != 2 || f.rows[1]["organization_id"] != e.orgA.String() || gRow(f, other)["quantity"] != int64(5) {
		t.Fatalf("org A must create its own row: %v", f.rows)
	}
}

func TestGuardCreateSkipsOwnRetryAndHonoursTombstone(t *testing.T) {
	f := guardFake()
	org := uuid.New()
	ex := &DynamicExecutor{store: f}
	idem := org.String() + ":0"
	rowID := uuid.NewString()
	f.rows = append(f.rows, map[string]any{"id": rowID, "organization_id": org.String(), "sku": "S1", "idem": idem, "source_at": gT2})
	op := Operation{Org: org, Route: Route{Model: "inv", Op: OpUpsert, Match: []string{"idem"}},
		Values: map[string]any{"idem": idem, "sku": "S1", "source_at": gT2},
		Guard:  &OperationGuard{Field: "source_at", Op: GuardGT, Value: gT2, Match: []string{"sku"}, Create: true}}
	// The row found by the idempotency column is the event's own earlier
	// attempt: even gt with an equal value must not call it stale.
	if out, err := ex.Execute(context.Background(), op); err != nil || out.Stale || out.RecordID != rowID {
		t.Fatalf("own retry: %+v %v", out, err)
	}
	// With no row of its own, a newer tombstone for the match blocks the create.
	f.rows[0]["deleted_at"] = "x"
	f.rows[0]["idem"] = "other"
	op.Guard.Value = gT1
	if out, err := ex.Execute(context.Background(), op); err != nil || !out.Stale {
		t.Fatalf("tombstone must block the create: %+v %v", out, err)
	}
}

func TestGuardNullNeverBlocksUnreadableFailsClosed(t *testing.T) {
	g := &OperationGuard{Op: GuardGT, Value: int64(5)}
	for name, cur := range map[string]any{"nil": nil, "nilptr": (*int64)(nil)} {
		if b, err := g.blocks(cur); b || err != nil {
			t.Errorf("%s is null: it must not block nor fail: %v %v", name, b, err)
		}
	}
	// Not null but unreadable or of another kind: fail closed.
	for name, cur := range map[string]any{"text": "garbage", "instant": time.Now(), "bool": true, "float": 1.5} {
		_, err := g.blocks(cur)
		if k, code := execKind(err); k != KindConflict || code != CodeGuardRowUnreadable {
			t.Errorf("%s must fail closed: %v", name, err)
		}
	}
	n := int64(5)
	for i, c := range []struct {
		cur  any
		want bool
	}{{&n, true}, {int64(9), true}, {int64(4), false}, {"5", true}, {[]byte("4"), false}, {[]byte("6"), true}} {
		if b, err := g.blocks(c.cur); err != nil || b != c.want {
			t.Fatalf("integer comparison #%d: %v %v", i, b, err)
		}
	}
	ts := &OperationGuard{Op: GuardGTE, Value: gT2}
	for cur, want := range map[string]bool{
		"2026-03-01 11:00:00+00:00": false, "2026-03-01 11:00:00+00": false, "2026-03-01T11:00:00Z": false,
		"2026-03-01 12:00:00+00:00": true, "2026-03-01 10:00:00": false, "2026-03-01T10:59:59Z": false} {
		if b, err := ts.blocks(cur); err != nil || b != want {
			t.Fatalf("database timestamp %q: blocks=%v err=%v", cur, b, err)
		}
	}
}

func TestGuardCheckContract(t *testing.T) {
	ctx := context.Background()
	f := guardFake()
	d := &DynamicExecutor{store: f}
	mk := func(do string, mut func(*Action)) Contract {
		c := guardContract("k", do, GuardGTE, "source_at")
		a := c.Actions[0]
		if mut != nil {
			mut(&a)
		}
		c.Actions = []Action{a}
		return c
	}
	for _, do := range []string{ActionRecordUpsert, ActionRecordUpdate, ActionRecordDelete} {
		if err := d.CheckContract(ctx, mk(do, nil)); err != nil {
			t.Fatalf("%s must pass: %v", do, err)
		}
	}
	err := d.CheckContract(ctx, mk(ActionRecordUpsert, func(a *Action) { delete(a.Mapping, "source_at") }))
	if !errors.Is(err, ErrInvalidContract) || !strings.Contains(err.Error(), "guard_field_not_mapped") {
		t.Fatalf("unmapped guard field: %v", err)
	}
	// record.delete holds no mapping for it: only a writable column is required.
	if err := d.CheckContract(ctx, mk(ActionRecordDelete, func(a *Action) { a.Guard.Field = "version" })); err != nil {
		t.Fatalf("delete guard column: %v", err)
	}
	for _, field := range []string{"ghost", "deleted_at"} {
		err := d.CheckContract(ctx, mk(ActionRecordDelete, func(a *Action) { a.Guard.Field = field }))
		if !errors.Is(err, ErrInvalidContract) {
			t.Fatalf("guard column %q must be a writable column: %v", field, err)
		}
	}
	// An upsert on a soft-delete model needs to read tombstones.
	blind := &DynamicExecutor{store: noTomb{f}}
	err = blind.CheckContract(ctx, mk(ActionRecordUpsert, nil))
	if !errors.Is(err, ErrInvalidContract) || !strings.Contains(err.Error(), "guard_tombstones_unavailable") {
		t.Fatalf("no tombstones: %v", err)
	}
	for _, do := range []string{ActionRecordUpdate, ActionRecordDelete} {
		if err := blind.CheckContract(ctx, mk(do, nil)); err != nil {
			t.Fatalf("%s needs no tombstones: %v", do, err)
		}
	}
	if err := (&DynamicExecutor{store: noTomb{f}, Tombstones: f}).CheckContract(ctx, mk(ActionRecordUpsert, nil)); err != nil {
		t.Fatalf("host finder: %v", err)
	}
	if !executorGuards(d) || executorGuards(newFakeExec()) {
		t.Fatal("GuardSupporter")
	}
}

func TestGuardContractValidation(t *testing.T) {
	if err := NewRegistry().Register(guardContract("k", ActionRecordUpsert, GuardGT, "source_at")); err != nil {
		t.Fatalf("valid: %v", err)
	}
	bad := map[string]func(*Action){
		"bad op":          func(a *Action) { a.Guard.Op = "lt" },
		"no op":           func(a *Action) { a.Guard.Op = "" },
		"event path":      func(a *Action) { a.Guard.Path = "event.key" },
		"missing path":    func(a *Action) { a.Guard.Path = "payload.nope" },
		"bare path":       func(a *Action) { a.Guard.Path = "source_at" },
		"system field":    func(a *Action) { a.Guard.Field = "deleted_at" },
		"bad field":       func(a *Action) { a.Guard.Field = "Updated At" },
		"field is match":  func(a *Action) { a.Guard.Field = "sku"; a.Guard.Path = "payload.sku" },
		"field is absent": func(a *Action) { a.Guard.Field = "" },
	}
	for name, mut := range bad {
		c := guardContract("k", ActionRecordUpsert, GuardGTE, "source_at")
		mut(&c.Actions[0])
		if err := NewRegistry().Register(c); !errors.Is(err, ErrInvalidContract) {
			t.Errorf("%s must be rejected, got %v", name, err)
		}
	}
	c := guardContract("k", ActionRecordUpsert, GuardGTE, "source_at")
	c.Actions = []Action{{Do: ActionNotify, Template: "stock.alert", Guard: c.Actions[0].Guard}}
	if err := NewRegistry().Register(c); !errors.Is(err, ErrInvalidContract) {
		t.Errorf("guard on notify must be rejected, got %v", err)
	}
}

// ------------------------------------------------- GormTombstones (SQL) ---

func TestGormTombstones(t *testing.T) {
	for name, open := range map[string]func(*testing.T) *gorm.DB{"SQLite": openSQLite, "Postgres": postgresDB} {
		open := open
		t.Run(name, func(t *testing.T) {
			db := open(t)
			if err := db.Exec(`CREATE TABLE guard_items (id TEXT PRIMARY KEY, organization_id TEXT, sku TEXT,
				source_at TIMESTAMP, deleted_at TIMESTAMP)`).Error; err != nil {
				t.Fatal(err)
			}
			org, other := uuid.New(), uuid.New()
			at := func(s string) time.Time { v, _ := time.Parse(time.RFC3339, s); return v }
			ins := func(o uuid.UUID, sku string, upd time.Time, del any) {
				if err := db.Exec(`INSERT INTO guard_items (id, organization_id, sku, source_at, deleted_at) VALUES (?,?,?,?,?)`,
					uuid.NewString(), o.String(), sku, upd, del).Error; err != nil {
					t.Fatal(err)
				}
			}
			ins(org, "S1", at(gT1), at(gT2))   // older tombstone
			ins(org, "S1", at(gT3), at(gT4))   // most recent tombstone
			ins(org, "S1", at(gT5), nil)       // live row: not a tombstone
			ins(org, "S2", at(gT5), at(gT6))   // another sku
			ins(other, "S1", at(gT6), at(gT6)) // another organization
			g := GormTombstones{DB: db, Table: func(string) string { return "guard_items" }}
			rows, err := g.Tombstones(context.Background(), TombstoneQuery{Model: "inv", Org: org, Where: map[string]any{"sku": "S1"}, Limit: 5})
			if err != nil || len(rows) != 2 {
				t.Fatalf("rows=%v err=%v", rows, err)
			}
			older := &OperationGuard{Op: GuardGTE, Value: gT2}
			same := &OperationGuard{Op: GuardGTE, Value: gT3}
			b1, e1 := older.blocks(rows[0]["source_at"])
			b2, e2 := same.blocks(rows[0]["source_at"])
			if e1 != nil || e2 != nil || !b1 || b2 {
				t.Fatalf("most recent tombstone first: %v %v %v", rows, e1, e2)
			}
		})
	}
}
