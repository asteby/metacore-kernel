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

	// A product of another org is invisible AND the write is refused: leaving
	// the targets empty would skip the price floor (shortfall = -price).
	row = map[string]any{"product_id": "p-other", "unit_price": 10.0, "min_price_at_sale": 0.0}
	if err := st.apply(context.Background(), db, org, uuid.New(), row, noConsume); !errors.Is(err, ErrSnapshotSource) {
		t.Fatalf("origen de otra org debe rechazar la escritura: %v", err)
	}
	if _, ok := row["unit_cost"]; ok {
		t.Fatalf("otra org no debe copiarse: %v", row)
	}
	// Nonexistent id: same refusal.
	row = map[string]any{"product_id": "nope", "unit_price": 10.0}
	if err := st.apply(context.Background(), db, org, uuid.New(), row, noConsume); !errors.Is(err, ErrSnapshotSource) {
		t.Fatalf("id inexistente: %v", err)
	}
	// Soft-deleted source: not a source.
	db.Exec(`UPDATE stamp_products SET deleted_at = CURRENT_TIMESTAMP WHERE id = 'p1'`)
	row = map[string]any{"product_id": "p1", "unit_price": 10.0}
	if err := st.apply(context.Background(), db, org, uuid.New(), row, noConsume); !errors.Is(err, ErrSnapshotSource) {
		t.Fatalf("origen borrado: %v", err)
	}
}

