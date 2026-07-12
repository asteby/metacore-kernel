package dynamic

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// salesManifest is a domain-agnostic-but-realistic manifest exercising both
// tiers: SalesOrder (parent, org-scoped) with a one_to_many "items" relation
// onto SalesOrderItem (child, NOT org-scoped). The relation declares Tier-1
// rollups (total = SUM(subtotal), line_count = COUNT). The child declares a
// Tier-2 formula (subtotal = quantity * unit_price - discount).
func salesManifest() manifest.Manifest {
	return manifest.Manifest{
		Key:     "sales",
		Name:    "Sales",
		Version: "1.0.0",
		ModelDefinitions: []manifest.ModelDefinition{
			{
				ModelKey:  "SalesOrder",
				TableName: "sales_orders",
				OrgScoped: true,
				Columns: []manifest.ColumnDef{
					{Name: "total", Type: "decimal"},
					{Name: "line_count", Type: "int"},
				},
				Relations: []manifest.RelationDef{
					{
						Name:       "items",
						Kind:       "one_to_many",
						Through:    "SalesOrderItem",
						ForeignKey: "sales_order_id",
						Rollups: []manifest.Rollup{
							{Target: "total", Fn: "sum", From: "subtotal"},
							{Target: "line_count", Fn: "count"},
						},
					},
				},
			},
			{
				ModelKey:  "SalesOrderItem",
				TableName: "sales_order_items",
				Columns: []manifest.ColumnDef{
					{Name: "sales_order_id", Type: "uuid"},
					{Name: "quantity", Type: "int"},
					{Name: "unit_price", Type: "decimal"},
					{Name: "discount", Type: "decimal"},
					{Name: "subtotal", Type: "decimal"},
				},
				Formulas: []manifest.Formula{
					{Target: "subtotal", Expr: "quantity * unit_price - discount"},
				},
			},
		},
	}
}

func TestBuildComputeBindings(t *testing.T) {
	b := BuildComputeBindings(salesManifest())

	got := b.rollupsByChild["SalesOrderItem"]
	if len(got) != 1 {
		t.Fatalf("expected 1 rollup binding on SalesOrderItem, got %d", len(got))
	}
	bind := got[0]
	if bind.parentTable != "sales_orders" || bind.childTable != "sales_order_items" || bind.fk != "sales_order_id" {
		t.Errorf("unexpected binding: %+v", bind)
	}
	if len(bind.rollups) != 2 {
		t.Errorf("expected 2 rollups, got %d", len(bind.rollups))
	}

	fb, ok := b.formulasByModel["SalesOrderItem"]
	if !ok || len(fb.formulas) != 1 {
		t.Fatalf("expected 1 formula on SalesOrderItem, got %+v", fb)
	}
}

func TestBuildRollupSQL(t *testing.T) {
	b := BuildComputeBindings(salesManifest()).rollupsByChild["SalesOrderItem"][0]
	pid := "11111111-1111-1111-1111-111111111111"
	org := "22222222-2222-2222-2222-222222222222"

	// No exclusion (create/update path).
	sql, args, err := buildRollupSQL(b, pid, org, nil)
	if err != nil {
		t.Fatalf("buildRollupSQL: %v", err)
	}
	wantFrags := []string{
		`UPDATE "sales_orders" SET`,
		`"total" = COALESCE((SELECT SUM("subtotal") FROM "sales_order_items" WHERE "sales_order_id" = ?), 0)`,
		`"line_count" = COALESCE((SELECT COUNT(*) FROM "sales_order_items" WHERE "sales_order_id" = ?), 0)`,
		`WHERE "id" = ? AND "organization_id" = ?`,
	}
	for _, f := range wantFrags {
		if !strings.Contains(sql, f) {
			t.Errorf("SQL missing fragment:\n  want substr: %s\n  got: %s", f, sql)
		}
	}
	// args: parentID (total), parentID (count), parentID (where), org
	if len(args) != 4 {
		t.Fatalf("expected 4 args, got %d: %v", len(args), args)
	}

	// Exclusion variant (delete path) adds `AND "id" <> ?` to each subquery
	// and one extra arg per rollup.
	exclID := "33333333-3333-3333-3333-333333333333"
	sqlEx, argsEx, err := buildRollupSQL(b, pid, org, &exclID)
	if err != nil {
		t.Fatalf("buildRollupSQL exclusion: %v", err)
	}
	if strings.Count(sqlEx, `AND "id" <> ?`) != 2 {
		t.Errorf("expected 2 exclusion clauses, got: %s", sqlEx)
	}
	// 2 rollups * (parentID + exclID) + parentID(where) + org = 6
	if len(argsEx) != 6 {
		t.Errorf("expected 6 args with exclusion, got %d: %v", len(argsEx), argsEx)
	}
}

