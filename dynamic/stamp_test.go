package dynamic

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/metadata"
	"github.com/asteby/metacore-kernel/modelbase"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// stampDB: a tenant-scoped catalog ("stamp_products") to copy from.
func stampDB(t *testing.T) (*gorm.DB, uuid.UUID, uuid.UUID) {
	t.Helper()
	db := setupTestDB(t)
	if err := db.Exec(`CREATE TABLE stamp_products (
		id TEXT PRIMARY KEY, organization_id TEXT, min_price REAL, cost_price REAL, deleted_at DATETIME)`).Error; err != nil {
		t.Fatal(err)
	}
	org, other := uuid.New(), uuid.New()
	for _, r := range []struct {
		id  string
		org uuid.UUID
		min float64
	}{{"p1", org, 50}, {"p-other", other, 99}} {
		if err := db.Exec(`INSERT INTO stamp_products (id, organization_id, min_price, cost_price) VALUES (?,?,?,?)`,
			r.id, r.org.String(), r.min, 30).Error; err != nil {
			t.Fatal(err)
		}
	}
	return db, org, other
}

// itemStamps mirrors a sale line: product snapshot, a per-unit shortfall
// formula, and a price authorization slot.
func itemStamps() *ModelStamps {
	cols := []manifest.ColumnDef{
		{Name: "product_id", Ref: "stamp_products"},
		{Name: "price_approval_id", Grant: &manifest.GrantDef{
			Policy: "price_below_min", Sets: "price_authorized",
			ContextEq:  map[string]string{"product_id": "product_id"},
			ContextGte: map[string]string{"unit_shortfall": "min_price_at_sale - unit_price"},
		}},
	}
	snaps := []manifest.SnapshotDef{{From: "product_id", Fields: map[string]string{"min_price": "min_price_at_sale", "cost_price": "unit_cost"}}}
	formulas := []manifest.Formula{
		{Target: "subtotal", Expr: "quantity * unit_price"}, // does not read a stamped column
		{Target: "price_shortfall", Expr: "(min_price_at_sale - unit_price) * (1 - price_authorized)"},
	}
	return NewModelStamps(cols, snaps, formulas)
}

func TestNewModelStampsDerivesOnlyStampDependentFormulas(t *testing.T) {
	st := itemStamps()
	if len(st.Derived) != 1 || st.Derived[0].Target != "price_shortfall" {
		t.Fatalf("derived = %+v", st.Derived)
	}
	if st.RefTables["product_id"] != "stamp_products" || len(st.Grants) != 1 {
		t.Fatalf("stamps = %+v", st)
	}
	if NewModelStamps([]manifest.ColumnDef{{Name: "a"}}, nil, nil) != nil {
		t.Fatal("un modelo sin snapshots ni grants no tiene fase de stamp")
	}
}

func TestStampSnapshotOverwritesForgedValuesAndScopesToOrg(t *testing.T) {
	db, org, _ := stampDB(t)
	st := itemStamps()
	noConsume := func(context.Context, ConsumeInput) error { t.Fatal("no se necesita autorización"); return nil }

	row := map[string]any{"product_id": "p1", "unit_price": 60.0, "min_price_at_sale": 0.0, "unit_cost": 1.0}
	if err := st.apply(context.Background(), db, org, uuid.New(), row, noConsume); err != nil {
		t.Fatal(err)
	}
	if row["min_price_at_sale"] != 50.0 || row["unit_cost"] != 30.0 {
		t.Fatalf("el servidor debe ganar sobre lo enviado: %v", row)
	}
	// Above the floor: shortfall is negative (headroom), nothing to authorize.
	if row["price_shortfall"] != -10.0 || row["price_authorized"] != 0.0 {
		t.Fatalf("derivado: %v", row)
	}

	// A product of another org is invisible: the targets are cleared.
	row = map[string]any{"product_id": "p-other", "unit_price": 10.0, "min_price_at_sale": 0.0}
	if err := st.apply(context.Background(), db, org, uuid.New(), row, noConsume); err != nil {
		t.Fatal(err)
	}
	if _, ok := row["unit_cost"]; ok || row["min_price_at_sale"] != nil && row["min_price_at_sale"] != 0.0 {
		t.Fatalf("otra org no debe copiarse: %v", row)
	}
}

