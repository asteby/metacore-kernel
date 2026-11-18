package dynamic

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/metadata"
	"github.com/asteby/metacore-kernel/modelbase"
)

// Hardening of the guarded writes (UpdateGuarded / DeleteSealed): one event for
// a sealed delete, tolerant post-commit steps, no second connection while the
// transaction is open, a lock that does not rewrite the row, and a guard
// column that can be compared. SQLite always; Postgres with TEST_POSTGRES_DSN
// (the CI job "postgres").

type gwCat struct {
	modelbase.BaseUUIDModel
	Name string `json:"name" gorm:"size:255"`
}

func (gwCat) TableName() string { return "gw_cats" }
func (gwCat) DefineTable() modelbase.TableMetadata {
	return modelbase.TableMetadata{Title: "Cats"}
}
func (gwCat) DefineModal() modelbase.ModalMetadata { return modelbase.ModalMetadata{Title: "Cat"} }

// gwItem has a unique column (sku) and a ref (category_id): the validation of
// both queries the database; ver is the guarded column; hot carries omitempty.
type gwItem struct {
	modelbase.BaseUUIDModel
	Name       string     `json:"name" gorm:"size:255"`
	Sku        string     `json:"sku" gorm:"size:255"`
	Qty        int        `json:"qty"`
	Ver        int        `json:"ver"`
	Hot        int        `json:"hot,omitempty"`
	CategoryID *uuid.UUID `json:"category_id"`
}

func (gwItem) TableName() string { return "gw_items" }
func (gwItem) DefineTable() modelbase.TableMetadata {
	return modelbase.TableMetadata{Title: "Items"}
}
func (gwItem) DefineModal() modelbase.ModalMetadata { return modelbase.ModalMetadata{Title: "Item"} }

type gwEv struct {
	name string
	ce   *CanonicalEvent
}

type gwBus struct {
	mu  sync.Mutex
	evs []gwEv
}

func (b *gwBus) Publish(_ context.Context, _, event string, _ uuid.UUID, payload any) error {
	ce, _ := payload.(*CanonicalEvent)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.evs = append(b.evs, gwEv{name: event, ce: ce})
	return nil
}

func (b *gwBus) count(suffix string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, e := range b.evs {
		if strings.HasSuffix(e.name, suffix) {
			n++
		}
	}
	return n
}

func (b *gwBus) last(suffix string) *CanonicalEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := len(b.evs) - 1; i >= 0; i-- {
		if strings.HasSuffix(b.evs[i].name, suffix) {
			return b.evs[i].ce
		}
	}
	return nil
}

type gwFx struct {
	svc                 *Service
	db                  *gorm.DB
	bus                 *gwBus
	hooks               *HookRegistry
	items, cats, pivot  string
	user                *fakeUser
	afterUpd, afterDel  atomic.Int32
	beforeDel, beforeUp atomic.Int32
}

var gwNever = &WriteGuard{Field: "ver", Stale: func(any) (bool, error) { return false, nil }}
var gwAlways = &WriteGuard{Field: "ver", Stale: func(any) (bool, error) { return true, nil }}

