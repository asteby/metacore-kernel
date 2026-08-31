package dynamic

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestBackfill_RollupCorrectsDesyncedTotals(t *testing.T) {
	db := setupComputeDB(t)
	ctx := context.Background()
	org := uuid.New()
	parentID := uuid.New()

	// Parent row imported with stale/default rollup values.
	db.Exec(`INSERT INTO sales_orders (id, organization_id, total, line_count) VALUES (?, ?, 0, 0)`,
		parentID.String(), org.String())
	// Children written directly (no hooks fired), subtotals 25 and 40.
	db.Exec(`INSERT INTO sales_order_items (id, sales_order_id, quantity, unit_price, discount, subtotal) VALUES (?, ?, 1, 25, 0, 25)`,
		uuid.New().String(), parentID.String())
	db.Exec(`INSERT INTO sales_order_items (id, sales_order_id, quantity, unit_price, discount, subtotal) VALUES (?, ?, 1, 40, 0, 40)`,
		uuid.New().String(), parentID.String())

	reports, err := Backfill(ctx, db, nil, salesManifest(), BackfillOptions{OrgID: org})
	if err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	total, count := readOrder(t, db, parentID.String())
	if total != 65 || count != 2 {
		t.Fatalf("after backfill: total=%v count=%v, want 65/2", total, count)
	}

	var rollupRep *BackfillModelReport
	for i := range reports {
		if reports[i].Model == "SalesOrder" {
			rollupRep = &reports[i]
		}
	}
	if rollupRep == nil {
		t.Fatal("no report for SalesOrder rollup")
	}
	if rollupRep.RowsScanned != 1 || rollupRep.RowsUpdated != 1 {
		t.Errorf("SalesOrder report = %+v, want scanned=1 updated=1", rollupRep)
	}

	// A second run against the now-consistent data must update nothing.
	reports2, err := Backfill(ctx, db, nil, salesManifest(), BackfillOptions{OrgID: org})
	if err != nil {
		t.Fatalf("Backfill (2nd run): %v", err)
	}
	for _, r := range reports2 {
		if r.RowsUpdated != 0 {
			t.Errorf("2nd run: %s tier %d updated %d rows, want 0 (already consistent)", r.Model, r.Tier, r.RowsUpdated)
		}
	}
}

func TestBackfill_FormulaCorrectsRawImportedRow(t *testing.T) {
	db := setupComputeDB(t)
	ctx := context.Background()
	org := uuid.New()
	parentID := uuid.New()
	db.Exec(`INSERT INTO sales_orders (id, organization_id, total, line_count) VALUES (?, ?, 0, 0)`,
		parentID.String(), org.String())
	// Child row imported raw: quantity/unit_price/discount set, subtotal left
	// at 0 (the formula never fired).
	itemID := uuid.New().String()
	db.Exec(`INSERT INTO sales_order_items (id, sales_order_id, quantity, unit_price, discount, subtotal) VALUES (?, ?, 3, 10, 5, 0)`,
		itemID, parentID.String())

	reports, err := Backfill(ctx, db, nil, salesManifest(), BackfillOptions{OrgID: org})
	if err != nil {
		t.Fatalf("Backfill: %v", err)
	}

	var subtotal float64
	if err := db.Raw(`SELECT subtotal FROM sales_order_items WHERE id = ?`, itemID).Scan(&subtotal).Error; err != nil {
		t.Fatalf("read item: %v", err)
	}
	if subtotal != 25 {
		t.Fatalf("subtotal after backfill = %v, want 25 (3*10-5)", subtotal)
	}
	// The rollup must have picked up the corrected subtotal in the same run
	// (Tier-2 runs before Tier-1).
	total, _ := readOrder(t, db, parentID.String())
	if total != 25 {
		t.Fatalf("parent total after backfill = %v, want 25 (formula-then-rollup ordering)", total)
	}

	var formulaRep *BackfillModelReport
	for i := range reports {
		if reports[i].Model == "SalesOrderItem" && reports[i].Tier == 2 {
			formulaRep = &reports[i]
		}
	}
	if formulaRep == nil || formulaRep.RowsUpdated != 1 {
		t.Fatalf("formula report = %+v, want RowsUpdated=1", formulaRep)
	}
}

func TestBackfill_DryRunDoesNotWrite(t *testing.T) {
	db := setupComputeDB(t)
	ctx := context.Background()
	org := uuid.New()
	parentID := uuid.New()
	db.Exec(`INSERT INTO sales_orders (id, organization_id, total, line_count) VALUES (?, ?, 0, 0)`,
		parentID.String(), org.String())
	db.Exec(`INSERT INTO sales_order_items (id, sales_order_id, quantity, unit_price, discount, subtotal) VALUES (?, ?, 1, 25, 0, 25)`,
		uuid.New().String(), parentID.String())

	reports, err := Backfill(ctx, db, nil, salesManifest(), BackfillOptions{OrgID: org, DryRun: true})
	if err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	total, count := readOrder(t, db, parentID.String())
	if total != 0 || count != 0 {
		t.Fatalf("dry-run wrote to the row: total=%v count=%v", total, count)
	}
	found := false
	for _, r := range reports {
		if r.Model == "SalesOrder" && r.RowsUpdated == 1 {
			found = true
		}
	}
	if !found {
		t.Error("dry-run report must still report the row it WOULD update")
	}
}