func TestComputeAggExpr(t *testing.T) {
	cases := []struct {
		r    manifest.Rollup
		want string
	}{
		{manifest.Rollup{Target: "total", Fn: "sum", From: "subtotal"}, `SUM("subtotal")`},
		{manifest.Rollup{Target: "n", Fn: "count"}, `COUNT(*)`},
		{manifest.Rollup{Target: "a", Fn: "avg", From: "x"}, `AVG("x")`},
		{manifest.Rollup{Target: "lo", Fn: "min", From: "x"}, `MIN("x")`},
		{manifest.Rollup{Target: "hi", Fn: "max", From: "x"}, `MAX("x")`},
		{manifest.Rollup{Target: "t", From: "x"}, `SUM("x")`}, // fn defaults to sum
		{manifest.Rollup{Target: "t", Fn: "sum", Expr: "subtotal + tax_amount"}, `SUM(("subtotal" + "tax_amount"))`},
	}
	for _, c := range cases {
		got, err := computeAggExpr(c.r)
		if err != nil {
			t.Fatalf("computeAggExpr(%+v): %v", c.r, err)
		}
		if got != c.want {
			t.Errorf("computeAggExpr(%+v) = %q, want %q", c.r, got, c.want)
		}
	}
}

func TestApplyFormulas(t *testing.T) {
	fb := BuildComputeBindings(salesManifest()).formulasByModel["SalesOrderItem"]

	// Create path: all values in input.
	input := map[string]any{"quantity": 3, "unit_price": 10.0, "discount": 5.0}
	if err := applyFormulas(context.Background(), nil, fb, input, nil); err != nil {
		t.Fatalf("applyFormulas: %v", err)
	}
	if got := input["subtotal"]; got != 25.0 {
		t.Errorf("subtotal = %v, want 25", got)
	}

	// Update path: partial input, existing supplies the rest.
	input2 := map[string]any{"quantity": 4} // unit_price/discount from existing
	existing := map[string]any{"unit_price": 10.0, "discount": 0.0, "subtotal": 30.0}
	if err := applyFormulas(context.Background(), nil, fb, input2, existing); err != nil {
		t.Fatalf("applyFormulas update: %v", err)
	}
	if got := input2["subtotal"]; got != 40.0 {
		t.Errorf("subtotal (update) = %v, want 40", got)
	}
}

// --- End-to-end against sqlite: hooks fire through a HookRegistry ----------

func setupComputeDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.Exec(`CREATE TABLE sales_orders (
		id TEXT PRIMARY KEY, organization_id TEXT, total REAL, line_count INTEGER )`)
	db.Exec(`CREATE TABLE sales_order_items (
		id TEXT PRIMARY KEY, sales_order_id TEXT,
		quantity REAL, unit_price REAL, discount REAL, subtotal REAL )`)
	return db
}