// eachGW runs fn on SQLite (a pool of one connection) and on Postgres (a pool
// of `pool` connections).
func eachGW(t *testing.T, pool int, fn func(t *testing.T, fx *gwFx)) {
	t.Helper()
	t.Run("SQLite", func(t *testing.T) {
		db := setupTestDB(t)
		sqlDB, err := db.DB()
		if err != nil {
			t.Fatal(err)
		}
		sqlDB.SetMaxOpenConns(1)
		ts := "DATETIME"
		for _, stmt := range gwDDL("gw_cats", "gw_items", "gw_pivot", "TEXT", ts, "INTEGER") {
			mustExec(t, db, stmt)
		}
		fn(t, newGWFixture(t, db, "gw_cats", "gw_items", "gw_pivot"))
	})
	t.Run("Postgres", func(t *testing.T) {
		db, sfx := pgTestDB(t)
		sqlDB, err := db.DB()
		if err != nil {
			t.Fatal(err)
		}
		sqlDB.SetMaxOpenConns(pool)
		cats, items, pivot := "gw_cats_"+sfx, "gw_items_"+sfx, "gw_pivot_"+sfx
		for _, stmt := range gwDDL(cats, items, pivot, "uuid", "timestamptz", "integer") {
			mustExec(t, db, stmt)
		}
		t.Cleanup(func() {
			db.Exec("DROP TABLE IF EXISTS " + items)
			db.Exec("DROP TABLE IF EXISTS " + cats)
			db.Exec("DROP TABLE IF EXISTS " + pivot)
		})
		fn(t, newGWFixture(t, db, cats, items, pivot))
	})
}

// gwDDL is the three tables; the pivot has a NOT NULL column the relation
// never fills, so inserting a join row fails while deleting them works.
func gwDDL(cats, items, pivot, uuidT, tsT, intT string) []string {
	pk := uuidT + " PRIMARY KEY"
	audit := fmt.Sprintf("organization_id %s, created_by_id %s, created_at %s, updated_at %s, deleted_at %s", uuidT, uuidT, tsT, tsT, tsT)
	return []string{
		fmt.Sprintf("CREATE TABLE %s (id %s, %s, name TEXT)", cats, pk, audit),
		fmt.Sprintf("CREATE TABLE %s (id %s, %s, name TEXT, sku TEXT, qty %s, ver %s, hot %s, category_id %s)",
			items, pk, audit, intT, intT, intT, uuidT),
		fmt.Sprintf("CREATE TABLE %s (owner_id %s NOT NULL, target_id %s NOT NULL, extra TEXT NOT NULL)", pivot, uuidT, uuidT),
	}
}

func newGWFixture(t *testing.T, db *gorm.DB, cats, items, pivot string) *gwFx {
	t.Helper()
	modelbase.Register("gw_cats", func() modelbase.ModelDefiner { return &gwCat{} })
	modelbase.Register("gw_items", func() modelbase.ModelDefiner { return &gwItem{} })
	fx := &gwFx{db: db, bus: &gwBus{}, hooks: NewHookRegistry(), items: items, cats: cats, pivot: pivot,
		user: newUser(uuid.New())}
	fx.hooks.RegisterAfterUpdate("gw_items", func(context.Context, HookContext, any) error { fx.afterUpd.Add(1); return nil })
	fx.hooks.RegisterAfterDelete("gw_items", func(context.Context, HookContext, string) error { fx.afterDel.Add(1); return nil })
	fx.hooks.RegisterBeforeDelete("gw_items", func(context.Context, HookContext, string) error { fx.beforeDel.Add(1); return nil })
	fx.hooks.RegisterBeforeUpdate("gw_items", func(context.Context, HookContext, string, map[string]any) error {
		fx.beforeUp.Add(1)
		return nil
	})
	tables := map[string]string{"gw_cats": cats, "gw_items": items}
	cols := []manifest.ColumnDef{
		{Name: "name", Type: "string"},
		{Name: "sku", Type: "string", Unique: true},
		{Name: "category_id", Type: "uuid", Ref: cats},
	}
	fx.svc = New(Config{
		AllowUnconfiguredPermissions: true,
		DB:                           db,
		Metadata:                     metadata.New(metadata.Config{CacheTTL: -1}),
		Bus:                          fx.bus,
		DisableEventOutbox:           true,
		Hooks:                        fx.hooks,
		TableNameResolver: func(_ context.Context, model string) (string, bool) {
			tb, ok := tables[model]
			return tb, ok
		},
		ValidationSchemaResolver: func(_ context.Context, model string) ([]manifest.ColumnDef, bool) {
			if model == "gw_items" {
				return cols, true
			}
			return nil, false
		},
		RelationResolver: func(_ context.Context, model string) ([]Relation, bool) {
			if model == "gw_items" {
				return []Relation{{Name: "tags", Kind: "many_to_many", PivotTable: pivot, OwnerColumn: "owner_id", TargetColumn: "target_id"}}, true
			}
			return nil, false
		},
	})
	return fx
}

