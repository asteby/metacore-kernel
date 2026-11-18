package inbound

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/metadata"
	"github.com/asteby/metacore-kernel/modelbase"
)

// Atomicity and multi-replica behaviour of the order guard (parte B): the
// stale decision and the write happen under one row lock held by one
// transaction (dynamic.UpdateGuarded / DeleteSealed), never in a read that an
// other replica can overtake. Runs against the real dynamic.Service on SQLite
// (CGO) and on Postgres (TEST_POSTGRES_DSN, the CI job "postgres").

// ---- fakeDyn: the guarded writes, under the fake's own mutex ----------------

func (f *fakeDyn) UpdateGuarded(_ context.Context, _ string, u modelbase.AuthUser, id uuid.UUID, in map[string]any, g *dynamic.WriteGuard) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rows {
		if r["id"] == id.String() && r["organization_id"] == u.GetOrganizationID().String() && r["deleted_at"] == nil {
			stale, err := g.Stale(r[g.Field])
			if err != nil {
				return nil, err
			}
			if stale {
				return nil, dynamic.ErrStaleWrite
			}
			atomic.AddInt32(&f.updates, 1)
			for k, v := range in {
				r[k] = v
			}
			return r, nil
		}
	}
	return nil, dynamic.ErrRecordNotFound
}

// DeleteSealed seals and tombstones in one step and counts ONE delete (the real
// service publishes only "deleted": the seal is not an "updated").
func (f *fakeDyn) DeleteSealed(_ context.Context, _ string, u modelbase.AuthUser, id uuid.UUID, seal map[string]any, g *dynamic.WriteGuard) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rows {
		if r["id"] == id.String() && r["organization_id"] == u.GetOrganizationID().String() && r["deleted_at"] == nil {
			if g != nil {
				stale, err := g.Stale(r[g.Field])
				if err != nil {
					return err
				}
				if stale {
					return dynamic.ErrStaleWrite
				}
			}
			atomic.AddInt32(&f.deletes, 1)
			for k, v := range seal {
				r[k] = v
			}
			r["deleted_at"] = "now"
			return nil
		}
	}
	return dynamic.ErrRecordNotFound
}

// ---- a real model --------------------------------------------------------

const atomModel = "inbound_guard_atoms"

