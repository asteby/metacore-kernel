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

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/modelbase"
)

// Hardening of the order guard: present-but-invalid values, far-future values,
// tombstone scope, database value reading, ties and the create race.

// guardLooseSchema accepts any JSON type for the guard value, so the handler
// (not the schema) is the one judging it.
const guardLooseSchema = `{"type":"object","required":["event_id","sku"],
 "properties":{"event_id":{"type":"string"},"sku":{"type":"string"},"warehouse":{"type":"string"},
 "quantity":{"type":"integer"},"source_at":{},"version":{}}}`

func execKind(err error) (Kind, string) {
	var ee *ExecError
	if !errors.As(err, &ee) {
		return -1, ""
	}
	return ee.Kind, ee.Code
}

func guardAt(s string) time.Time {
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return v
}

// gTomb seeds a soft-deleted row sealed with seal (branch "" = no branch).
func gTomb(f *fakeDyn, org uuid.UUID, sku, branch string, seal any) {
	r := map[string]any{"id": uuid.NewString(), "organization_id": org.String(), "sku": sku,
		"quantity": int64(1), "source_at": seal, "deleted_at": "x"}
	if branch != "" {
		r["branch_id"] = branch
	}
	f.rows = append(f.rows, r)
}

func guardUpsertOp(org uuid.UUID, val any) Operation {
	return Operation{Org: org, Route: Route{Model: "inv", Op: OpUpsert, Match: []string{"sku"}},
		Values: map[string]any{"sku": "S1", "quantity": int64(1), "source_at": val},
		Guard:  &OperationGuard{Field: "source_at", Op: GuardGTE, Value: val, Match: []string{"sku"}}}
}

func guardBranchOp(org uuid.UUID, branch string, val any) Operation {
	op := guardUpsertOp(org, val)
	op.Branch = branch
	op.Values["branch_id"] = branch
	return op
}

func guardDeleteOp(org uuid.UUID, val any) Operation {
	return Operation{Org: org, Route: Route{Model: "inv", Op: OpDelete, Match: []string{"sku"}},
		Values: map[string]any{"sku": "S1"},
		Guard:  &OperationGuard{Field: "source_at", Op: GuardGTE, Value: val, Match: []string{"sku"}}}
}

// ------------------------------------------------------------- H1: invalid ---

func TestGuardPresentButInvalidValueFails(t *testing.T) {
	f := guardFake()
	up := guardContract("inv.up", ActionRecordUpsert, GuardGTE, "source_at")
	del := guardContract("inv.del", ActionRecordDelete, GuardGTE, "source_at")
	up.JSONSchema, del.JSONSchema = json.RawMessage(guardLooseSchema), json.RawMessage(guardLooseSchema)
	e := guardEnv(t, f, up, del)
	id := gSeed(f, e.orgA, "S1", "source_at", gT3)

	junk := []string{`""`, `"N/A"`, `"2024-01-01"`, `"5"`, `1.5`, `true`, `"2026-03-01 11:00:00"`, `"  "`}
	n := 0
	for _, j := range junk {
		for _, c := range []Contract{up, del} {
			n++
			body := fmt.Sprintf(`{"event_id":"j%d","sku":"S1","quantity":1,"source_at":%s}`, n, j)
			code, out := e.send(t, e.orgA, c.Key, body, nil)
			if code != 422 || errCode(out) != CodeGuardValueInvalid {
				t.Fatalf("%s with source_at=%s must fail with %s: %d %v", c.Key, j, CodeGuardValueInvalid, code, out)
			}
		}
	}
	if r := gRow(f, id); r["quantity"] != int64(5) || r["source_at"] != gT3 || deleted(f, id) ||
		atomic.LoadInt32(&f.updates) != 0 || atomic.LoadInt32(&f.deletes) != 0 || atomic.LoadInt32(&f.creates) != 0 {
		t.Fatalf("an invalid guard value wrote something: %v", r)
	}

	// Absent and an explicit null keep applying: the guard is opt-in by data.
	id2 := gSeed(f, e.orgA, "S2", "source_at", gT3)
	code, out := e.send(t, e.orgA, del.Key, `{"event_id":"a1","sku":"S2","quantity":1}`, nil)
	assertDone(t, code, out)
	id3 := gSeed(f, e.orgA, "S3", "source_at", gT3)
	code, out = e.send(t, e.orgA, del.Key, `{"event_id":"a2","sku":"S3","quantity":1,"source_at":null}`, nil)
	assertDone(t, code, out)
	if !deleted(f, id2) || !deleted(f, id3) {
		t.Fatal("absent / null guard value must apply")
	}
}