func TestRecomputeRollups_EndToEnd(t *testing.T) {
	db := setupComputeDB(t)
	ctx := context.Background()
	org := uuid.New()
	parentID := uuid.New().String()

	db.Exec(`INSERT INTO sales_orders (id, organization_id, total, line_count) VALUES (?, ?, 0, 0)`,
		parentID, org.String())

	bind := BuildComputeBindings(salesManifest()).rollupsByChild["SalesOrderItem"][0]

	// Two children, subtotals 25 and 40.
	c1 := uuid.New().String()
	c2 := uuid.New().String()
	db.Exec(`INSERT INTO sales_order_items (id, sales_order_id, subtotal) VALUES (?, ?, 25)`, c1, parentID)
	db.Exec(`INSERT INTO sales_order_items (id, sales_order_id, subtotal) VALUES (?, ?, 40)`, c2, parentID)

	if err := recomputeRollups(ctx, db, org, bind, parentID, nil); err != nil {
		t.Fatalf("recompute: %v", err)
	}
	total, count := readOrder(t, db, parentID)
	if total != 65 || count != 2 {
		t.Fatalf("after 2 inserts: total=%v count=%v, want 65/2", total, count)
	}

	// Update child 1 subtotal to 100 -> total 140.
	db.Exec(`UPDATE sales_order_items SET subtotal = 100 WHERE id = ?`, c1)
	if err := recomputeRollups(ctx, db, org, bind, parentID, nil); err != nil {
		t.Fatalf("recompute after update: %v", err)
	}
	total, count = readOrder(t, db, parentID)
	if total != 140 || count != 2 {
		t.Fatalf("after update: total=%v count=%v, want 140/2", total, count)
	}

	// Delete path: recompute with c2 EXCLUDED (simulates BeforeDelete) -> total 100, count 1.
	if err := recomputeRollups(ctx, db, org, bind, parentID, &c2); err != nil {
		t.Fatalf("recompute with exclusion: %v", err)
	}
	total, count = readOrder(t, db, parentID)
	if total != 100 || count != 1 {
		t.Fatalf("after exclusion: total=%v count=%v, want 100/1", total, count)
	}

	// Org isolation: wrong org id must not touch the row.
	if err := recomputeRollups(ctx, db, uuid.New(), bind, parentID, nil); err != nil {
		t.Fatalf("recompute wrong org: %v", err)
	}
	total, count = readOrder(t, db, parentID)
	if total != 100 || count != 1 {
		t.Fatalf("wrong-org recompute mutated the row: total=%v count=%v", total, count)
	}
}

// TestComputeHooks_FireThroughRegistry proves the registered hooks, when run
// via the HookRegistry's runners, self-compute the child (Tier-2) and roll up
// to the parent (Tier-1) — the ordering guarantee end-to-end.
func TestComputeHooks_FireThroughRegistry(t *testing.T) {
	db := setupComputeDB(t)
	ctx := context.Background()
	org := uuid.New()
	parentID := uuid.New().String()
	db.Exec(`INSERT INTO sales_orders (id, organization_id, total, line_count) VALUES (?, ?, 0, 0)`,
		parentID, org.String())

	reg := NewHookRegistry()
	RegisterComputeHooks(reg, salesManifest())

	user := &fakeUser{id: uuid.New(), orgID: org}

	// Simulate Service.Create for a child: BeforeCreate (formula) then write
	// then AfterCreate (rollup).
	createChild := func(quantity, unitPrice, discount float64) string {
		id := uuid.New().String()
		input := map[string]any{
			"id":             id,
			"sales_order_id": parentID,
			"quantity":       quantity,
			"unit_price":     unitPrice,
			"discount":       discount,
		}
		hc := HookContext{Model: "SalesOrderItem", User: user, DB: db}
		if err := reg.runBeforeCreate(ctx, hc, input); err != nil {
			t.Fatalf("beforeCreate: %v", err)
		}
		// subtotal must have been computed by the Tier-2 formula.
		db.Exec(`INSERT INTO sales_order_items (id, sales_order_id, quantity, unit_price, discount, subtotal)
			VALUES (?, ?, ?, ?, ?, ?)`,
			id, parentID, input["quantity"], input["unit_price"], input["discount"], input["subtotal"])
		record := map[string]any{"id": id, "sales_order_id": parentID, "subtotal": input["subtotal"]}
		if err := reg.runAfterCreate(ctx, hc, record); err != nil {
			t.Fatalf("afterCreate: %v", err)
		}
		return id
	}

	createChild(3, 10, 5) // subtotal 25
	createChild(2, 20, 0) // subtotal 40

	total, count := readOrder(t, db, parentID)
	if total != 65 || count != 2 {
		t.Fatalf("after 2 hook-driven creates: total=%v count=%v, want 65/2", total, count)
	}

	// Delete the second child via the BeforeDelete hook (exclusion path).
	var c2 string
	db.Raw(`SELECT id FROM sales_order_items WHERE subtotal = 40`).Scan(&c2)
	hc := HookContext{Model: "SalesOrderItem", User: user, DB: db}
	if err := reg.runBeforeDelete(ctx, hc, c2); err != nil {
		t.Fatalf("beforeDelete: %v", err)
	}
	db.Exec(`DELETE FROM sales_order_items WHERE id = ?`, c2)
	total, count = readOrder(t, db, parentID)
	if total != 25 || count != 1 {
		t.Fatalf("after hook-driven delete: total=%v count=%v, want 25/1", total, count)
	}
}

