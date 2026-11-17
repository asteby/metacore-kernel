package inbound

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/metadata"
	"github.com/asteby/metacore-kernel/modelbase"
)

// openSQLite skips when the SQLite driver is unavailable (CGO disabled, e.g.
// Windows without a C toolchain). CI (Linux, CGO on) runs these.
func openSQLite(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+uuid.NewString()+"?mode=memory&cache=shared"),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent), DisableForeignKeyConstraintWhenMigrating: true})
	if err != nil {
		t.Skipf("sqlite unavailable: %v", err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	return db
}

func TestGormReceiptStore(t *testing.T) {
	db := openSQLite(t)
	st, err := NewGormReceiptStore(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	org, other := uuid.New(), uuid.New()
	r := Receipt{OrgID: org, Contract: "stock.changed", Version: 1, KeyHash: HashKey(org, "stock.changed", "k1"), BodyHash: "b1"}

	cur, claimed, err := st.Begin(ctx, r, time.Minute)
	if err != nil || !claimed || cur.Status != StatusProcessing {
		t.Fatalf("first begin: %+v %v %v", cur, claimed, err)
	}
	// Second reservation of the same key (even concurrent) is refused.
	var wg sync.WaitGroup
	var won int32
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, c, e := st.Begin(ctx, r, time.Minute); e == nil && c {
				atomic.AddInt32(&won, 1)
			}
		}()
	}
	wg.Wait()
	if won != 0 {
		t.Fatalf("reservation granted %d more times", won)
	}
	// Same key in another org is independent.
	r2 := r
	r2.OrgID, r2.KeyHash = other, HashKey(other, "stock.changed", "k1")
	if _, c, err := st.Begin(ctx, r2, time.Minute); err != nil || !c {
		t.Fatalf("other org: %v %v", c, err)
	}

	if err := st.Finish(ctx, cur.ID, StatusFailed, "", "internal"); err != nil {
		t.Fatal(err)
	}
	cur2, claimed, err := st.Begin(ctx, r, time.Minute)
	if err != nil || !claimed || cur2.Attempts != 2 {
		t.Fatalf("failed must be re-claimable: %+v %v %v", cur2, claimed, err)
	}
	if err := st.Finish(ctx, cur.ID, StatusApplied, "rec-1", ""); err != nil {
		t.Fatal(err)
	}
	cur3, claimed, _ := st.Begin(ctx, r, time.Minute)
	if claimed || cur3.Status != StatusApplied || cur3.RecordID != "rec-1" {
		t.Fatalf("applied must replay: %+v %v", cur3, claimed)
	}

	if err := st.Record(ctx, Receipt{OrgID: org, Contract: "stock.changed", Status: StatusRejected, ErrorCode: CodeInvalidSchema, BodyHash: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Record(ctx, Receipt{OrgID: org, Contract: "stock.changed", Status: StatusRejected, ErrorCode: CodeInvalidSchema, BodyHash: "y"}); err != nil {
		t.Fatalf("several rejected rows must coexist: %v", err)
	}
	all, _ := st.List(ctx, org, ReceiptFilter{})
	if len(all) != 3 {
		t.Fatalf("org list = %d", len(all))
	}
	for _, x := range all {
		if x.OrgID != org {
			t.Fatal("list leaked another org")
		}
	}
	byKey, _ := st.List(ctx, org, ReceiptFilter{Contract: "stock.changed", EventKey: "k1"})
	if len(byKey) != 1 {
		t.Fatalf("by key = %d", len(byKey))
	}
	rej, _ := st.List(ctx, org, ReceiptFilter{Status: StatusRejected})
	if len(rej) != 2 {
		t.Fatalf("rejected = %d", len(rej))
	}
}

// ---- DynamicExecutor against the real dynamic.Service ----------------------

type invItem struct {
	modelbase.BaseUUIDModel
	SKU       string `json:"sku" gorm:"size:64"`
	Warehouse string `json:"warehouse" gorm:"size:32"`
	Quantity  int    `json:"quantity"`
	Bin       string `json:"bin" gorm:"size:32"`
}

func (invItem) TableName() string { return "inbound_inv_items" }
func (invItem) DefineTable() modelbase.TableMetadata {
	return modelbase.TableMetadata{
		Title: "Inventory",
		Columns: []modelbase.ColumnDef{
			{Key: "sku", Label: "SKU"}, {Key: "warehouse", Label: "Warehouse"}, {Key: "quantity", Label: "Qty"},
		},
		SearchColumns: []string{"sku"},
	}
}
func (invItem) DefineModal() modelbase.ModalMetadata {
	return modelbase.ModalMetadata{Title: "Inventory"}
}

func newDynExec(t *testing.T) (*DynamicExecutor, *dynamic.Service) {
	t.Helper()
	db := openSQLite(t)
	db.Exec(`CREATE TABLE inbound_inv_items (id TEXT PRIMARY KEY, organization_id TEXT, created_by_id TEXT,
		created_at DATETIME, updated_at DATETIME, deleted_at DATETIME,
		sku TEXT, warehouse TEXT, quantity INTEGER, bin TEXT)`)
	modelbase.Register("inbound_inv_items", func() modelbase.ModelDefiner { return &invItem{} })
	svc := dynamic.New(dynamic.Config{AllowUnconfiguredPermissions: true, DB: db, Metadata: metadata.New(metadata.Config{CacheTTL: -1})})
	return &DynamicExecutor{Service: svc}, svc
}

func TestDynamicExecutorUpsertAndOrgIsolation(t *testing.T) {
	ex, svc := newDynExec(t)
	ctx := context.Background()
	orgA, orgB := uuid.New(), uuid.New()
	route := Route{Model: "inbound_inv_items", Op: OpUpsert, Match: []string{"sku", "warehouse"},
		Mapping: map[string]string{"sku": "sku", "warehouse": "warehouse", "quantity": "quantity"}}
	run := func(org uuid.UUID, qty int64) Outcome {
		out, err := ex.Execute(ctx, Operation{Org: org, Route: route,
			Values: map[string]any{"sku": "S1", "warehouse": "W1", "quantity": qty}})
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
		return out
	}
	a1 := run(orgA, 5)
	a2 := run(orgA, 9)
	b1 := run(orgB, 100)
	if !a1.Created || a2.Created || a1.RecordID != a2.RecordID {
		t.Fatalf("upsert must update in place: %+v %+v", a1, a2)
	}
	if !b1.Created || b1.RecordID == a1.RecordID {
		t.Fatalf("org B must get its own row: %+v", b1)
	}
	id, _ := uuid.Parse(a1.RecordID)
	row, err := svc.Get(ctx, "inbound_inv_items", dynamic.NewSystemCaller(orgA), id)
	if err != nil || row["quantity"] != float64(9) && row["quantity"] != int64(9) && row["quantity"] != 9 {
		t.Fatalf("row: %v %v", row, err)
	}
	if _, err := svc.Get(ctx, "inbound_inv_items", dynamic.NewSystemCaller(orgB), id); err == nil {
		t.Fatal("org B can read org A's row")
	}

	// update with no match -> not found; create always inserts.
	upd := route
	upd.Op = OpUpdate
	_, err = ex.Execute(ctx, Operation{Org: orgA, Route: upd, Values: map[string]any{"sku": "NOPE", "warehouse": "W1", "quantity": int64(1)}})
	if xe, ok := err.(*ExecError); !ok || xe.Kind != KindNotFound {
		t.Fatalf("update without row: %v", err)
	}
}

func TestDynamicExecutorRefusesUnfilterableMatchField(t *testing.T) {
	ex, _ := newDynExec(t)
	ctx := context.Background()
	org := uuid.New()
	mk := func(sku, bin string) Operation {
		return Operation{Org: org, Values: map[string]any{"sku": sku, "bin": bin, "warehouse": "W", "quantity": int64(1)},
			Route: Route{Model: "inbound_inv_items", Op: OpUpsert, Match: []string{"bin"},
				Mapping: map[string]string{"sku": "sku", "bin": "bin"}}}
	}
	if _, err := ex.Execute(ctx, Operation{Org: org, Route: Route{Model: "inbound_inv_items", Op: OpCreate},
		Values: map[string]any{"sku": "A", "bin": "B1", "warehouse": "W", "quantity": int64(1)}}); err != nil {
		t.Fatal(err)
	}
	// "bin" is not a declared column: the query layer drops the filter. The
	// executor must notice and refuse instead of updating an arbitrary row.
	_, err := ex.Execute(ctx, mk("B", "B2"))
	if xe, ok := err.(*ExecError); !ok || xe.Kind != KindInternal {
		t.Fatalf("want internal ExecError, got %v", err)
	}
}

func TestDynamicExecutorEndToEndOverHTTP(t *testing.T) {
	ex, _ := newDynExec(t)
	reg := NewRegistry()
	c := Contract{Key: "stock.changed", Version: 1, JSONSchema: json.RawMessage(stockSchema), Idempotency: "{event_id}",
		Auth: ProfileHMACCanonicalV1,
		Route: Route{Model: "inbound_inv_items", Op: OpUpsert, Match: []string{"sku", "warehouse"},
			Mapping: map[string]string{"sku": "sku", "warehouse": "warehouse", "quantity": "quantity"}}}
	if err := reg.Register(c); err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, func(cfg *Config) { cfg.Contracts, cfg.Executor = reg, ex })
	code, out := e.send(t, e.orgA, "stock.changed", stockBody("d1", "SKU-1", 4), nil)
	if code != http.StatusOK || out["status"] != "applied" {
		t.Fatalf("%d %v", code, out)
	}
	_, out = e.send(t, e.orgA, "stock.changed", stockBody("d1", "SKU-1", 4), nil)
	if out["status"] != "replayed" {
		t.Fatalf("%v", out)
	}
}