func (fx *gwFx) create(t *testing.T, ctx context.Context, model string, in map[string]any) map[string]any {
	t.Helper()
	out, err := fx.svc.Create(ctx, model, fx.user, in)
	if err != nil {
		t.Fatalf("create %s: %v", model, err)
	}
	return out
}

func gwID(t *testing.T, row map[string]any) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(fmt.Sprint(row["id"]))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// row reads the item even when it is soft-deleted.
func (fx *gwFx) row(t *testing.T, id uuid.UUID) gwItem {
	t.Helper()
	var r gwItem
	if err := fx.db.Unscoped().Table(fx.items).Where("id = ?", id).Take(&r).Error; err != nil {
		t.Fatal(err)
	}
	return r
}

// ---- R1: a sealed delete publishes only "deleted" ---------------------------

func TestDeleteSealedPublishesOnlyDeleted(t *testing.T) {
	eachGW(t, 2, func(t *testing.T, fx *gwFx) {
		ctx := context.Background()
		id := gwID(t, fx.create(t, ctx, "gw_items", map[string]any{"name": "n", "sku": "A"}))
		upd, del := fx.bus.count(".updated"), fx.bus.count(".deleted")

		if err := fx.svc.DeleteSealed(ctx, "gw_items", fx.user, id, map[string]any{"ver": 7}, gwNever); err != nil {
			t.Fatal(err)
		}
		if got := fx.bus.count(".updated") - upd; got != 0 {
			t.Fatalf("the seal published %d updated events, want none", got)
		}
		if got := fx.bus.count(".deleted") - del; got != 1 {
			t.Fatalf("deleted events: %d, want exactly one", got)
		}
		ev := fx.bus.last(".deleted")
		if ev == nil || ev.After != nil || ev.Before["ver"] != float64(7) {
			t.Fatalf("deleted event must carry the sealed row as before and no after: %+v", ev)
		}
		if fx.afterUpd.Load() != 0 || fx.afterDel.Load() != 1 {
			t.Fatalf("hooks: after-update %d (want 0), after-delete %d (want 1)", fx.afterUpd.Load(), fx.afterDel.Load())
		}
		if fx.beforeUp.Load() != 1 {
			t.Fatalf("the before-update hook of the seal must run inside the transaction, ran %d", fx.beforeUp.Load())
		}
		if r := fx.row(t, id); !r.DeletedAt.Valid || r.Ver != 7 {
			t.Fatalf("tombstone: %+v", r)
		}
		// A plain UpdateGuarded still publishes its "updated".
		id2 := gwID(t, fx.create(t, ctx, "gw_items", map[string]any{"name": "m", "sku": "B"}))
		upd = fx.bus.count(".updated")
		if _, err := fx.svc.UpdateGuarded(ctx, "gw_items", fx.user, id2, map[string]any{"qty": 2}, gwNever); err != nil {
			t.Fatal(err)
		}
		if fx.bus.count(".updated")-upd != 1 {
			t.Fatal("UpdateGuarded must publish its updated event")
		}
	})
}

// ---- R2: a failing post-commit step does not stop deleted -------------------