func TestGuardEvalGuardKinds(t *testing.T) {
	a := &cAction{Action: Action{Do: ActionRecordUpsert, Match: []string{"sku"}, Guard: &Guard{Field: "version", Op: GuardGT}}, guardPath: "v"}
	cases := []struct {
		name    string
		payload any
		nilG    bool
		bad     bool
		want    any
	}{
		{"absent", map[string]any{}, true, false, nil},
		{"null", map[string]any{"v": nil}, true, false, nil},
		{"int64", map[string]any{"v": int64(7)}, false, false, int64(7)},
		{"whole float", map[string]any{"v": float64(7)}, false, false, int64(7)},
		{"json number", map[string]any{"v": json.Number("7")}, false, false, int64(7)},
		{"instant", map[string]any{"v": " " + gT2 + " "}, false, false, gT2},
		{"empty", map[string]any{"v": ""}, false, true, nil},
		{"text", map[string]any{"v": "N/A"}, false, true, nil},
		{"date only", map[string]any{"v": "2024-01-01"}, false, true, nil},
		{"numeric text", map[string]any{"v": "5"}, false, true, nil},
		{"fraction", map[string]any{"v": 1.5}, false, true, nil},
		{"bool", map[string]any{"v": true}, false, true, nil},
		{"object", map[string]any{"v": map[string]any{"a": 1}}, false, true, nil},
	}
	for _, c := range cases {
		g, err := evalGuard(a, c.payload)
		if c.bad {
			if k, code := execKind(err); g != nil || k != KindInvalid || code != CodeGuardValueInvalid {
				t.Errorf("%s: want %s, got %v %v", c.name, CodeGuardValueInvalid, g, err)
			}
			continue
		}
		if err != nil || (g == nil) != c.nilG || (g != nil && g.Value != c.want) {
			t.Errorf("%s: got %+v %v", c.name, g, err)
		}
	}
}

// ------------------------------------------------------------- H2: future ---

func TestGuardFutureValueRejected(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339Nano) }
	run := func(ex *DynamicExecutor, f *fakeDyn, op Operation) (Outcome, error) {
		ex.store, ex.now = f, func() time.Time { return now }
		return ex.Execute(context.Background(), op)
	}
	for _, c := range []struct {
		name   string
		skew   time.Duration
		val    any
		future bool
	}{
		{"year 9999", 0, "9999-01-01T00:00:00Z", true},
		{"default skew: 25h", 0, at(25 * time.Hour), true},
		{"default skew: 23h", 0, at(23 * time.Hour), false},
		{"custom skew: 2h", time.Hour, at(2 * time.Hour), true},
		{"custom skew: 30m", time.Hour, at(30 * time.Minute), false},
		{"bound disabled", -1, "9999-01-01T00:00:00Z", false},
		{"huge integer is not bounded", 0, int64(1) << 50, false},
	} {
		for _, kind := range []string{"upsert", "delete"} {
			f := guardFake()
			org := uuid.New()
			var seed any = gT3
			if _, isInt := c.val.(int64); isInt {
				seed = int64(1)
			}
			id := gSeed(f, org, "S1", "source_at", seed)
			op := guardUpsertOp(org, c.val)
			if kind == "delete" {
				op = guardDeleteOp(org, c.val)
			}
			_, err := run(&DynamicExecutor{GuardMaxSkew: c.skew}, f, op)
			k, code := execKind(err)
			if c.future {
				if k != KindInvalid || code != CodeGuardValueFuture {
					t.Errorf("%s/%s: want %s, got %v", c.name, kind, CodeGuardValueFuture, err)
				}
				if r := gRow(f, id); r["source_at"] != gT3 || deleted(f, id) || atomic.LoadInt32(&f.updates) != 0 {
					t.Errorf("%s/%s: a future event wrote: %v", c.name, kind, r)
				}
			} else if err != nil {
				t.Errorf("%s/%s: must apply: %v", c.name, kind, err)
			}
		}
	}
}

func TestGuardFutureValueThroughTheChain(t *testing.T) {
	f := guardFake()
	c := guardContract("inv.up", ActionRecordUpsert, GuardGTE, "source_at")
	e := guardEnv(t, f, c)
	id := gSeed(f, e.orgA, "S1", "source_at", gT3)
	code, out := e.send(t, e.orgA, c.Key, gBody("f1", "S1", "source_at", "9999-12-31T23:59:59Z", 9), nil)
	if code != 422 || errCode(out) != CodeGuardValueFuture {
		t.Fatalf("a year-9999 event must be refused: %d %v", code, out)
	}
	if r := gRow(f, id); r["source_at"] != gT3 || r["quantity"] != int64(5) {
		t.Fatalf("the row was frozen: %v", r)
	}
	// A legitimate later event still applies.
	code, out = e.send(t, e.orgA, c.Key, gBody("f2", "S1", "source_at", gT4, 9), nil)
	assertDone(t, code, out)
}

// ------------------------------------------- H6/H7: tombstone branch/scope ---

func TestGuardTombstoneOfAnotherBranchDoesNotBlock(t *testing.T) {
	ctx := context.Background()
	org := uuid.New()

	// Only a newer tombstone of branch b1 exists: branch b2 is free to create.
	f := guardFake()
	gTomb(f, org, "S1", "b1", gT6)
	out, err := (&DynamicExecutor{store: f}).Execute(ctx, guardBranchOp(org, "b2", gT3))
	if err != nil || out.Stale || !out.Created {
		t.Fatalf("a tombstone of another branch must not block: %+v %v", out, err)
	}

	// The same branch is blocked, even when a more recent tombstone of
	// another branch sits in front of it.
	f = guardFake()
	gTomb(f, org, "S1", "b1", gT6)
	gTomb(f, org, "S1", "b2", gT1) // most recently deleted, sealed old
	out, err = (&DynamicExecutor{store: f}).Execute(ctx, guardBranchOp(org, "b1", gT3))
	if err != nil || !out.Stale || atomic.LoadInt32(&f.creates) != 0 {
		t.Fatalf("the own-branch tombstone must block: %+v %v", out, err)
	}
}