func TestBackfill_OrgScoped(t *testing.T) {
	db := setupComputeDB(t)
	ctx := context.Background()
	orgA := uuid.New()
	orgB := uuid.New()
	parentA := uuid.New()
	parentB := uuid.New()
	db.Exec(`INSERT INTO sales_orders (id, organization_id, total, line_count) VALUES (?, ?, 0, 0)`, parentA.String(), orgA.String())
	db.Exec(`INSERT INTO sales_orders (id, organization_id, total, line_count) VALUES (?, ?, 0, 0)`, parentB.String(), orgB.String())
	db.Exec(`INSERT INTO sales_order_items (id, sales_order_id, quantity, unit_price, discount, subtotal) VALUES (?, ?, 1, 25, 0, 25)`, uuid.New().String(), parentA.String())
	db.Exec(`INSERT INTO sales_order_items (id, sales_order_id, quantity, unit_price, discount, subtotal) VALUES (?, ?, 1, 99, 0, 99)`, uuid.New().String(), parentB.String())

	if _, err := Backfill(ctx, db, nil, salesManifest(), BackfillOptions{OrgID: orgA}); err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	totalA, _ := readOrder(t, db, parentA.String())
	totalB, _ := readOrder(t, db, parentB.String())
	if totalA != 25 {
		t.Errorf("orgA total = %v, want 25", totalA)
	}
	if totalB != 0 {
		t.Errorf("orgB total = %v, want untouched (0) — backfill leaked across orgs", totalB)
	}
}

func TestBackfill_ParentWithNoChildrenZeroes(t *testing.T) {
	db := setupComputeDB(t)
	ctx := context.Background()
	org := uuid.New()
	parentID := uuid.New()
	// Parent imported with a stale nonzero total but no children at all.
	db.Exec(`INSERT INTO sales_orders (id, organization_id, total, line_count) VALUES (?, ?, 999, 3)`,
		parentID.String(), org.String())

	if _, err := Backfill(ctx, db, nil, salesManifest(), BackfillOptions{OrgID: org}); err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	total, count := readOrder(t, db, parentID.String())
	if total != 0 || count != 0 {
		t.Fatalf("childless parent after backfill: total=%v count=%v, want 0/0", total, count)
	}
}

func TestBackfill_BatchesAcrossMultiplePages(t *testing.T) {
	db := setupComputeDB(t)
	ctx := context.Background()
	org := uuid.New()

	const nOrders = 7
	ids := make([]uuid.UUID, nOrders)
	for i := 0; i < nOrders; i++ {
		ids[i] = uuid.New()
		db.Exec(`INSERT INTO sales_orders (id, organization_id, total, line_count) VALUES (?, ?, 0, 0)`,
			ids[i].String(), org.String())
		db.Exec(`INSERT INTO sales_order_items (id, sales_order_id, quantity, unit_price, discount, subtotal) VALUES (?, ?, 1, 10, 0, 10)`,
			uuid.New().String(), ids[i].String())
	}

	reports, err := Backfill(ctx, db, nil, salesManifest(), BackfillOptions{OrgID: org, BatchSize: 3})
	if err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	for _, id := range ids {
		total, count := readOrder(t, db, id.String())
		if total != 10 || count != 1 {
			t.Fatalf("order %s: total=%v count=%v, want 10/1", id, total, count)
		}
	}
	var rollupRep *BackfillModelReport
	for i := range reports {
		if reports[i].Model == "SalesOrder" {
			rollupRep = &reports[i]
		}
	}
	if rollupRep == nil || rollupRep.RowsScanned != nOrders {
		t.Fatalf("rollup report = %+v, want RowsScanned=%d across 3 pages of batch size 3", rollupRep, nOrders)
	}
}

func TestBackfill_FieldsNarrowsScope(t *testing.T) {
	db := setupComputeDB(t)
	ctx := context.Background()
	org := uuid.New()
	parentID := uuid.New()
	db.Exec(`INSERT INTO sales_orders (id, organization_id, total, line_count) VALUES (?, ?, 999, 999)`,
		parentID.String(), org.String())
	db.Exec(`INSERT INTO sales_order_items (id, sales_order_id, quantity, unit_price, discount, subtotal) VALUES (?, ?, 1, 25, 0, 25)`,
		uuid.New().String(), parentID.String())

	if _, err := Backfill(ctx, db, nil, salesManifest(), BackfillOptions{OrgID: org, Fields: []string{"total"}}); err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	total, count := readOrder(t, db, parentID.String())
	if total != 25 {
		t.Errorf("total = %v, want 25 (in Fields)", total)
	}
	if count != 999 {
		t.Errorf("line_count = %v, want untouched at 999 (excluded via Fields)", count)
	}
}

func TestBackfill_RequiresOrgID(t *testing.T) {
	db := setupComputeDB(t)
	if _, err := Backfill(context.Background(), db, nil, salesManifest(), BackfillOptions{}); err == nil {
		t.Fatal("expected an error when OrgID is uuid.Nil")
	}
}