func TestDeleteSealedToleratesPostCommitFailure(t *testing.T) {
	eachGW(t, 2, func(t *testing.T, fx *gwFx) {
		ctx := context.Background()
		id := gwID(t, fx.create(t, ctx, "gw_items", map[string]any{"name": "n", "sku": "A"}))
		del := fx.bus.count(".deleted")

		var buf bytes.Buffer
		log.SetOutput(&buf)
		defer log.SetOutput(os.Stderr)

		// The relation is synced after the commit and its INSERT fails (the
		// pivot has a NOT NULL column nobody fills): the row is already deleted.
		seal := map[string]any{"ver": 8, "tags": []any{uuid.NewString()}}
		if err := fx.svc.DeleteSealed(ctx, "gw_items", fx.user, id, seal, gwNever); err != nil {
			t.Fatalf("a failing post-commit step must not fail the delete: %v", err)
		}
		if !strings.Contains(buf.String(), "pivot sync after commit") {
			t.Fatalf("the failing step did not run (nothing logged): %q", buf.String())
		}
		if r := fx.row(t, id); !r.DeletedAt.Valid {
			t.Fatalf("row not deleted: %+v", r)
		}
		if fx.bus.count(".deleted")-del != 1 || fx.afterDel.Load() != 1 {
			t.Fatalf("deleted events %d, after-delete hooks %d: both must happen", fx.bus.count(".deleted")-del, fx.afterDel.Load())
		}
		var n int64
		if err := fx.db.Table(fx.pivot).Where("owner_id = ?", id).Count(&n).Error; err != nil || n != 0 {
			t.Fatalf("pivot rows left: %d %v", n, err)
		}
	})
}

// ---- R3: no second connection while the transaction is open -----------------

func TestGuardedWriteWithOneConnectionDoesNotBlock(t *testing.T) {
	eachGW(t, 1, func(t *testing.T, fx *gwFx) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cat := fx.create(t, ctx, "gw_cats", map[string]any{"name": "c"})
		id := gwID(t, fx.create(t, ctx, "gw_items", map[string]any{"name": "n", "sku": "A"}))

		// Cold schema caches: the introspection happens while the transaction
		// is open, on its connection.
		InvalidateTableColumns()
		softDeleteCols.Delete(fx.cats)
		upd := fx.bus.count(".updated")
		_, err := fx.svc.UpdateGuarded(ctx, "gw_items", fx.user, id,
			map[string]any{"sku": "B", "category_id": cat["id"], "qty": 3}, gwNever)
		if err != nil {
			t.Fatalf("UpdateGuarded on a pool of one connection (unique + ref lookups): %v", err)
		}
		if fx.bus.count(".updated")-upd != 1 {
			t.Fatal("no updated event")
		}
		if r := fx.row(t, id); r.Sku != "B" || r.Qty != 3 || r.CategoryID == nil {
			t.Fatalf("not written: %+v", r)
		}

		InvalidateTableColumns()
		if err := fx.svc.DeleteSealed(ctx, "gw_items", fx.user, id,
			map[string]any{"ver": 3, "sku": "C", "category_id": cat["id"]}, gwNever); err != nil {
			t.Fatalf("DeleteSealed on a pool of one connection: %v", err)
		}
		if r := fx.row(t, id); !r.DeletedAt.Valid || r.Ver != 3 || r.Sku != "C" {
			t.Fatalf("not sealed and deleted: %+v", r)
		}
	})
}