func TestGuardTombstoneEntityScope(t *testing.T) {
	ctx := context.Background()
	org := uuid.New()
	scope := dynamic.EntityScope{EntityID: "e1", Restricted: true, BranchIDs: []string{"b1"}}
	mk := func(f *fakeDyn) *DynamicExecutor {
		return &DynamicExecutor{store: f, EntityScope: func(context.Context, uuid.UUID, string) (dynamic.EntityScope, bool) {
			return scope, true
		}}
	}
	entityOp := func() Operation {
		op := guardBranchOp(org, "b1", gT3)
		op.Entity = "e1"
		return op
	}

	// A newer tombstone of a branch outside the entity does not block.
	f := guardFake()
	gTomb(f, org, "S1", "b2", gT6)
	out, err := mk(f).Execute(ctx, entityOp())
	if err != nil || out.Stale || !out.Created {
		t.Fatalf("a tombstone outside the entity scope must not block: %+v %v", out, err)
	}

	// The tombstone inside the scope blocks even behind a newer foreign one.
	f = guardFake()
	gTomb(f, org, "S1", "b1", gT6)
	gTomb(f, org, "S1", "b2", gT1)
	out, err = mk(f).Execute(ctx, entityOp())
	if err != nil || !out.Stale {
		t.Fatalf("the in-scope tombstone must block: %+v %v", out, err)
	}
}

func insGuardRow(t *testing.T, db *gorm.DB, table string, org uuid.UUID, sku, branch string, seal, deletedAt time.Time) {
	t.Helper()
	var br any
	if branch != "" {
		br = branch
	}
	var del any
	if !deletedAt.IsZero() {
		del = deletedAt
	}
	if err := db.Exec(`INSERT INTO `+table+` (id, organization_id, sku, branch_id, source_at, deleted_at) VALUES (?,?,?,?,?,?)`,
		uuid.NewString(), org.String(), sku, br, seal, del).Error; err != nil {
		t.Fatal(err)
	}
}

func eachGuardDB(t *testing.T, fn func(t *testing.T, db *gorm.DB)) {
	for name, open := range map[string]func(*testing.T) *gorm.DB{"SQLite": openSQLite, "Postgres": postgresDB} {
		open := open
		t.Run(name, func(t *testing.T) { fn(t, open(t)) })
	}
}

const guardTableDDL = `CREATE TABLE %s (id TEXT PRIMARY KEY, organization_id TEXT, sku TEXT, branch_id TEXT,
	source_at TIMESTAMP, deleted_at TIMESTAMP)`

func newGuardTable(t *testing.T, db *gorm.DB, name string) {
	t.Helper()
	if err := db.Exec(fmt.Sprintf(guardTableDDL, name)).Error; err != nil {
		t.Fatal(err)
	}
}

// 25 newer tombstones of another branch must not push the row's own out of
// the page: the filter runs in SQL before the limit.
func TestGuardTombstoneLimitDoesNotHideOwn(t *testing.T) {
	eachGuardDB(t, func(t *testing.T, db *gorm.DB) {
		newGuardTable(t, db, "guard_items")
		org := uuid.New()
		insGuardRow(t, db, "guard_items", org, "S1", "b1", guardAt(gT6), guardAt(gT2)) // own, sealed late
		for i := 0; i < 25; i++ {
			insGuardRow(t, db, "guard_items", org, "S1", "b2", guardAt(gT1), guardAt(gT3).Add(time.Duration(i)*time.Minute))
		}
		f := guardFake()
		ex := &DynamicExecutor{store: f, Tombstones: GormTombstones{DB: db, Table: func(string) string { return "guard_items" }}}
		out, err := ex.Execute(context.Background(), guardBranchOp(org, "b1", gT3))
		if err != nil || !out.Stale || atomic.LoadInt32(&f.creates) != 0 {
			t.Fatalf("the own tombstone was hidden by the limit: %+v %v", out, err)
		}
		out, err = ex.Execute(context.Background(), guardBranchOp(org, "b3", gT3))
		if err != nil || out.Stale || !out.Created {
			t.Fatalf("a branch without tombstones must create: %+v %v", out, err)
		}
	})
}