func readOrder(t *testing.T, db *gorm.DB, id string) (total float64, count int) {
	t.Helper()
	row := struct {
		Total     float64
		LineCount int
	}{}
	if err := db.Raw(`SELECT total, line_count FROM sales_orders WHERE id = ?`, id).Scan(&row).Error; err != nil {
		t.Fatalf("read order: %v", err)
	}
	return row.Total, row.LineCount
}

func TestApplyFormulas_Tier3WasmHandler(t *testing.T) {
	fb := formulaBinding{
		model: "test_products",
		table: "test_products",
		formulas: []manifest.Formula{
			{Target: "price", Tier: 3, Handler: "wasm:resolve_price"},
			// A subsequent Tier-2 formula must see the Tier-3 result in its env.
			{Target: "total", Expr: "price * 2"},
		},
	}
	invoked := false
	invoke := func(_ context.Context, model, handler string, row map[string]any) (any, error) {
		invoked = true
		if model != "test_products" || handler != "wasm:resolve_price" {
			t.Errorf("invoker got (%q, %q)", model, handler)
		}
		// The merged row must carry both existing and incoming values.
		if row["cost"] != 8.0 || row["quantity"] != 2.0 {
			t.Errorf("merged row = %v", row)
		}
		return 12.5, nil
	}

	input := map[string]any{"quantity": 2.0}
	existing := map[string]any{"cost": 8.0}
	if err := applyFormulas(context.Background(), invoke, fb, input, existing); err != nil {
		t.Fatalf("applyFormulas: %v", err)
	}
	if !invoked {
		t.Fatal("invoker was not called")
	}
	if input["price"] != 12.5 {
		t.Errorf("price = %v, want 12.5", input["price"])
	}
	if input["total"] != 25.0 {
		t.Errorf("total = %v, want 25 (Tier-2 must see the Tier-3 result)", input["total"])
	}

	// No invoker configured -> the Tier-3 formula is skipped, not an error.
	input2 := map[string]any{"quantity": 2.0}
	if err := applyFormulas(context.Background(), nil, fb, input2, existing); err != nil {
		t.Fatalf("applyFormulas nil invoker: %v", err)
	}
	if _, set := input2["price"]; set {
		t.Error("price must stay unset when no invoker is configured")
	}

	// Invoker failure aborts the write.
	fail := func(context.Context, string, string, map[string]any) (any, error) {
		return nil, errors.New("boom")
	}
	if err := applyFormulas(context.Background(), fail, fb, map[string]any{}, nil); err == nil {
		t.Error("invoker error must propagate")
	}
}