func TestGuardedWritesConcurrentSmallPoolDoNotStarve(t *testing.T) {
	eachGW(t, 3, func(t *testing.T, fx *gwFx) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		cat := fx.create(t, ctx, "gw_cats", map[string]any{"name": "c"})
		const rows, workers = 4, 12
		upIDs := make([]uuid.UUID, rows)
		for i := range upIDs {
			upIDs[i] = gwID(t, fx.create(t, ctx, "gw_items", map[string]any{"name": "u", "sku": fmt.Sprintf("U%d", i)}))
		}
		delIDs := make([]uuid.UUID, workers)
		for i := range delIDs {
			delIDs[i] = gwID(t, fx.create(t, ctx, "gw_items", map[string]any{"name": "d", "sku": fmt.Sprintf("D%d", i)}))
		}
		InvalidateTableColumns()

		var wg sync.WaitGroup
		errs := make(chan error, 2*workers)
		for w := 0; w < workers; w++ {
			wg.Add(2)
			go func(w int) {
				defer wg.Done()
				in := map[string]any{"sku": fmt.Sprintf("W%d", w), "category_id": cat["id"], "qty": w}
				if _, err := fx.svc.UpdateGuarded(ctx, "gw_items", fx.user, upIDs[w%rows], in, gwNever); err != nil {
					errs <- fmt.Errorf("update %d: %w", w, err)
				}
			}(w)
			go func(w int) {
				defer wg.Done()
				seal := map[string]any{"ver": w + 1, "category_id": cat["id"]}
				if err := fx.svc.DeleteSealed(ctx, "gw_items", fx.user, delIDs[w], seal, gwNever); err != nil {
					errs <- fmt.Errorf("delete %d: %w", w, err)
				}
			}(w)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Error(err)
		}
		if t.Failed() {
			return
		}
		if got := fx.bus.count(".updated"); got != workers {
			t.Fatalf("updated events: %d, want %d", got, workers)
		}
		if got := fx.bus.count(".deleted"); got != workers {
			t.Fatalf("deleted events: %d, want %d", got, workers)
		}
	})
}

// ---- R4: the lock does not rewrite the row, and respects the tenant ---------

func TestGuardedWriteOtherTenantIsNotFound(t *testing.T) {
	eachGW(t, 2, func(t *testing.T, fx *gwFx) {
		ctx := context.Background()
		id := gwID(t, fx.create(t, ctx, "gw_items", map[string]any{"name": "n", "sku": "A"}))
		other := newUser(uuid.New())
		if _, err := fx.svc.UpdateGuarded(ctx, "gw_items", other, id, map[string]any{"qty": 9}, gwNever); !errors.Is(err, ErrRecordNotFound) {
			t.Fatalf("update of another tenant's row: %v", err)
		}
		if err := fx.svc.DeleteSealed(ctx, "gw_items", other, id, map[string]any{"ver": 9}, gwNever); !errors.Is(err, ErrRecordNotFound) {
			t.Fatalf("delete of another tenant's row: %v", err)
		}
		if r := fx.row(t, id); r.Qty != 0 || r.Ver != 0 || r.DeletedAt.Valid {
			t.Fatalf("another tenant changed the row: %+v", r)
		}
	})
}