// With several tombstones of one row the most recently deleted decides.
func TestGuardMostRecentTombstoneDecides(t *testing.T) {
	eachGuardDB(t, func(t *testing.T, db *gorm.DB) {
		newGuardTable(t, db, "guard_items")
		org := uuid.New()
		insGuardRow(t, db, "guard_items", org, "S1", "", guardAt(gT5), guardAt(gT2)) // older deletion, sealed late
		insGuardRow(t, db, "guard_items", org, "S1", "", guardAt(gT3), guardAt(gT4)) // most recent deletion
		f := guardFake()
		ex := &DynamicExecutor{store: f, Tombstones: GormTombstones{DB: db, Table: func(string) string { return "guard_items" }}}
		out, err := ex.Execute(context.Background(), guardUpsertOp(org, gT4))
		if err != nil || out.Stale || !out.Created {
			t.Fatalf("the most recent tombstone (sealed gT3) lets gT4 recreate: %+v %v", out, err)
		}
		f2 := guardFake()
		ex.store = f2
		out, err = ex.Execute(context.Background(), guardUpsertOp(org, gT2))
		if err != nil || !out.Stale {
			t.Fatalf("gT2 is older than the most recent tombstone: %+v %v", out, err)
		}
	})
}

func TestGormTombstonesBranchAndScopeFilters(t *testing.T) {
	eachGuardDB(t, func(t *testing.T, db *gorm.DB) {
		newGuardTable(t, db, "guard_br")
		org := uuid.New()
		insGuardRow(t, db, "guard_br", org, "S1", "b1", guardAt(gT1), guardAt(gT2))
		insGuardRow(t, db, "guard_br", org, "S1", "b2", guardAt(gT1), guardAt(gT3))
		insGuardRow(t, db, "guard_br", org, "S1", "", guardAt(gT1), guardAt(gT4))
		g := GormTombstones{DB: db, Table: func(string) string { return "guard_br" }}
		count := func(q TombstoneQuery) int {
			t.Helper()
			q.Model, q.Org, q.Where, q.BranchColumn, q.Limit = "inv", org, map[string]any{"sku": "S1"}, "branch_id", 20
			rows, err := g.Tombstones(context.Background(), q)
			if err != nil {
				t.Fatal(err)
			}
			return len(rows)
		}
		for name, c := range map[string]struct {
			q    TombstoneQuery
			want int
		}{
			"no filter":               {TombstoneQuery{}, 3},
			"branch b1":               {TombstoneQuery{Branch: "b1"}, 1},
			"branch b9":               {TombstoneQuery{Branch: "b9"}, 0},
			"unrestricted scope":      {TombstoneQuery{Scope: &dynamic.EntityScope{}}, 3},
			"restricted b1,b2":        {TombstoneQuery{Scope: &dynamic.EntityScope{Restricted: true, BranchIDs: []string{"b1", "b2"}}}, 2},
			"restricted b1 + default": {TombstoneQuery{Scope: &dynamic.EntityScope{Restricted: true, BranchIDs: []string{"b1"}, IncludeUnassigned: true}}, 2},
			"restricted none":         {TombstoneQuery{Scope: &dynamic.EntityScope{Restricted: true}}, 0},
			"restricted default only": {TombstoneQuery{Scope: &dynamic.EntityScope{Restricted: true, IncludeUnassigned: true}}, 1},
			"unresolved":              {TombstoneQuery{Scope: &dynamic.EntityScope{Unresolved: true, BranchIDs: []string{"b1"}}}, 0},
			"unresolved + default":    {TombstoneQuery{Scope: &dynamic.EntityScope{Unresolved: true, IncludeUnassigned: true}}, 1},
			"branch outside scope":    {TombstoneQuery{Branch: "b1", Scope: &dynamic.EntityScope{Restricted: true, BranchIDs: []string{"b2"}}}, 0},
		} {
			if got := count(c.q); got != c.want {
				t.Errorf("%s: want %d rows, got %d", name, c.want, got)
			}
		}
		// Most recently deleted first.
		rows, err := g.Tombstones(context.Background(), TombstoneQuery{Model: "inv", Org: org, Where: map[string]any{"sku": "S1"}, Limit: 20})
		if err != nil || len(rows) != 3 || rows[0]["branch_id"] != nil {
			t.Fatalf("order: %v %v", rows, err)
		}
	})
}

// ---------------------------------------------- H9: columns and identifiers ---