func TestStampGrantRedeemedOnlyWhenNeeded(t *testing.T) {
	db, org, _ := stampDB(t)
	st := itemStamps()
	actor, grant := uuid.New(), uuid.New()
	var got []ConsumeInput
	consume := func(_ context.Context, in ConsumeInput) error { got = append(got, in); return nil }

	// Under the floor with an authorization: redeemed with the product anchored
	// and the per-unit shortfall as the amount; flag 1 zeroes the shortfall.
	row := map[string]any{"product_id": "p1", "unit_price": 40.0, "price_approval_id": grant.String()}
	if err := st.apply(context.Background(), db, org, actor, row, consume); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ApprovalID != grant || got[0].Policy != "price_below_min" || got[0].RequestedBy != actor ||
		got[0].ContextEq["product_id"] != "p1" || got[0].ContextGte["unit_shortfall"] != 10 {
		t.Fatalf("consume = %+v", got)
	}
	if row["price_authorized"] != 1.0 || row["price_shortfall"] != 0.0 {
		t.Fatalf("autorizada: %v", row)
	}

	// Over the floor the id is ignored: not redeemed, not burned, flag 0.
	got = nil
	row = map[string]any{"product_id": "p1", "unit_price": 60.0, "price_approval_id": grant.String()}
	if err := st.apply(context.Background(), db, org, actor, row, consume); err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 || row["price_authorized"] != 0.0 {
		t.Fatalf("no se necesitaba: %v %+v", row, got)
	}

	// Under the floor and NO id: flag 0, the shortfall stays positive for the guard.
	row = map[string]any{"product_id": "p1", "unit_price": 40.0, "price_authorized": 1.0}
	if err := st.apply(context.Background(), db, org, actor, row, consume); err != nil {
		t.Fatal(err)
	}
	if row["price_authorized"] != 0.0 || row["price_shortfall"] != 10.0 {
		t.Fatalf("un flag enviado por el cliente no autoriza: %v", row)
	}

	// A refused grant refuses the write.
	refuse := func(context.Context, ConsumeInput) error { return fmt.Errorf("%w: already used", ErrApprovalConsume) }
	row = map[string]any{"product_id": "p1", "unit_price": 40.0, "price_approval_id": grant.String()}
	if err := st.apply(context.Background(), db, org, actor, row, refuse); !errors.Is(err, ErrApprovalConsume) {
		t.Fatalf("err = %v", err)
	}
	row = map[string]any{"product_id": "p1", "unit_price": 40.0, "price_approval_id": "not-a-uuid"}
	if err := st.apply(context.Background(), db, org, actor, row, consume); !errors.Is(err, ErrApprovalConsume) {
		t.Fatalf("id inválido: %v", err)
	}
}

// Through Service.Create: the client's forged value is replaced by the copy
// from the related row, before hooks and guards.
func TestServiceCreateRunsStampPhase(t *testing.T) {
	db, org, _ := stampDB(t)
	modelbase.Register("test_products", func() modelbase.ModelDefiner { return &TestProduct{} })
	st := NewModelStamps(
		[]manifest.ColumnDef{{Name: "name", Ref: "stamp_products"}},
		[]manifest.SnapshotDef{{From: "name", Fields: map[string]string{"min_price": "price"}}},
		nil,
	)
	svc := New(Config{
		DB: db, Metadata: metadata.New(metadata.Config{CacheTTL: -1}),
		StampResolver: func(_ context.Context, model string) (*ModelStamps, bool) {
			return st, model == "test_products"
		},
	})
	out, err := svc.Create(context.Background(), "test_products", newUser(org), map[string]any{"name": "p1", "price": 999.0})
	if err != nil {
		t.Fatal(err)
	}
	if out["price"] != 50.0 {
		t.Fatalf("price = %v, want the snapshot 50", out["price"])
	}
}
