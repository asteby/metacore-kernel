package dynamic

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"gorm.io/gorm"

	v3 "github.com/asteby/metacore-kernel/manifest/v3"
	"github.com/asteby/metacore-kernel/modelbase"
)

// Declarative option display (v3 models[].option_display + option_metrics[]):
// the options endpoint serves each option a ready-to-paint display — own
// columns, contributed metrics resolved in ONE grouped query per page, tones,
// badges — scoped by org and by the picker context.

type optdProduct struct {
	modelbase.BaseUUIDModel
	Name        string   `json:"name"`
	Sku         string   `json:"sku"`
	SizeCode    *string  `json:"size_code"`
	UnitPrice   *float64 `json:"unit_price"`
	MinStock    *float64 `json:"min_stock"`
	Image       string   `json:"image"`
	ProductType string   `json:"product_type"`
}

func (optdProduct) TableName() string                    { return "test_optd_products" }
func (optdProduct) DefineTable() modelbase.TableMetadata { return modelbase.TableMetadata{} }
func (optdProduct) DefineModal() modelbase.ModalMetadata { return modelbase.ModalMetadata{} }

type optdStock struct {
	modelbase.BaseUUIDModel
	ProductID   uuid.UUID `json:"product_id"`
	WarehouseID uuid.UUID `json:"warehouse_id"`
	Available   float64   `json:"available"`
}

func (optdStock) TableName() string                    { return "test_optd_stock" }
func (optdStock) DefineTable() modelbase.TableMetadata { return modelbase.TableMetadata{} }
func (optdStock) DefineModal() modelbase.ModalMetadata { return modelbase.ModalMetadata{} }

type optdWarehouse struct {
	modelbase.BaseUUIDModel
	Name     string    `json:"name"`
	BranchID uuid.UUID `json:"branch_id"`
}

func (optdWarehouse) TableName() string                    { return "test_optd_warehouses" }
func (optdWarehouse) DefineTable() modelbase.TableMetadata { return modelbase.TableMetadata{} }
func (optdWarehouse) DefineModal() modelbase.ModalMetadata { return modelbase.ModalMetadata{} }

type optdFixture struct {
	svc                   *Service
	db                    *gorm.DB
	org, otherOrg         uuid.UUID
	tire, oil, svcProduct uuid.UUID
	whA, whB              uuid.UUID
	branch                uuid.UUID
}

func productDisplay() *v3.OptionDisplay {
	return &v3.OptionDisplay{
		Title:    "name",
		Subtitle: []string{"sku", "Medida {size_code}"},
		Image:    "image",
		Trailing: []v3.OptionTrailing{
			{Key: "price", Label: "Precio", Field: "unit_price", Format: "money"},
			{Key: "stock", Label: "Disp.", Metric: "stock_available", Format: "number", Tones: []v3.OptionTone{
				{When: v3.OptionDisplayCondition{Op: "lte", Value: 0}, Tone: "danger", Text: "Agotado", Dim: true},
				{When: v3.OptionDisplayCondition{Op: "lte", Ref: "min_stock"}, Tone: "warning"},
				{When: v3.OptionDisplayCondition{Op: "gt", Value: 0}, Tone: "success"},
			}},
			{Key: "lead", Metric: "lead_time_days", Format: "integer"},
		},
		Badges: []v3.OptionBadge{
			{Field: "product_type", When: &v3.OptionDisplayCondition{Op: "eq", Value: "service"}, Text: "Servicio", Tone: "info"},
		},
	}
}

func stockMetric() v3.OptionMetric {
	return v3.OptionMetric{
		Key: "stock_available", Target: "products.Product", Model: "test_optd_stock",
		ForeignKey: "product_id", Aggregate: "sum", Column: "available",
		Scope: []v3.OptionMetricScope{
			{Context: "warehouse_id", Column: "warehouse_id"},
			{Context: "branch_id", Column: "warehouse_id", Through: &v3.OptionMetricThrough{Model: "test_optd_warehouses", Column: "branch_id"}},
		},
	}
}