func TestGormTombstonesCustomColumnsAndIdentifiers(t *testing.T) {
	eachGuardDB(t, func(t *testing.T, db *gorm.DB) {
		if err := db.Exec(`CREATE TABLE guard_custom (id TEXT PRIMARY KEY, tenant_id TEXT, sku TEXT,
			removed_at TIMESTAMP, source_at TIMESTAMP)`).Error; err != nil {
			t.Fatal(err)
		}
		org := uuid.New()
		for _, o := range []uuid.UUID{org, uuid.New()} {
			if err := db.Exec(`INSERT INTO guard_custom (id, tenant_id, sku, removed_at, source_at) VALUES (?,?,?,?,?)`,
				uuid.NewString(), o.String(), "S1", guardAt(gT2), guardAt(gT3)).Error; err != nil {
				t.Fatal(err)
			}
		}
		table := func(string) string { return "guard_custom" }
		g := GormTombstones{DB: db, Table: table, OrgColumn: "tenant_id"}
		q := TombstoneQuery{Model: "inv", Org: org, Where: map[string]any{"sku": "S1"}, DeletedAtColumn: "removed_at", Limit: 5}
		rows, err := g.Tombstones(context.Background(), q)
		if err != nil || len(rows) != 1 {
			t.Fatalf("custom organization and deleted_at columns: %v %v", rows, err)
		}

		bad := map[string]func() (GormTombstones, TombstoneQuery){
			"org column": func() (GormTombstones, TombstoneQuery) {
				return GormTombstones{DB: db, Table: table, OrgColumn: "tenant_id; DROP TABLE guard_custom"}, q
			},
			"deleted_at column": func() (GormTombstones, TombstoneQuery) {
				b := q
				b.DeletedAtColumn = "Removed At"
				return g, b
			},
			"match column": func() (GormTombstones, TombstoneQuery) {
				b := q
				b.Where = map[string]any{"sku' OR '1'='1": "x"}
				return g, b
			},
			"table": func() (GormTombstones, TombstoneQuery) {
				return GormTombstones{DB: db, Table: func(string) string { return "guard_custom; DROP TABLE x" }, OrgColumn: "tenant_id"}, q
			},
		}
		for name, mk := range bad {
			gg, qq := mk()
			if _, err := gg.Tombstones(context.Background(), qq); err == nil {
				t.Errorf("an invalid %s must be refused", name)
			}
		}
		if n := 0; db.Raw(`SELECT COUNT(*) FROM guard_custom`).Scan(&n).Error != nil || n != 2 {
			t.Fatalf("the table was damaged: %d", n)
		}
	})
}

// The executor asks the model metadata for the soft-delete column.
type metaDeletedAt struct {
	*fakeDyn
	col string
	got *TombstoneQuery
}

func (m *metaDeletedAt) TableMetadata(ctx context.Context, model string) (*modelbase.TableMetadata, error) {
	md, err := m.fakeDyn.TableMetadata(ctx, model)
	if md != nil && md.Audit != nil {
		md.Audit.DeletedAt = m.col
	}
	return md, err
}

func (m *metaDeletedAt) Tombstones(ctx context.Context, q TombstoneQuery) ([]map[string]any, error) {
	m.got = &q
	return nil, nil
}