// atomRow spells the audit columns out (see stageOrder) and carries a nullable
// instant to guard on.
type atomRow struct {
	ID             uuid.UUID      `json:"id" gorm:"type:uuid;primaryKey"`
	OrganizationID uuid.UUID      `json:"organization_id" gorm:"type:uuid;index"`
	CreatedByID    *uuid.UUID     `json:"created_by_id,omitempty" gorm:"type:uuid"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	DeletedAt      gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
	Sku            string         `json:"sku" gorm:"size:64"`
	Warehouse      string         `json:"warehouse" gorm:"size:32"`
	Qty            int            `json:"qty"`
	SourceAt       *time.Time     `json:"source_at"`
}

func (a *atomRow) BeforeCreate(*gorm.DB) error {
	if a.ID == uuid.Nil {
		a.ID = uuid.New()
	}
	return nil
}

func (atomRow) TableName() string { return atomModel }
func (atomRow) DefineTable() modelbase.TableMetadata {
	return modelbase.TableMetadata{
		Title: "Atoms",
		Columns: []modelbase.ColumnDef{
			{Key: "sku", Label: "SKU"}, {Key: "warehouse", Label: "Warehouse"}, {Key: "qty", Label: "Qty"},
			{Key: "source_at", Label: "Source at"},
		},
		SearchColumns: []string{"sku"},
	}
}
func (atomRow) DefineModal() modelbase.ModalMetadata { return modelbase.ModalMetadata{Title: "Atoms"} }

const (
	atomSQLite = `CREATE TABLE inbound_guard_atoms (id TEXT PRIMARY KEY, organization_id TEXT, created_by_id TEXT,
		created_at DATETIME, updated_at DATETIME, deleted_at DATETIME, sku TEXT, warehouse TEXT, qty INTEGER, source_at DATETIME)`
	atomPG = `CREATE TABLE inbound_guard_atoms (id uuid PRIMARY KEY, organization_id uuid, created_by_id uuid,
		created_at timestamptz, updated_at timestamptz, deleted_at timestamptz, sku text, warehouse text, qty integer, source_at timestamptz)`
	// The unique index every multi-replica upsert needs (docs/inbound-events.md).
	atomUnique = `CREATE UNIQUE INDEX inbound_guard_atoms_key ON inbound_guard_atoms (organization_id, sku, warehouse)`
)

func eachAtomDB(t *testing.T, fn func(t *testing.T, db *gorm.DB)) {
	t.Helper()
	run := func(t *testing.T, db *gorm.DB, ddl string) {
		t.Helper()
		if err := db.Exec(ddl).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Exec(atomUnique).Error; err != nil {
			t.Fatal(err)
		}
		fn(t, db)
	}
	// SQLite is capped to one connection (the pool serializes the transactions);
	// Postgres is the one that proves the row lock across real connections.
	t.Run("SQLite", func(t *testing.T) { run(t, openSQLite(t), atomSQLite) })
	t.Run("Postgres", func(t *testing.T) { run(t, postgresDB(t), atomPG) })
}

// atomBus records the canonical events the service publishes.
type atomBus struct {
	mu     sync.Mutex
	events []string
}

func (b *atomBus) Publish(_ context.Context, _, event string, _ uuid.UUID, _ any) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, event)
	return nil
}

func (b *atomBus) count(suffix string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, e := range b.events {
		if strings.HasSuffix(e, suffix) {
			n++
		}
	}
	return n
}

// atomStore wraps the real service and runs a hook right before the guarded
// write of the executor under test: the moment another replica gets in.
type atomStore struct {
	dynStore
	beforeUpdate func()
	beforeSeal   func()
}

func (a *atomStore) UpdateGuarded(ctx context.Context, model string, user modelbase.AuthUser, id uuid.UUID, in map[string]any, g *dynamic.WriteGuard) (map[string]any, error) {
	if f := a.beforeUpdate; f != nil {
		a.beforeUpdate = nil
		f()
	}
	return a.dynStore.UpdateGuarded(ctx, model, user, id, in, g)
}

func (a *atomStore) DeleteSealed(ctx context.Context, model string, user modelbase.AuthUser, id uuid.UUID, seal map[string]any, g *dynamic.WriteGuard) error {
	if f := a.beforeSeal; f != nil {
		a.beforeSeal = nil
		f()
	}
	return a.dynStore.DeleteSealed(ctx, model, user, id, seal, g)
}

// atomEnv is one database with two executors over one service: two replicas
// (separate in-process locks, one database).
type atomEnv struct {
	t      *testing.T
	db     *gorm.DB
	svc    *dynamic.Service
	bus    *atomBus
	tomb   TombstoneFinder
	ex1    *DynamicExecutor
	ex2    *DynamicExecutor
	org    uuid.UUID
	caller modelbase.AuthUser
}

func newAtomEnv(t *testing.T, db *gorm.DB) *atomEnv {
	t.Helper()
	modelbase.Register(atomModel, func() modelbase.ModelDefiner { return &atomRow{} })
	bus := &atomBus{}
	svc := dynamic.New(dynamic.Config{
		AllowUnconfiguredPermissions: true, DB: db, Metadata: metadata.New(metadata.Config{CacheTTL: -1}),
		Bus: bus, DisableEventOutbox: true,
	})
	tomb := GormTombstones{DB: db, Table: func(string) string { return atomModel }}
	org := uuid.New()
	return &atomEnv{t: t, db: db, svc: svc, bus: bus, tomb: tomb, org: org, caller: dynamic.NewSystemCaller(org),
		ex1: &DynamicExecutor{Service: svc, Tombstones: tomb},
		ex2: &DynamicExecutor{Service: svc, Tombstones: tomb}}
}

var atomBase = time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)

// atomAt is the instant of the i-th event (one minute apart), RFC 3339.
func atomAt(i int) string { return atomBase.Add(time.Duration(i) * time.Minute).Format(time.RFC3339) }

func (e *atomEnv) seed(sku, wh string, at string) uuid.UUID {
	e.t.Helper()
	row, err := e.svc.Create(context.Background(), atomModel, e.caller,
		map[string]any{"sku": sku, "warehouse": wh, "qty": 0, "source_at": at})
	if err != nil {
		e.t.Fatal(err)
	}
	id, err := uuid.Parse(row["id"].(string))
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

// row reads the row even when it is soft-deleted.
func (e *atomEnv) row(id uuid.UUID) atomRow {
	e.t.Helper()
	var r atomRow
	if err := e.db.Unscoped().Where("id = ?", id).Take(&r).Error; err != nil {
		e.t.Fatal(err)
	}
	return r
}

func (e *atomEnv) live(sku string) int {
	e.t.Helper()
	var n int64
	if err := e.db.Model(&atomRow{}).Where("organization_id = ? AND sku = ?", e.org, sku).Count(&n).Error; err != nil {
		e.t.Fatal(err)
	}
	return int(n)
}

func atomSealed(r atomRow, i int) bool {
	return r.SourceAt != nil && r.SourceAt.Equal(atomBase.Add(time.Duration(i)*time.Minute))
}

// upsertOp is the i-th event of sku: guard gt on source_at, match sku.
func (e *atomEnv) upsertOp(sku string, qty int64, i int) Operation {
	at := atomAt(i)
	return Operation{Org: e.org, Route: Route{Model: atomModel, Op: OpUpsert, Match: []string{"sku"}},
		Values: map[string]any{"sku": sku, "qty": qty, "source_at": at},
		Guard:  &OperationGuard{Field: "source_at", Op: GuardGT, Value: at, Match: []string{"sku"}}}
}

// deleteOp is the i-th event as a delete keyed by (sku, warehouse): another
// match than upsertOp's.
func (e *atomEnv) deleteOp(sku, wh string, i int) Operation {
	at := atomAt(i)
	m := []string{"sku", "warehouse"}
	return Operation{Org: e.org, Route: Route{Model: atomModel, Op: OpDelete, Match: m},
		Values: map[string]any{"sku": sku, "warehouse": wh},
		Guard:  &OperationGuard{Field: "source_at", Op: GuardGT, Value: at, Match: m}}
}

// ---- H3: two replicas, same key, different guard values --------------------

func TestGuardedUpsertConcurrentReplicasNewestWins(t *testing.T) {
	eachAtomDB(t, func(t *testing.T, db *gorm.DB) {
		e := newAtomEnv(t, db)
		ctx := context.Background()
		id := e.seed("S1", "", atomAt(0))
		const n = 16
		var wg sync.WaitGroup
		errs := make(chan error, n)
		start := make(chan struct{})
		for i := 1; i <= n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				ex := e.ex1
				if i%2 == 0 {
					ex = e.ex2
				}
				if _, err := ex.Execute(ctx, e.upsertOp("S1", int64(i), i)); err != nil {
					errs <- fmt.Errorf("event %d: %w", i, err)
				}
			}(i)
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Error(err)
		}
		if r := e.row(id); !atomSealed(r, n) || r.Qty != n {
			t.Fatalf("the newest event must win whatever the order: qty=%d source_at=%v", r.Qty, r.SourceAt)
		}
		if got := e.live("S1"); got != 1 {
			t.Fatalf("live rows = %d", got)
		}
	})
}

// A key that does not exist yet: the creates race, the unique index throws the
// losers out and they fall back to the guarded update.
func TestGuardedUpsertCreateRaceConverges(t *testing.T) {
	eachAtomDB(t, func(t *testing.T, db *gorm.DB) {
		e := newAtomEnv(t, db)
		ctx := context.Background()
		const n = 12
		var wg sync.WaitGroup
		errs := make(chan error, n)
		start := make(chan struct{})
		for i := 1; i <= n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				ex := e.ex1
				if i%2 == 0 {
					ex = e.ex2
				}
				// A concrete warehouse: the unique index does not see NULLs.
				op := e.upsertOp("N1", int64(i), i)
				op.Values["warehouse"] = "W9"
				if _, err := ex.Execute(ctx, op); err != nil {
					errs <- fmt.Errorf("event %d: %w", i, err)
				}
			}(i)
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Error(err)
		}
		var r atomRow
		if err := db.Where("organization_id = ? AND sku = ?", e.org, "N1").Take(&r).Error; err != nil {
			t.Fatal(err)
		}
		if !atomSealed(r, n) || r.Qty != n || e.live("N1") != 1 {
			t.Fatalf("converged to qty=%d source_at=%v live=%d", r.Qty, r.SourceAt, e.live("N1"))
		}
	})
}

// The exact interleaving of the review: A reads (t=1), B writes (t=2), A writes.
// The old read-compare-write let A overwrite B; the guarded write judges A on
// the locked row.
func TestGuardedUpsertStaleReadDoesNotOverwriteNewer(t *testing.T) {
	eachAtomDB(t, func(t *testing.T, db *gorm.DB) {
		e := newAtomEnv(t, db)
		ctx := context.Background()
		id := e.seed("S1", "", atomAt(0))
		updatedMid := -1
		hook := &atomStore{dynStore: e.svc}
		hook.beforeUpdate = func() {
			if _, err := e.ex2.Execute(ctx, e.upsertOp("S1", 22, 2)); err != nil {
				t.Error(err)
			}
			updatedMid = e.bus.count(".updated")
		}
		slow := &DynamicExecutor{store: hook, Tombstones: e.tomb}
		out, err := slow.Execute(ctx, e.upsertOp("S1", 11, 1))
		if err != nil || !out.Stale {
			t.Fatalf("the older event must come back stale: %+v %v", out, err)
		}
		if r := e.row(id); r.Qty != 22 || !atomSealed(r, 2) {
			t.Fatalf("the newer event was overwritten: qty=%d source_at=%v", r.Qty, r.SourceAt)
		}
		if got := e.bus.count(".updated"); got != updatedMid {
			t.Fatalf("a stale write published an event (%d -> %d)", updatedMid, got)
		}
	})
}

// A row another replica deletes between the find and the write is judged again
// from the top: here nothing blocks the recreate.
func TestGuardedUpsertRowVanishesIsReJudged(t *testing.T) {
	eachAtomDB(t, func(t *testing.T, db *gorm.DB) {
		e := newAtomEnv(t, db)
		ctx := context.Background()
		id := e.seed("S1", "W1", atomAt(0))
		hook := &atomStore{dynStore: e.svc}
		hook.beforeUpdate = func() {
			if err := e.svc.Delete(ctx, atomModel, e.caller, id); err != nil {
				t.Error(err)
			}
		}
		slow := &DynamicExecutor{store: hook, Tombstones: e.tomb}
		out, err := slow.Execute(ctx, e.upsertOp("S1", 5, 1))
		if err != nil || !out.Created || out.Stale {
			t.Fatalf("the event must recreate the row: %+v %v", out, err)
		}
		if got := e.live("S1"); got != 1 {
			t.Fatalf("live rows = %d", got)
		}
	})
}

// ---- H3b: a delete keyed differently from the upsert -----------------------

func TestGuardedDeleteAndUpsertWithDifferentMatchDoNotClobber(t *testing.T) {
	eachAtomDB(t, func(t *testing.T, db *gorm.DB) {
		e := newAtomEnv(t, db)
		ctx := context.Background()
		id := e.seed("S1", "W1", atomAt(0))

		// A delete (t=3, keyed by sku+warehouse) selected the row; before it
		// seals, an upsert keyed by sku only (t=5) lands: the delete is stale.
		hook := &atomStore{dynStore: e.svc}
		hook.beforeSeal = func() {
			if _, err := e.ex2.Execute(ctx, e.upsertOp("S1", 55, 5)); err != nil {
				t.Error(err)
			}
		}
		slow := &DynamicExecutor{store: hook, Tombstones: e.tomb}
		out, err := slow.Execute(ctx, e.deleteOp("S1", "W1", 3))
		if err != nil || !out.Stale || out.Affected != 0 {
			t.Fatalf("the older delete must not delete: %+v %v", out, err)
		}
		if r := e.row(id); r.DeletedAt.Valid || r.Qty != 55 || !atomSealed(r, 5) {
			t.Fatalf("the newer upsert was lost: %+v", r)
		}

		// A newer delete (t=7) goes through: sealed with its date, one
		// `deleted` and NO `updated` (the seal is not an event).
		upd, del := e.bus.count(".updated"), e.bus.count(".deleted")
		out, err = e.ex1.Execute(ctx, e.deleteOp("S1", "W1", 7))
		if err != nil || out.Affected != 1 {
			t.Fatalf("the newer delete: %+v %v", out, err)
		}
		if r := e.row(id); !r.DeletedAt.Valid || !atomSealed(r, 7) {
			t.Fatalf("tombstone not sealed with the delete's date: %+v", r)
		}
		if e.bus.count(".updated")-upd != 0 || e.bus.count(".deleted")-del != 1 {
			t.Fatalf("events: updated +%d deleted +%d, want 0 and 1", e.bus.count(".updated")-upd, e.bus.count(".deleted")-del)
		}

		// And an older upsert (t=6) cannot resurrect it.
		out, err = e.ex2.Execute(ctx, e.upsertOp("S1", 66, 6))
		if err != nil || !out.Stale || e.live("S1") != 0 {
			t.Fatalf("a late upsert resurrected the row: %+v %v live=%d", out, err, e.live("S1"))
		}
	})
}

// ---- H4: seal + delete is one transaction ---------------------------------

func TestSealedDeleteIsAtomicAndRetryConvergesWithGt(t *testing.T) {
	eachAtomDB(t, func(t *testing.T, db *gorm.DB) {
		e := newAtomEnv(t, db)
		ctx := context.Background()
		id := e.seed("S1", "W1", atomAt(1))

		var fail atomic.Bool
		fail.Store(true)
		if err := db.Callback().Delete().Before("gorm:delete").Register("atom:fail_delete", func(tx *gorm.DB) {
			if fail.Load() {
				_ = tx.AddError(errors.New("simulated crash between the seal and the delete"))
			}
		}); err != nil {
			t.Fatal(err)
		}

		upd, del := e.bus.count(".updated"), e.bus.count(".deleted")
		if _, err := e.ex1.Execute(ctx, e.deleteOp("S1", "W1", 3)); err == nil {
			t.Fatal("the simulated failure must surface")
		}
		// No intermediate state: the row is live and still carries the OLD value.
		if r := e.row(id); r.DeletedAt.Valid || !atomSealed(r, 1) {
			t.Fatalf("a live row was left sealed (or deleted): %+v", r)
		}
		if e.bus.count(".updated") != upd || e.bus.count(".deleted") != del {
			t.Fatal("a rolled-back delete published events")
		}

		// The retry (same event, op gt) converges instead of stale_event.
		fail.Store(false)
		out, err := e.ex1.Execute(ctx, e.deleteOp("S1", "W1", 3))
		if err != nil || out.Stale || out.Affected != 1 {
			t.Fatalf("retry: %+v %v", out, err)
		}
		if r := e.row(id); !r.DeletedAt.Valid || !atomSealed(r, 3) {
			t.Fatalf("retry did not delete and seal: %+v", r)
		}
		if e.bus.count(".updated")-upd != 0 || e.bus.count(".deleted")-del != 1 {
			t.Fatalf("events after the retry: updated +%d deleted +%d, want 0 and 1",
				e.bus.count(".updated")-upd, e.bus.count(".deleted")-del)
		}
	})
}

// ---- dynamic.Service contract ----------------------------------------------

func TestServiceGuardedWritesContract(t *testing.T) {
	eachAtomDB(t, func(t *testing.T, db *gorm.DB) {
		e := newAtomEnv(t, db)
		ctx := context.Background()
		id := e.seed("S1", "W1", atomAt(1))
		never := &dynamic.WriteGuard{Field: "source_at", Stale: func(any) (bool, error) { return false, nil }}
		always := &dynamic.WriteGuard{Field: "source_at", Stale: func(any) (bool, error) { return true, nil }}
		boom := errors.New("unreadable")
		failing := &dynamic.WriteGuard{Field: "source_at", Stale: func(any) (bool, error) { return false, boom }}

		if _, err := e.svc.UpdateGuarded(ctx, atomModel, e.caller, id, map[string]any{"qty": 1}, nil); !errors.Is(err, dynamic.ErrInvalidInput) {
			t.Fatalf("nil guard: %v", err)
		}
		if _, err := e.svc.UpdateGuarded(ctx, atomModel, e.caller, uuid.New(), map[string]any{"qty": 1}, never); !errors.Is(err, dynamic.ErrRecordNotFound) {
			t.Fatalf("missing row: %v", err)
		}
		if _, err := e.svc.UpdateGuarded(ctx, atomModel, e.caller, id, map[string]any{"qty": 1}, always); !errors.Is(err, dynamic.ErrStaleWrite) {
			t.Fatalf("stale: %v", err)
		}
		if _, err := e.svc.UpdateGuarded(ctx, atomModel, e.caller, id, map[string]any{"qty": 1}, failing); !errors.Is(err, boom) {
			t.Fatalf("guard error: %v", err)
		}
		if _, err := e.svc.UpdateGuarded(ctx, atomModel, e.caller, id, map[string]any{"qty": 1}, &dynamic.WriteGuard{Field: "nope", Stale: never.Stale}); !errors.Is(err, dynamic.ErrInvalidInput) {
			t.Fatalf("unknown guard column: %v", err)
		}
		if r := e.row(id); r.Qty != 0 {
			t.Fatalf("a refused write changed the row: %+v", r)
		}
		if e.bus.count(".updated") != 0 {
			t.Fatal("a refused write published an event")
		}
		if _, err := e.svc.UpdateGuarded(ctx, atomModel, e.caller, id, map[string]any{"qty": 9}, never); err != nil {
			t.Fatal(err)
		}
		if r := e.row(id); r.Qty != 9 || e.bus.count(".updated") != 1 {
			t.Fatalf("applied write: %+v updated=%d", r, e.bus.count(".updated"))
		}

		seal := map[string]any{"source_at": atomAt(4)}
		if err := e.svc.DeleteSealed(ctx, atomModel, e.caller, id, nil, never); !errors.Is(err, dynamic.ErrInvalidInput) {
			t.Fatalf("empty seal: %v", err)
		}
		if err := e.svc.DeleteSealed(ctx, atomModel, e.caller, id, seal, always); !errors.Is(err, dynamic.ErrStaleWrite) {
			t.Fatalf("stale seal: %v", err)
		}
		if r := e.row(id); r.DeletedAt.Valid || !atomSealed(r, 1) || e.bus.count(".deleted") != 0 {
			t.Fatalf("a stale seal wrote: %+v", r)
		}
		if err := e.svc.DeleteSealed(ctx, atomModel, e.caller, id, seal, nil); err != nil {
			t.Fatal(err)
		}
		if r := e.row(id); !r.DeletedAt.Valid || !atomSealed(r, 4) || e.bus.count(".deleted") != 1 {
			t.Fatalf("sealed delete: %+v", r)
		}
		if err := e.svc.DeleteSealed(ctx, atomModel, e.caller, id, seal, nil); !errors.Is(err, dynamic.ErrRecordNotFound) {
			t.Fatalf("second delete: %v", err)
		}
	})
}

// ---- the fake honours the same contract (the fast, driver-free tests) ------

func TestGuardedFakeStoreStaleSealKeepsRow(t *testing.T) {
	f := guardFake()
	org := uuid.New()
	id := gSeed(f, org, "S1", "source_at", gT3)
	ex := &DynamicExecutor{store: f}
	out, err := ex.Execute(context.Background(), guardDeleteOp(org, gT2))
	if err != nil || !out.Stale {
		t.Fatalf("%+v %v", out, err)
	}
	if deleted(f, id) || gRow(f, id)["source_at"] != gT3 {
		t.Fatalf("stale delete wrote: %v", gRow(f, id))
	}
}