func newOptdFixture(t *testing.T, metrics []v3.OptionMetric) *optdFixture {
	t.Helper()
	db := setupTestDB(t)
	ddls := []string{
		`CREATE TABLE test_optd_products (id TEXT PRIMARY KEY, organization_id TEXT, created_by_id TEXT, created_at DATETIME, updated_at DATETIME, deleted_at DATETIME,
			name TEXT, sku TEXT, size_code TEXT, unit_price REAL, min_stock REAL, image TEXT, product_type TEXT)`,
		`CREATE TABLE test_optd_stock (id TEXT PRIMARY KEY, organization_id TEXT, created_by_id TEXT, created_at DATETIME, updated_at DATETIME, deleted_at DATETIME,
			product_id TEXT, warehouse_id TEXT, available REAL)`,
		`CREATE TABLE test_optd_warehouses (id TEXT PRIMARY KEY, organization_id TEXT, created_by_id TEXT, created_at DATETIME, updated_at DATETIME, deleted_at DATETIME,
			name TEXT, branch_id TEXT)`,
	}
	return newOptdFixtureOn(t, db, ddls, func(n string) string { return n }, metrics)
}

// newOptdFixtureOn seeds the fixture on db; table maps a model name to its
// (possibly schema-qualified) table.
func newOptdFixtureOn(t *testing.T, db *gorm.DB, ddls []string, table func(string) string, metrics []v3.OptionMetric) *optdFixture {
	t.Helper()
	for _, ddl := range ddls {
		if err := db.Exec(ddl).Error; err != nil {
			t.Fatal(err)
		}
	}
	modelbase.Register("test_optd_products", func() modelbase.ModelDefiner { return &optdProduct{} })
	modelbase.Register("test_optd_stock", func() modelbase.ModelDefiner { return &optdStock{} })
	modelbase.Register("test_optd_warehouses", func() modelbase.ModelDefiner { return &optdWarehouse{} })

	f := &optdFixture{db: db, org: uuid.New(), otherOrg: uuid.New(),
		tire: uuid.New(), oil: uuid.New(), svcProduct: uuid.New(),
		whA: uuid.New(), whB: uuid.New(), branch: uuid.New()}
	exec := func(q string, args ...any) {
		t.Helper()
		if err := db.Exec(q, args...).Error; err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO `+table("test_optd_products")+` (id, organization_id, name, sku, size_code, unit_price, min_stock, image, product_type) VALUES
		(?, ?, 'Llanta 205/55R16', 'MIC-2055516', '205/55R16', 1899.5, 4, 'https://img/tire.png', 'product'),
		(?, ?, 'Aceite 5W30', 'ACE-530', NULL, 250, 2, '', 'product'),
		(?, ?, 'Alineación', 'SRV-ALI', NULL, 400, NULL, '', 'service')`,
		f.tire, f.org, f.oil, f.org, f.svcProduct, f.org)
	exec(`INSERT INTO `+table("test_optd_warehouses")+` (id, organization_id, name, branch_id) VALUES (?, ?, 'A', ?), (?, ?, 'B', ?)`,
		f.whA, f.org, f.branch, f.whB, f.org, uuid.New())
	exec(`INSERT INTO `+table("test_optd_stock")+` (id, organization_id, product_id, warehouse_id, available, deleted_at) VALUES
		(?, ?, ?, ?, 3, NULL),
		(?, ?, ?, ?, 5, NULL),
		(?, ?, ?, ?, 100, NULL),
		(?, ?, ?, ?, 50, '2026-01-01 00:00:00')`,
		uuid.New(), f.org, f.tire, f.whA,
		uuid.New(), f.org, f.tire, f.whB,
		uuid.New(), f.otherOrg, f.tire, f.whA, // other org: never counted
		uuid.New(), f.org, f.oil, f.whA) // soft-deleted: never counted

	svc := newOptionsService(t, db, func(context.Context, string, any) (*OptionsConfig, error) {
		return nil, ErrNoOptionsConfig
	}, nil)
	svc.selfOptions = true
	svc.tableNameResolver = func(_ context.Context, model string) (string, bool) {
		if strings.HasPrefix(model, "test_optd_") {
			return table(model), true
		}
		return "", false
	}
	svc.optionDisplays = func(_ context.Context, model string) (OptionDisplaySpec, bool) {
		if model != "test_optd_products" {
			return OptionDisplaySpec{}, false
		}
		return OptionDisplaySpec{Display: productDisplay(), Metrics: metrics}, true
	}
	f.svc = svc
	return f
}

func (f *optdFixture) options(t *testing.T, q OptionsQuery) map[string]Option {
	t.Helper()
	q.Model, q.Field = "test_optd_products", "id"
	res, err := f.svc.Options(context.Background(), newUser(f.org), q)
	if err != nil {
		t.Fatalf("options: %v", err)
	}
	out := map[string]Option{}
	for _, o := range res.Options {
		out[optionKey(o.Value)] = o
	}
	return out
}

func trailingByKey(d *OptionDisplayValue) map[string]OptionTrailingItem {
	out := map[string]OptionTrailingItem{}
	if d == nil {
		return out
	}
	for _, t := range d.Trailing {
		out[t.Key] = t
	}
	return out
}

func TestOptionDisplay_ResolvesColumnsMetricsTonesBadges(t *testing.T) {
	f := newOptdFixture(t, []v3.OptionMetric{stockMetric()})
	opts := f.options(t, OptionsQuery{})

	tire := opts[f.tire.String()].Display
	if tire == nil {
		t.Fatalf("tire has no display")
	}
	if tire.Title != "Llanta 205/55R16" || tire.Subtitle != "MIC-2055516 · Medida 205/55R16" {
		t.Errorf("title/subtitle = %q / %q", tire.Title, tire.Subtitle)
	}
	if tire.Image != "https://img/tire.png" {
		t.Errorf("image = %v", tire.Image)
	}
	tr := trailingByKey(tire)
	if tr["price"].Value != 1899.5 || tr["price"].Format != "money" {
		t.Errorf("price = %+v", tr["price"])
	}
	// 3 + 5 in the org; the other org's 100 is never counted.
	if tr["stock"].Value != float64(8) || tr["stock"].Tone != "success" {
		t.Errorf("stock = %+v, want 8 success", tr["stock"])
	}
	if _, ok := tr["lead"]; ok {
		t.Errorf("an unprovided metric must be omitted: %+v", tr["lead"])
	}
	if tire.Dimmed || tire.Blocked || tire.Tone != "success" {
		t.Errorf("row flags = %+v", tire)
	}

	// Oil: its only stock row is soft-deleted → 0 → Agotado, dimmed row.
	oil := opts[f.oil.String()].Display
	st := trailingByKey(oil)["stock"]
	if st.Value != float64(0) || st.Tone != "danger" || st.Text != "Agotado" || !oil.Dimmed || oil.Blocked {
		t.Errorf("oil stock = %+v dimmed=%v blocked=%v", st, oil.Dimmed, oil.Blocked)
	}
	if oil.Subtitle != "ACE-530" {
		t.Errorf("an empty template part must drop: %q", oil.Subtitle)
	}

	svcOpt := opts[f.svcProduct.String()].Display
	if len(svcOpt.Badges) != 1 || svcOpt.Badges[0].Text != "Servicio" || svcOpt.Badges[0].Tone != "info" {
		t.Errorf("badges = %+v", svcOpt.Badges)
	}
	if len(oil.Badges) != 0 {
		t.Errorf("badge gated by when leaked: %+v", oil.Badges)
	}
}

func TestOptionDisplay_WarningAgainstColumnRef(t *testing.T) {
	f := newOptdFixture(t, []v3.OptionMetric{stockMetric()})
	// Warehouse A only: tire has 3 ≤ min_stock 4 → warning.
	opts := f.options(t, OptionsQuery{Context: map[string]string{"warehouse_id": f.whA.String()}})
	st := trailingByKey(opts[f.tire.String()].Display)["stock"]
	if st.Value != float64(3) || st.Tone != "warning" {
		t.Fatalf("stock in A = %+v, want 3 warning", st)
	}
}

func TestOptionDisplay_ScopeThroughBranch(t *testing.T) {
	f := newOptdFixture(t, []v3.OptionMetric{stockMetric()})
	opts := f.options(t, OptionsQuery{Context: map[string]string{"branch_id": f.branch.String()}})
	if v := trailingByKey(opts[f.tire.String()].Display)["stock"].Value; v != float64(3) {
		t.Fatalf("stock in branch (warehouse A only) = %v, want 3", v)
	}
}

func TestOptionDisplay_InvalidContextOmitsMetricNotError(t *testing.T) {
	f := newOptdFixture(t, []v3.OptionMetric{stockMetric()})
	opts := f.options(t, OptionsQuery{Context: map[string]string{"warehouse_id": "not-a-uuid"}})
	if _, ok := trailingByKey(opts[f.tire.String()].Display)["stock"]; ok {
		t.Fatal("a context that cannot match must omit the metric, not widen it")
	}
	if _, ok := trailingByKey(opts[f.tire.String()].Display)["price"]; !ok {
		t.Fatal("column metrics must survive")
	}
}

func TestOptionDisplay_WithoutProviderStockIsOmitted(t *testing.T) {
	f := newOptdFixture(t, nil) // inventory not installed/enabled
	opts := f.options(t, OptionsQuery{})
	tr := trailingByKey(opts[f.tire.String()].Display)
	if _, ok := tr["stock"]; ok {
		t.Fatal("stock must be omitted without a provider")
	}
	if tr["price"].Value != 1899.5 {
		t.Fatalf("price = %+v", tr["price"])
	}
	if opts[f.oil.String()].Display.Dimmed {
		t.Fatal("no stock metric → no dimming")
	}
}

func TestOptionDisplay_IDsResolveModeCarriesDisplay(t *testing.T) {
	f := newOptdFixture(t, []v3.OptionMetric{stockMetric()})
	opts := f.options(t, OptionsQuery{IDs: []string{f.tire.String()}})
	if len(opts) != 1 {
		t.Fatalf("want 1 option, got %d", len(opts))
	}
	if v := trailingByKey(opts[f.tire.String()].Display)["stock"].Value; v != float64(8) {
		t.Fatalf("resolve mode stock = %v", v)
	}
}

func TestOptionDisplay_OneQueryPerMetricNotPerOption(t *testing.T) {
	f := newOptdFixture(t, []v3.OptionMetric{stockMetric()})
	var stockQueries int32
	_ = f.db.Callback().Query().Before("gorm:query").Register("optd:count", func(tx *gorm.DB) {
		if strings.Contains(tx.Statement.Table, "test_optd_stock") {
			atomic.AddInt32(&stockQueries, 1)
		}
	})
	// Row callback for Scan with Table() goes through Row, count both.
	_ = f.db.Callback().Row().Before("gorm:row").Register("optd:countrow", func(tx *gorm.DB) {
		if strings.Contains(tx.Statement.Table, "test_optd_stock") {
			atomic.AddInt32(&stockQueries, 1)
		}
	})
	opts := f.options(t, OptionsQuery{})
	if len(opts) != 3 {
		t.Fatalf("want 3 options, got %d", len(opts))
	}
	if n := atomic.LoadInt32(&stockQueries); n != 1 {
		t.Fatalf("stock queries = %d, want exactly 1 for the whole page", n)
	}
}

func TestOptionDisplay_NoResolverLeavesOptionsUntouched(t *testing.T) {
	f := newOptdFixture(t, nil)
	f.svc.optionDisplays = nil
	for _, o := range f.options(t, OptionsQuery{}) {
		if o.Display != nil {
			t.Fatalf("display without resolver: %+v", o.Display)
		}
		b, _ := json.Marshal(o)
		if strings.Contains(string(b), `"display"`) {
			t.Fatalf("display key serialized: %s", b)
		}
	}
}

func TestOptionContextFromQuery(t *testing.T) {
	app := fiber.New()
	var got map[string]string
	app.Get("/", func(c fiber.Ctx) error {
		got = OptionContextFromQuery(c)
		return c.SendStatus(200)
	})
	long := strings.Repeat("x", maxOptionContextValue+1)
	req := httptest.NewRequest("GET", "/?ctx.warehouse_id=w1&ctx%5Bbranch_id%5D=b1&ctx.Bad=x&ctx.empty=&ctx.long="+long+"&other=1", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	if len(got) != 2 || got["warehouse_id"] != "w1" || got["branch_id"] != "b1" {
		t.Fatalf("context = %v", got)
	}
}

func TestEvalOptionCondition(t *testing.T) {
	cases := []struct {
		op       string
		lhs, rhs any
		want     bool
	}{
		{"lte", float64(0), 0, true},
		{"lte", "3", 4.0, true},
		{"gt", int64(5), "4.5", true},
		{"eq", "Accepted", "accepted", true},
		{"neq", "draft", "accepted", true},
		{"lt", "2026-01-01T00:00:00Z", "2026-02-01T00:00:00Z", true},
		{"empty", nil, nil, true},
		{"empty", " ", nil, true},
		{"not_empty", "x", nil, true},
		{"lt", nil, 3, false},
		{"eq", true, "true", true},
	}
	for _, c := range cases {
		if got := evalOptionCondition(c.op, c.lhs, c.rhs); got != c.want {
			t.Errorf("%s(%v, %v) = %v, want %v", c.op, c.lhs, c.rhs, got, c.want)
		}
	}
}