func TestGuardTombstoneQueryUsesMetadataColumns(t *testing.T) {
	f := guardFake()
	m := &metaDeletedAt{fakeDyn: f, col: "removed_at"}
	org := uuid.New()
	op := guardBranchOp(org, "b1", gT3)
	if _, err := (&DynamicExecutor{store: m}).Execute(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	if m.got == nil || m.got.DeletedAtColumn != "removed_at" || m.got.BranchColumn != "branch_id" ||
		m.got.Branch != "b1" || m.got.Org != org || m.got.Model != "inv" || m.got.Limit <= 0 {
		t.Fatalf("query: %+v", m.got)
	}
}

// -------------------------------------------- H8/H10: reading database values ---

func TestGuardValuesEqualNormalizes(t *testing.T) {
	u := uuid.New()
	lower, upper := u.String(), strings.ToUpper(u.String())
	sp := &lower
	for name, c := range map[string][2]any{
		"uuid.UUID vs upper text": {u, upper},
		"[16]byte vs upper text":  {[16]byte(u), upper},
		"[]byte vs upper text":    {[]byte(lower), upper},
		"*string vs text":         {sp, upper},
		"upper vs lower":          {upper, lower},
		"bytes vs bytes":          {[]byte("abc"), "abc"},
		"zone +05:00 vs Z":        {"2026-03-01T16:00:00+05:00", guardAt(gT2)},
		"time vs time":            {guardAt(gT2).In(time.FixedZone("x", 3600)), guardAt(gT2)},
		"int vs float":            {int64(3), float64(3)},
	} {
		if !valuesEqual(c[0], c[1]) || !valuesEqual(c[1], c[0]) {
			t.Errorf("%s must be equal", name)
		}
	}
	other := uuid.New()
	for name, c := range map[string][2]any{
		"other uuid":       {[16]byte(other), upper},
		"other bytes":      {[]byte("abd"), "abc"},
		"other instant":    {"2026-03-01T16:00:00Z", guardAt(gT2)},
		"nil vs text":      {nil, "x"},
		"text vs instant":  {"nope", guardAt(gT2)},
		"int vs other int": {int64(3), int64(4)},
	} {
		if valuesEqual(c[0], c[1]) {
			t.Errorf("%s must differ", name)
		}
	}
}

func TestGuardTombstoneMatchesUUIDReadAsBytes(t *testing.T) {
	f := guardFake()
	org := uuid.New()
	u := uuid.New()
	gTomb(f, org, "", "", gT6)
	f.rows[0]["sku"] = [16]byte(u) // what pgx hands back for a uuid column
	op := guardUpsertOp(org, gT3)
	op.Values["sku"] = strings.ToUpper(u.String())
	out, err := (&DynamicExecutor{store: f}).Execute(context.Background(), op)
	if err != nil || !out.Stale || atomic.LoadInt32(&f.creates) != 0 {
		t.Fatalf("a [16]byte uuid tombstone must still block (no resurrection): %+v %v", out, err)
	}
}

// Postgres: the real driver, a uuid match column and an upper-case value.
func TestGuardTombstoneUUIDColumnPostgres(t *testing.T) {
	db := postgresDB(t)
	if err := db.Exec(`CREATE TABLE guard_uuid (id UUID PRIMARY KEY, organization_id TEXT, sku UUID, branch_id TEXT,
		source_at TIMESTAMP, deleted_at TIMESTAMP)`).Error; err != nil {
		t.Fatal(err)
	}
	org, sku := uuid.New(), uuid.New()
	if err := db.Exec(`INSERT INTO guard_uuid (id, organization_id, sku, source_at, deleted_at) VALUES (?,?,?,?,?)`,
		uuid.New(), org.String(), sku, guardAt(gT6), guardAt(gT5)).Error; err != nil {
		t.Fatal(err)
	}
	f := guardFake()
	ex := &DynamicExecutor{store: f, Tombstones: GormTombstones{DB: db, Table: func(string) string { return "guard_uuid" }}}
	op := guardUpsertOp(org, gT3)
	op.Values["sku"] = strings.ToUpper(sku.String())
	op.Guard.Value = gT3
	out, err := ex.Execute(context.Background(), op)
	if err != nil || !out.Stale || atomic.LoadInt32(&f.creates) != 0 {
		t.Fatalf("a uuid column read by pgx must still block: %+v %v", out, err)
	}
}

func TestGuardTimestampsWithZoneAndPrecision(t *testing.T) {
	for _, c := range []struct {
		name, op string
		seal     any
		event    string
		stale    bool
	}{
		{"same second, gte applies", GuardGTE, gT2, gT2, false},
		{"same second, gt rejects", GuardGT, gT2, gT2, true},
		{"+05:00 is the same instant as Z (gt)", GuardGT, gT2, "2026-03-01T16:00:00+05:00", true},
		{"+05:00 is the same instant as Z (gte)", GuardGTE, gT2, "2026-03-01T16:00:00+05:00", false},
		{"+05:00 earlier than Z", GuardGTE, gT2, "2026-03-01T15:59:59+05:00", true},
		{"short offset +00 from the database", GuardGT, "2026-03-01 11:00:00+00", gT2, true},
		{"offset +00:00 from the database", GuardGTE, "2026-03-01 11:00:00+00:00", gT2, false},
		{"zone-less database text is UTC", GuardGT, "2026-03-01 11:00:00", gT2, true},
		{"time.Time in another zone", GuardGT, guardAt(gT2).In(time.FixedZone("x", -5*3600)), gT2, true},
		{"ns event vs postgres-rounded us (gt dedupes)", GuardGT, "2026-03-01T11:00:00.123457Z", "2026-03-01T11:00:00.123456789Z", true},
		{"ns event vs postgres-rounded us (gte applies)", GuardGTE, "2026-03-01T11:00:00.123457Z", "2026-03-01T11:00:00.123456789Z", false},
		{"ns event vs truncated us (gt dedupes)", GuardGT, "2026-03-01T11:00:00.123456Z", "2026-03-01T11:00:00.1234564Z", true},
		{"one microsecond later is newer", GuardGT, "2026-03-01T11:00:00.123456Z", "2026-03-01T11:00:00.123457Z", false},
	} {
		// Against a tombstone (no live row) ...
		f := guardFake()
		org := uuid.New()
		gTomb(f, org, "S1", "", c.seal)
		op := guardUpsertOp(org, c.event)
		op.Guard.Op = c.op
		out, err := (&DynamicExecutor{store: f}).Execute(context.Background(), op)
		if err != nil || out.Stale != c.stale {
			t.Errorf("tombstone / %s: stale=%v err=%v", c.name, out.Stale, err)
		}
		// ... and against a live row.
		f = guardFake()
		gSeed(f, org, "S1", "source_at", c.seal)
		op = guardUpsertOp(org, c.event)
		op.Guard.Op = c.op
		out, err = (&DynamicExecutor{store: f}).Execute(context.Background(), op)
		if err != nil || out.Stale != c.stale {
			t.Errorf("live row / %s: stale=%v err=%v", c.name, out.Stale, err)
		}
	}
}

func TestGuardRowValueUnreadableFailsClosed(t *testing.T) {
	ctx := context.Background()
	for _, bad := range []any{"garbage", int64(5), true, 1.5, []byte("nope")} { // an instant event against each
		// Live row: no write.
		f := guardFake()
		org := uuid.New()
		id := gSeed(f, org, "S1", "source_at", bad)
		_, err := (&DynamicExecutor{store: f}).Execute(ctx, guardUpsertOp(org, gT3))
		if k, code := execKind(err); k != KindConflict || code != CodeGuardRowUnreadable {
			t.Fatalf("live %v: want %s, got %v", bad, CodeGuardRowUnreadable, err)
		}
		if atomic.LoadInt32(&f.updates) != 0 || atomic.LoadInt32(&f.creates) != 0 {
			t.Fatalf("live %v: wrote", bad)
		}
		// Delete: the row stays.
		_, err = (&DynamicExecutor{store: f}).Execute(ctx, guardDeleteOp(org, gT3))
		if k, code := execKind(err); k != KindConflict || code != CodeGuardRowUnreadable {
			t.Fatalf("delete %v: got %v", bad, err)
		}
		if deleted(f, id) || atomic.LoadInt32(&f.deletes) != 0 || atomic.LoadInt32(&f.updates) != 0 {
			t.Fatalf("delete %v: touched the row", bad)
		}
		// Tombstone: no create.
		f = guardFake()
		gTomb(f, org, "S1", "", bad)
		_, err = (&DynamicExecutor{store: f}).Execute(ctx, guardUpsertOp(org, gT3))
		if k, code := execKind(err); k != KindConflict || code != CodeGuardRowUnreadable {
			t.Fatalf("tombstone %v: got %v", bad, err)
		}
		if atomic.LoadInt32(&f.creates) != 0 {
			t.Fatalf("tombstone %v: created", bad)
		}
	}
	// An integer event against an instant column fails closed as well.
	f := guardFake()
	org := uuid.New()
	gSeed(f, org, "S1", "version", gT3)
	op := guardUpsertOp(org, int64(7))
	op.Guard.Field, op.Values["version"] = "version", int64(7)
	if _, err := (&DynamicExecutor{store: f}).Execute(ctx, op); err == nil {
		t.Fatal("an integer event against an instant must fail closed")
	}
	// A NULL stored value is "no value": it applies. So do a tombstone without one.
	f = guardFake()
	id := gSeed(f, org, "S1", "source_at", nil)
	if out, err := (&DynamicExecutor{store: f}).Execute(ctx, guardUpsertOp(org, gT3)); err != nil || out.Stale || out.RecordID != id {
		t.Fatalf("null stored value must apply: %+v %v", out, err)
	}
	f = guardFake()
	gTomb(f, org, "S1", "", nil)
	if out, err := (&DynamicExecutor{store: f}).Execute(ctx, guardUpsertOp(org, gT3)); err != nil || out.Stale || !out.Created {
		t.Fatalf("a tombstone without a seal must not block: %+v %v", out, err)
	}
}

func TestGuardRowValueUnreadableThroughTheChain(t *testing.T) {
	f := guardFake()
	c := guardContract("inv.up", ActionRecordUpsert, GuardGTE, "source_at")
	e := guardEnv(t, f, c)
	id := gSeed(f, e.orgA, "S1", "source_at", "garbage")
	code, out := e.send(t, e.orgA, c.Key, gBody("u1", "S1", "source_at", gT3, 9), nil)
	if code != 409 || errCode(out) != CodeGuardRowUnreadable {
		t.Fatalf("want 409 %s: %d %v", CodeGuardRowUnreadable, code, out)
	}
	if r := gRow(f, id); r["quantity"] != int64(5) {
		t.Fatalf("wrote: %v", r)
	}
}

// ------------------------------------------- H5: the seal needs "update" too ---

func TestGuardedSoftDeleteNeedsUpdatePermission(t *testing.T) {
	ctx := context.Background()
	mk := func(allow func(action string) bool, asked *[]string) (*fakeDyn, *DynamicExecutor, uuid.UUID, string) {
		f := guardFake()
		org := uuid.New()
		id := gSeed(f, org, "S1", "source_at", gT1)
		return f, &DynamicExecutor{store: f, Authorize: func(_ context.Context, _ uuid.UUID, _, action string) bool {
			*asked = append(*asked, action)
			return allow(action)
		}}, org, id
	}
	var asked []string

	// delete granted, update not: the guarded delete is refused before any effect.
	f, ex, org, id := mk(func(a string) bool { return a == "delete" }, &asked)
	_, err := ex.Execute(ctx, guardDeleteOp(org, gT3))
	if k, code := execKind(err); k != KindForbidden || code != "forbidden" {
		t.Fatalf("want forbidden, got %v", err)
	}
	if deleted(f, id) || atomic.LoadInt32(&f.updates) != 0 || atomic.LoadInt32(&f.deletes) != 0 {
		t.Fatal("a refused guarded delete wrote")
	}
	if strings.Join(asked, ",") != "delete,update" {
		t.Fatalf("asked %v", asked)
	}

	// Without a guard value the delete needs only "delete".
	asked = nil
	f, ex, org, id = mk(func(a string) bool { return a == "delete" }, &asked)
	op := guardDeleteOp(org, gT3)
	op.Guard = nil
	if _, err := ex.Execute(ctx, op); err != nil || !deleted(f, id) {
		t.Fatalf("unguarded delete: %v", err)
	}
	if strings.Join(asked, ",") != "delete" {
		t.Fatalf("asked %v", asked)
	}

	// Both granted: the guarded delete seals and deletes.
	asked = nil
	f, ex, org, id = mk(func(string) bool { return true }, &asked)
	if out, err := ex.Execute(ctx, guardDeleteOp(org, gT3)); err != nil || out.Affected != 1 {
		t.Fatalf("both granted: %+v %v", out, err)
	}
	if !deleted(f, id) || gRow(f, id)["source_at"] != gT3 {
		t.Fatalf("seal + delete: %v", gRow(f, id))
	}
}

// ------------------------------------- H11: a delete whose match differs ---

func TestGuardDeleteMatchDifferentFromUpsert(t *testing.T) {
	f := &fakeDyn{cols: []string{"sku", "warehouse", "quantity", "source_at", "version", "idem", "branch_id"}}
	up := guardContract("inv.up", ActionRecordUpsert, GuardGTE, "source_at")
	del := guardContract("inv.del", ActionRecordDelete, GuardGTE, "source_at")
	del.Actions[0].Match = []string{"sku", "warehouse"}
	del.Actions[0].Mapping["warehouse"] = "{{payload.warehouse}}"
	up.JSONSchema, del.JSONSchema = json.RawMessage(guardLooseSchema), json.RawMessage(guardLooseSchema)
	e := guardEnv(t, f, up, del)
	w2 := gSeed(f, e.orgA, "S1", "source_at", gT1)
	f.rows[0]["warehouse"] = "W2"
	w1 := gSeed(f, e.orgA, "S1", "source_at", gT1)
	f.rows[1]["warehouse"] = "W1"

	// The delete keys by (sku, warehouse): only W1 goes, sealed with its date.
	code, out := e.send(t, e.orgA, del.Key, `{"event_id":"d1","sku":"S1","warehouse":"W1","source_at":"`+gT3+`"}`, nil)
	assertDone(t, code, out)
	if !deleted(f, w1) || deleted(f, w2) || gRow(f, w1)["source_at"] != gT3 {
		t.Fatalf("delete by two columns: %v", f.rows)
	}
	// The other warehouse is a different row: an equal value with gte applies.
	code, out = e.send(t, e.orgA, del.Key, `{"event_id":"d2","sku":"S1","warehouse":"W2","source_at":"`+gT1+`"}`, nil)
	assertDone(t, code, out)
	if !deleted(f, w2) {
		t.Fatal("gte with an equal value must delete")
	}
	// An upsert keyed by sku only finds both tombstones: a late event stays stale,
	// a newer one recreates.
	code, out = e.send(t, e.orgA, up.Key, `{"event_id":"u1","sku":"S1","quantity":1,"source_at":"`+gT2+`"}`, nil)
	assertStale(t, code, out)
	code, out = e.send(t, e.orgA, up.Key, `{"event_id":"u2","sku":"S1","quantity":1,"source_at":"`+gT4+`"}`, nil)
	assertDone(t, code, out)
}

// -------------------------------------------- H12: the create race fallback ---

// raceStore makes the first Create lose a unique-index race: onLose runs (the
// winner and whatever else another replica wrote) and the create fails.
type raceStore struct {
	*fakeDyn
	onLose func(f *fakeDyn)
	fired  bool
}

func (r *raceStore) Create(ctx context.Context, model string, u modelbase.AuthUser, in map[string]any) (map[string]any, error) {
	if !r.fired {
		r.fired = true
		r.fakeDyn.mu.Lock()
		r.onLose(r.fakeDyn)
		r.fakeDyn.mu.Unlock()
		return nil, errors.New("duplicate key value violates unique constraint")
	}
	return r.fakeDyn.Create(ctx, model, u, in)
}

func TestGuardCreateRaceFallbackGoesThroughGuardAndTombstone(t *testing.T) {
	ctx := context.Background()
	winner := func(org uuid.UUID, seal any) map[string]any {
		return map[string]any{"id": uuid.NewString(), "organization_id": org.String(), "sku": "S1", "quantity": int64(5), "source_at": seal}
	}
	for _, c := range []struct {
		name  string
		lose  func(f *fakeDyn, org uuid.UUID)
		stale bool
	}{
		{"winner is newer than the event", func(f *fakeDyn, org uuid.UUID) { f.rows = append(f.rows, winner(org, gT6)) }, true},
		{"winner is older: the event updates it", func(f *fakeDyn, org uuid.UUID) { f.rows = append(f.rows, winner(org, gT1)) }, false},
		{"a newer tombstone appeared in the race", func(f *fakeDyn, org uuid.UUID) {
			f.rows = append(f.rows, winner(org, nil))
			gTomb(f, org, "S1", "", gT6)
		}, true},
	} {
		f := guardFake()
		org := uuid.New()
		rs := &raceStore{fakeDyn: f, onLose: func(f *fakeDyn) { c.lose(f, org) }}
		out, err := (&DynamicExecutor{store: rs}).Execute(ctx, guardUpsertOp(org, gT3))
		if err != nil || out.Stale != c.stale {
			t.Errorf("%s: stale=%v err=%v", c.name, out.Stale, err)
			continue
		}
		updated := atomic.LoadInt32(&f.updates) == 1
		if c.stale && (updated || f.rows[0]["quantity"] != int64(5)) {
			t.Errorf("%s: the stale event overwrote the winner: %v", c.name, f.rows)
		}
		if !c.stale && (!updated || f.rows[0]["quantity"] != int64(1) || f.rows[0]["source_at"] != gT3) {
			t.Errorf("%s: the event must update the older winner: %v", c.name, f.rows)
		}
	}
}