func TestStampGrantRedeemedOnlyWhenNeeded(t *testing.T) {
	db, org, _ := stampDB(t)
	st := itemStamps()
	actor, grant := uuid.New(), uuid.New()
	var got []ConsumeInput
	consume := func(_ context.Context, in ConsumeInput) error {
		if !in.DryRun {
			got = append(got, in)
		}
		return nil
	}

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

// Multi-org: the same product id exists in two orgs with different costs. Each
// writer gets ONLY its own org's value, and a foreign id copies nothing.
func TestStampSnapshotMultiOrgNeverCrossesTenants(t *testing.T) {
	db := setupTestDB(t)
	if err := db.Exec(`CREATE TABLE stamp_products (
		id TEXT, organization_id TEXT, min_price REAL, cost_price REAL, deleted_at DATETIME)`).Error; err != nil {
		t.Fatal(err)
	}
	a, b := uuid.New(), uuid.New()
	for _, r := range []struct {
		id   string
		org  uuid.UUID
		cost float64
	}{{"shared", a, 10}, {"shared", b, 777}, {"only-b", b, 888}} {
		if err := db.Exec(`INSERT INTO stamp_products (id, organization_id, min_price, cost_price) VALUES (?,?,?,?)`,
			r.id, r.org.String(), 1, r.cost).Error; err != nil {
			t.Fatal(err)
		}
	}
	st := itemStamps()
	noConsume := func(context.Context, ConsumeInput) error { return nil }
	for org, want := range map[uuid.UUID]float64{a: 10, b: 777} {
		row := map[string]any{"product_id": "shared", "unit_price": 5.0}
		if err := st.apply(context.Background(), db, org, uuid.New(), row, noConsume); err != nil {
			t.Fatal(err)
		}
		if row["unit_cost"] != want {
			t.Fatalf("org %s: unit_cost=%v, quiero %v", org, row["unit_cost"], want)
		}
	}
	// Org A referencing a product that only exists in org B: nothing is copied,
	// even if the client forged the target.
	row := map[string]any{"product_id": "only-b", "unit_price": 5.0, "unit_cost": 1.0}
	if err := st.apply(context.Background(), db, a, uuid.New(), row, noConsume); !errors.Is(err, ErrSnapshotSource) {
		t.Fatalf("producto de otra org: %v", err)
	}
}

// Without a tenant, or with a source table that cannot be scoped, the stamp
// refuses to run instead of copying unscoped.
func TestStampSnapshotFailsClosedWithoutTenantScope(t *testing.T) {
	db, org, _ := stampDB(t)
	st := itemStamps()
	noConsume := func(context.Context, ConsumeInput) error { return nil }
	row := map[string]any{"product_id": "p-other", "unit_price": 5.0}
	if err := st.apply(context.Background(), db, uuid.Nil, uuid.New(), row, noConsume); !errors.Is(err, ErrTenantScopeUnavailable) {
		t.Fatalf("org nula debe fallar cerrado: %v", err)
	}
	if _, ok := row["unit_cost"]; ok {
		t.Fatalf("no debe copiar sin org: %v", row)
	}

	if err := db.Exec(`CREATE TABLE stamp_global (id TEXT PRIMARY KEY, cost_price REAL)`).Error; err != nil {
		t.Fatal(err)
	}
	db.Exec(`INSERT INTO stamp_global VALUES ('g1', 5)`)
	st.RefTables["product_id"] = "stamp_global"
	row = map[string]any{"product_id": "g1", "unit_price": 5.0}
	if err := st.apply(context.Background(), db, org, uuid.New(), row, noConsume); err == nil {
		t.Fatalf("tabla sin organization_id debe rechazarse; row=%v", row)
	}
}

func addProduct(t *testing.T, db *gorm.DB, id string, org uuid.UUID, min, cost float64) {
	t.Helper()
	if err := db.Exec(`INSERT INTO stamp_products (id, organization_id, min_price, cost_price) VALUES (?,?,?,?)`,
		id, org.String(), min, cost).Error; err != nil {
		t.Fatal(err)
	}
}

// UPDATE: a changed snapshot source is re-snapshotted; a forged target in the
// patch is replaced; a source of another org refuses the write.
func TestStampUpdateRetakesSnapshotOnSourceChange(t *testing.T) {
	db, org, _ := stampDB(t)
	addProduct(t, db, "p2", org, 80, 55)
	st := itemStamps()
	noConsume := func(context.Context, ConsumeInput) error { return nil }
	ctx := context.Background()
	before := map[string]any{"product_id": "p1", "unit_price": 90.0, "min_price_at_sale": 50.0, "unit_cost": 30.0, "price_authorized": 0.0}

	patch := map[string]any{"product_id": "p2", "unit_cost": 1.0, "min_price_at_sale": 0.0}
	pend, err := st.prepareUpdate(ctx, db, org, uuid.New(), "items", "line1", before, patch, nil, noConsume)
	if err != nil || len(pend) != 0 {
		t.Fatalf("pend=%v err=%v", pend, err)
	}
	if patch["min_price_at_sale"] != 80.0 || patch["unit_cost"] != 55.0 {
		t.Fatalf("re-snapshot: %v", patch)
	}
	// 90 over the new floor of 80 -> headroom.
	if patch["price_shortfall"] != -10.0 {
		t.Fatalf("derivada: %v", patch)
	}

	patch = map[string]any{"product_id": "p-other"}
	if _, err := st.prepareUpdate(ctx, db, org, uuid.New(), "items", "line1", before, patch, nil, noConsume); !errors.Is(err, ErrSnapshotSource) {
		t.Fatalf("origen de otra org: %v", err)
	}
}

// The bypass: authorized at create (price_authorized=1), then PATCH unit_price.
// The authorization is dropped, the shortfall reappears and a NEW grant is needed.
func TestStampUpdateDropsAuthorizationWhenCoveredColumnChanges(t *testing.T) {
	db, org, _ := stampDB(t)
	st := itemStamps()
	ctx := context.Background()
	actor, newGrant := uuid.New(), uuid.New()
	var dry, spent []ConsumeInput
	consume := func(_ context.Context, in ConsumeInput) error {
		if in.DryRun {
			dry = append(dry, in)
		} else {
			spent = append(spent, in)
		}
		return nil
	}
	// Created at 45 under a floor of 50 with a grant: flag 1, shortfall 0.
	before := map[string]any{"product_id": "p1", "unit_price": 45.0, "min_price_at_sale": 50.0, "unit_cost": 30.0,
		"price_authorized": 1.0, "price_shortfall": 0.0, "price_approval_id": uuid.NewString()}

	// PATCH unit_price=1 with the old row's flag: no PIN, no free pass.
	patch := map[string]any{"unit_price": 1.0, "price_authorized": 1.0}
	pend, err := st.prepareUpdate(ctx, db, org, actor, "items", "line1", before, patch, nil, consume)
	if err != nil {
		t.Fatal(err)
	}
	if len(pend) != 0 || len(dry) != 0 {
		t.Fatalf("el id viejo no debe reutilizarse: pend=%v dry=%v", pend, dry)
	}
	if patch["price_authorized"] != 0.0 || patch["price_shortfall"] != 49.0 {
		t.Fatalf("la autorización debe caer y el faltante reaparecer: %v", patch)
	}

	// With a NEW grant in the patch it is validated (not spent) and the flag opens.
	patch = map[string]any{"unit_price": 1.0, "price_approval_id": newGrant.String()}
	pend, err = st.prepareUpdate(ctx, db, org, actor, "items", "line1", before, patch, nil, consume)
	if err != nil || len(pend) != 1 || len(spent) != 0 || len(dry) != 1 {
		t.Fatalf("pend=%v spent=%v dry=%v err=%v", pend, spent, dry, err)
	}
	if dry[0].ApprovalID != newGrant || dry[0].ContextGte["unit_shortfall"] != 49 || dry[0].RecordID != "line1" {
		t.Fatalf("dry = %+v", dry[0])
	}
	if patch["price_authorized"] != 1.0 || patch["price_shortfall"] != 0.0 {
		t.Fatalf("patch = %v", patch)
	}
	if err := pend.Redeem(ctx, db, consume); err != nil || len(spent) != 1 || spent[0].DryRun {
		t.Fatalf("spent=%v err=%v", spent, err)
	}

	// An additive delta on unit_price counts as a change too.
	patch, inc := map[string]any{}, map[string]any{"unit_price": -44.0}
	if _, err := st.prepareUpdate(ctx, db, org, actor, "items", "line1", before, patch, inc, consume); err != nil {
		t.Fatal(err)
	}
	if patch["unit_price"] != 1.0 || len(inc) != 0 || patch["price_authorized"] != 0.0 {
		t.Fatalf("inc: patch=%v inc=%v", patch, inc)
	}
}

// A patch that touches nothing a grant covered keeps the authorization, and the
// client cannot write the server-owned columns.
func TestStampUpdateUnrelatedPatchKeepsAuthorization(t *testing.T) {
	db, org, _ := stampDB(t)
	st := itemStamps()
	before := map[string]any{"product_id": "p1", "unit_price": 45.0, "min_price_at_sale": 50.0, "price_authorized": 1.0}
	patch := map[string]any{"quantity": 3.0, "price_authorized": 1.0, "unit_cost": 0.0, "min_price_at_sale": 0.0}
	pend, err := st.prepareUpdate(context.Background(), db, org, uuid.New(), "items", "l", before, patch,
		nil, func(context.Context, ConsumeInput) error { t.Fatal("no debe canjear"); return nil })
	if err != nil || len(pend) != 0 {
		t.Fatal(pend, err)
	}
	for _, k := range []string{"price_authorized", "unit_cost", "min_price_at_sale", "price_shortfall"} {
		if _, ok := patch[k]; ok {
			t.Errorf("%s no debe venir del cliente ni tocarse: %v", k, patch)
		}
	}
	if patch["quantity"] != 3.0 {
		t.Fatalf("patch = %v", patch)
	}
}

// context_gte is evaluated on the row AFTER the derived columns are recomputed:
// a gte that reads a derived column sees its real (uncovered) value.
func TestStampGrantContextGteSeesDerivedColumns(t *testing.T) {
	db, org, _ := stampDB(t)
	cols := []manifest.ColumnDef{
		{Name: "product_id", Ref: "stamp_products"},
		{Name: "price_approval_id", Grant: &manifest.GrantDef{Policy: "price_below_min", Sets: "price_authorized",
			ContextEq:  map[string]string{"product_id": "product_id"},
			ContextGte: map[string]string{"unit_shortfall": "price_shortfall"}}},
	}
	st := NewModelStamps(cols,
		[]manifest.SnapshotDef{{From: "product_id", Fields: map[string]string{"min_price": "min_price_at_sale"}}},
		[]manifest.Formula{{Target: "price_shortfall", Expr: "(min_price_at_sale - unit_price) * (1 - price_authorized)"}})
	var dry ConsumeInput
	consume := func(_ context.Context, in ConsumeInput) error { dry = in; return nil }
	row := map[string]any{"product_id": "p1", "unit_price": 40.0, "price_approval_id": uuid.NewString(), "price_authorized": 1.0}
	if _, err := st.prepare(context.Background(), db, org, uuid.New(), "items", row, "", consume); err != nil {
		t.Fatal(err)
	}
	if dry.ContextGte["unit_shortfall"] != 10 {
		t.Fatalf("gte debe ver el faltante real: %v", dry.ContextGte)
	}
	if row["price_shortfall"] != 0.0 || row["price_authorized"] != 1.0 {
		t.Fatalf("row = %v", row)
	}
}

// With the real ConsumeApproval: preparing a create spends nothing (a guard that
// refuses or parks the write burns no grant, and the replay can still redeem),
// and redeeming on a transaction that rolls back leaves the grant usable.
func TestStampGrantSpentOnlyOnRedeemAndRollsBack(t *testing.T) {
	svc, cashier, _ := pinFixture(t)
	db := svc.db
	if err := db.Exec(`CREATE TABLE stamp_products (
		id TEXT PRIMARY KEY, organization_id TEXT, min_price REAL, cost_price REAL, deleted_at DATETIME)`).Error; err != nil {
		t.Fatal(err)
	}
	addProduct(t, db, "p1", cashier.orgID, 50, 30)
	svc.stamps = func(context.Context, string) (*ModelStamps, bool) { return itemStamps(), true }
	ctx := context.Background()
	grant, err := svc.GrantPINApproval(ctx, cashier, PINGrantInput{PolicyKey: "price_below_min", Reason: "r", PIN: "4321",
		Context: map[string]any{"product_id": "p1", "unit_shortfall": 10.0}})
	if err != nil {
		t.Fatal(err)
	}
	mk := func() map[string]any {
		return map[string]any{"product_id": "p1", "unit_price": 40.0, "price_approval_id": grant.ID.String()}
	}

	// Validated twice (create + replay of a parked request): never "already used".
	for i := 0; i < 2; i++ {
		row := mk()
		if _, err := svc.StampCreatePrepare(ctx, db, cashier.orgID, cashier.id, "items", row); err != nil {
			t.Fatalf("prepare #%d: %v", i, err)
		}
		if row["price_authorized"] != 1.0 {
			t.Fatalf("flag: %v", row)
		}
	}

	// Redeem on a transaction that rolls back: the grant survives.
	pend, _ := svc.StampCreatePrepare(ctx, db, cashier.orgID, cashier.id, "items", mk())
	tx := db.Begin()
	if err := pend.Redeem(ctx, tx, svc.ConsumeApproval); err != nil {
		t.Fatal(err)
	}
	tx.Rollback()

	// Committed: spent exactly once.
	tx = db.Begin()
	if err := pend.Redeem(ctx, tx, svc.ConsumeApproval); err != nil {
		t.Fatalf("tras el rollback sigue vigente: %v", err)
	}
	tx.Commit()
	if _, err := svc.StampCreatePrepare(ctx, db, cashier.orgID, cashier.id, "items", mk()); !errors.Is(err, ErrApprovalConsume) {
		t.Fatalf("ya gastada: %v", err)
	}

	// A grant of another user in the same org cannot be redeemed through the stamp.
	g2, _ := svc.GrantPINApproval(ctx, cashier, PINGrantInput{PolicyKey: "price_below_min", Reason: "r", PIN: "4321",
		Context: map[string]any{"product_id": "p1", "unit_shortfall": 10.0}})
	row := map[string]any{"product_id": "p1", "unit_price": 40.0, "price_approval_id": g2.ID.String()}
	if _, err := svc.StampCreatePrepare(ctx, db, cashier.orgID, uuid.New(), "items", row); !errors.Is(err, ErrApprovalConsume) {
		t.Fatalf("otro solicitante: %v", err)
	}
}

// Through Service.Update: the snapshot target cannot be forged with a PATCH and
// follows its source when the source changes.
func TestServiceUpdateRunsStampPhase(t *testing.T) {
	db, org, _ := stampDB(t)
	addProduct(t, db, "p2", org, 80, 55)
	modelbase.Register("test_products", func() modelbase.ModelDefiner { return &TestProduct{} })
	st := NewModelStamps(
		[]manifest.ColumnDef{{Name: "name", Ref: "stamp_products"}},
		[]manifest.SnapshotDef{{From: "name", Fields: map[string]string{"min_price": "price"}}},
		nil,
	)
	svc := New(Config{
		DB: db, Metadata: metadata.New(metadata.Config{CacheTTL: -1}),
		StampResolver: func(_ context.Context, model string) (*ModelStamps, bool) { return st, model == "test_products" },
	})
	ctx := context.Background()
	u := newUser(org)
	out, err := svc.Create(ctx, "test_products", u, map[string]any{"name": "p1", "price": 999.0})
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.MustParse(out["id"].(string))

	// Forging the frozen column with a PATCH is discarded.
	got, err := svc.Update(ctx, "test_products", u, id, map[string]any{"price": 1.0})
	if err != nil {
		t.Fatal(err)
	}
	if got["price"] != 50.0 {
		t.Fatalf("price forjado: %v", got["price"])
	}
	// Changing the source re-takes the snapshot.
	got, err = svc.Update(ctx, "test_products", u, id, map[string]any{"name": "p2", "price": 1.0})
	if err != nil {
		t.Fatal(err)
	}
	if got["price"] != 80.0 {
		t.Fatalf("re-snapshot: %v", got["price"])
	}
	// A source of another org refuses the write and leaves the row as it was.
	if _, err = svc.Update(ctx, "test_products", u, id, map[string]any{"name": "p-other"}); !errors.Is(err, ErrSnapshotSource) {
		t.Fatalf("origen ajeno: %v", err)
	}
}

// Un grant gastado se limpia con NULL (nil), nunca con "": la columna puede ser
// uuid y "" no es un uuid válido.
func TestStampUpdateClearsSpentGrantWithNil(t *testing.T) {
	db, org, _ := stampDB(t)
	st := itemStamps()
	consume := func(context.Context, ConsumeInput) error { return nil }
	before := map[string]any{"product_id": "p1", "unit_price": 45.0, "min_price_at_sale": 50.0, "unit_cost": 30.0,
		"price_authorized": 1.0, "price_shortfall": 0.0, "price_approval_id": uuid.NewString()}
	patch := map[string]any{"unit_price": 60.0}
	if _, err := st.prepareUpdate(context.Background(), db, org, uuid.New(), "items", "line1", before, patch, nil, consume); err != nil {
		t.Fatal(err)
	}
	v, ok := patch["price_approval_id"]
	if !ok || v != nil {
		t.Fatalf("el grant gastado debe ir como nil en el patch, got %#v (present=%v)", v, ok)
	}
}