func TestClaimRowPostgresLocksWithoutNewTupleVersion(t *testing.T) {
	eachGW(t, 4, func(t *testing.T, fx *gwFx) {
		if fx.db.Dialector.Name() != "postgres" {
			t.Skip("the row lock of SELECT ... FOR UPDATE is a Postgres property")
		}
		ctx := context.Background()
		id := gwID(t, fx.create(t, ctx, "gw_items", map[string]any{"name": "n", "sku": "A"}))
		inst, _, err := fx.svc.resolveModel(ctx, "gw_items")
		if err != nil {
			t.Fatal(err)
		}
		xminOf := func(db *gorm.DB) string {
			var x string
			if err := db.Raw("SELECT xmin::text FROM "+fx.items+" WHERE id = ?", id).Scan(&x).Error; err != nil {
				t.Fatal(err)
			}
			return x
		}
		held := func() error { // another connection tries to take the same lock
			var got []string
			return fx.db.Raw("SELECT id::text FROM "+fx.items+" WHERE id = ? FOR UPDATE NOWAIT", id).Scan(&got).Error
		}

		before := xminOf(fx.db)
		err = fx.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := fx.svc.claimRow(ctx, tx, fx.items, inst, fx.user, id, "ver"); err != nil {
				return err
			}
			if got := xminOf(tx); got != before {
				return fmt.Errorf("the claim wrote a new tuple version (xmin %s -> %s)", before, got)
			}
			if held() == nil {
				return errors.New("the row is not locked after the claim")
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := xminOf(fx.db); got != before {
			t.Fatalf("xmin changed after the claim: %s -> %s", before, got)
		}

		// A row of another tenant is not found and no lock is taken on it.
		other := newUser(uuid.New())
		err = fx.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := fx.svc.claimRow(ctx, tx, fx.items, inst, other, id, "ver"); !errors.Is(err, ErrRecordNotFound) {
				return fmt.Errorf("claim of another tenant's row: %v", err)
			}
			if err := held(); err != nil {
				return fmt.Errorf("a row of another tenant was locked: %w", err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}

// ---- R5: the guarded column must be comparable -------------------------------

func TestGuardFieldMustBeComparable(t *testing.T) {
	eachGW(t, 2, func(t *testing.T, fx *gwFx) {
		ctx := context.Background()
		for _, ok := range []string{"ver", "qty", "category_id"} {
			if err := fx.svc.CheckGuardField(ctx, "gw_items", ok); err != nil {
				t.Fatalf("%s must be accepted: %v", ok, err)
			}
		}
		err := fx.svc.CheckGuardField(ctx, "gw_items", "hot")
		if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "guard_field_not_comparable") {
			t.Fatalf("omitempty column: %v", err)
		}
		for _, bad := range []string{"deleted_at", "ghost"} {
			if err := fx.svc.CheckGuardField(ctx, "gw_items", bad); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("%s must be refused: %v", bad, err)
			}
		}

		// The guarded writes refuse it too (a caller that skipped registration),
		// before touching the row or running a before-delete hook.
		id := gwID(t, fx.create(t, ctx, "gw_items", map[string]any{"name": "n", "sku": "A"}))
		g := &WriteGuard{Field: "hot", Stale: func(any) (bool, error) { return false, nil }}
		if _, err := fx.svc.UpdateGuarded(ctx, "gw_items", fx.user, id, map[string]any{"qty": 1}, g); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("UpdateGuarded: %v", err)
		}
		if err := fx.svc.DeleteSealed(ctx, "gw_items", fx.user, id, map[string]any{"qty": 1}, g); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("DeleteSealed: %v", err)
		}
		if fx.beforeDel.Load() != 0 {
			t.Fatal("a refused guard ran the before-delete hook")
		}
		if r := fx.row(t, id); r.Qty != 0 || r.DeletedAt.Valid {
			t.Fatalf("a refused guard wrote: %+v", r)
		}
	})
}

// ---- R6: a stale seal does not run the before-delete hooks ------------------

func TestDeleteSealedAlreadyStaleSkipsBeforeDeleteHooks(t *testing.T) {
	eachGW(t, 2, func(t *testing.T, fx *gwFx) {
		ctx := context.Background()
		id := gwID(t, fx.create(t, ctx, "gw_items", map[string]any{"name": "n", "sku": "A"}))
		if err := fx.svc.DeleteSealed(ctx, "gw_items", fx.user, id, map[string]any{"ver": 2}, gwAlways); !errors.Is(err, ErrStaleWrite) {
			t.Fatalf("stale seal: %v", err)
		}
		if fx.beforeDel.Load() != 0 || fx.beforeUp.Load() != 0 {
			t.Fatalf("hooks ran for a write that was already stale: before-delete %d, before-update %d", fx.beforeDel.Load(), fx.beforeUp.Load())
		}
		if fx.bus.count(".deleted") != 0 || fx.row(t, id).DeletedAt.Valid {
			t.Fatal("a stale seal deleted the row")
		}
		// Not stale: the hook runs once and the row goes.
		if err := fx.svc.DeleteSealed(ctx, "gw_items", fx.user, id, map[string]any{"ver": 2}, gwNever); err != nil {
			t.Fatal(err)
		}
		if fx.beforeDel.Load() != 1 {
			t.Fatalf("before-delete hook ran %d times", fx.beforeDel.Load())
		}
	})
}
