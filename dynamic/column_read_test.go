package dynamic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
	"github.com/asteby/metacore-kernel/metadata"
	"github.com/asteby/metacore-kernel/modelbase"
	"github.com/asteby/metacore-kernel/permission"
	"github.com/asteby/metacore-kernel/query"
)

// ---------------------------------------------------------------------------
// Models: a catalogue item whose cost only some roles may read, and its
// supplier whose rebate is gated by another permission.
// ---------------------------------------------------------------------------

const (
	permViewCost   = "inventory.product.view_cost"
	permViewRebate = "inventory.supplier.view_rebate"
)

type crSupplier struct {
	modelbase.BaseUUIDModel
	Name   string  `json:"name"`
	Rebate float64 `json:"rebate"`
}

func (crSupplier) TableName() string { return "cr_suppliers" }
func (crSupplier) DefineTable() modelbase.TableMetadata {
	return modelbase.TableMetadata{Title: "Suppliers", SearchColumns: []string{"name", "rebate"}, Columns: []modelbase.ColumnDef{
		{Key: "name"},
		{Key: "rebate", ReadPermission: permViewRebate},
	}}
}
func (crSupplier) DefineModal() modelbase.ModalMetadata {
	return modelbase.ModalMetadata{Title: "Supplier"}
}

type crProduct struct {
	modelbase.BaseUUIDModel
	Name       string      `json:"name"`
	Cost       float64     `json:"cost"`
	Price      float64     `json:"price"`
	SupplierID *uuid.UUID  `json:"supplier_id"`
	Supplier   *crSupplier `json:"supplier,omitempty" gorm:"foreignKey:SupplierID"`
}

func (crProduct) TableName() string { return "cr_products" }
func (crProduct) DefineTable() modelbase.TableMetadata {
	return modelbase.TableMetadata{
		Title:         "Products",
		SearchColumns: []string{"name", "cost"},
		SearchRefs:    []string{"supplier_id"},
		Columns: []modelbase.ColumnDef{
			{Key: "name"},
			{Key: "cost", ReadPermission: permViewCost, StyleConfig: map[string]any{"aggregate": "sum"}},
			{Key: "price", StyleConfig: map[string]any{"aggregate": "sum"}},
			{Key: "supplier_id", Ref: "cr_suppliers"},
		},
	}
}
func (crProduct) DefineModal() modelbase.ModalMetadata {
	return modelbase.ModalMetadata{Title: "Product"}
}
func (crProduct) DefineRelations() []modelbase.RelationDef {
	return []modelbase.RelationDef{{Name: "Supplier", Kind: "belongs_to", Through: "cr_suppliers", ForeignKey: "supplier_id"}}
}

func registerCRModels() {
	modelbase.Register("cr_products", func() modelbase.ModelDefiner { return &crProduct{} })
	modelbase.Register("cr_suppliers", func() modelbase.ModelDefiner { return &crSupplier{} })
}

// capturingBus records every published canonical event.
type capturingBus struct {
	mu     sync.Mutex
	events []*CanonicalEvent
}

func (b *capturingBus) Publish(_ context.Context, _ string, _ string, _ uuid.UUID, payload any) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ce, ok := payload.(*CanonicalEvent); ok {
		b.events = append(b.events, ce)
	}
	return nil
}

type crFixture struct {
	svc      *Service
	db       *gorm.DB
	app      *fiber.App
	bus      *capturingBus
	org      uuid.UUID
	cashier  modelbase.AuthUser
	manager  modelbase.AuthUser
	supplier string
	product  string
}

func newCRFixture(t *testing.T, cfg func(*Config)) *crFixture {
	t.Helper()
	registerCRModels()
	db := setupTestDB(t)
	mustExec(t, db, `CREATE TABLE cr_suppliers (id TEXT PRIMARY KEY, organization_id TEXT, created_by_id TEXT,
		created_at DATETIME, updated_at DATETIME, deleted_at DATETIME, name TEXT, rebate REAL)`)
	mustExec(t, db, `CREATE TABLE cr_products (id TEXT PRIMARY KEY, organization_id TEXT, created_by_id TEXT,
		created_at DATETIME, updated_at DATETIME, deleted_at DATETIME, name TEXT, cost REAL, price REAL, supplier_id TEXT)`)

	crud := func(models ...string) []permission.Capability {
		var out []permission.Capability
		for _, m := range models {
			for _, a := range []string{"read", "create", "update", "delete"} {
				out = append(out, permission.Cap(m, a))
			}
		}
		return out
	}
	store := permission.NewInMemoryStore(map[permission.Role][]permission.Capability{
		"cashier": crud("cr_products", "cr_suppliers"),
		"manager": append(crud("cr_products", "cr_suppliers"), permViewCost, permViewRebate),
	}, nil)
	bus := &capturingBus{}
	c := Config{
		DB:                 db,
		Metadata:           metadata.New(metadata.Config{CacheTTL: -1}),
		Permissions:        permission.New(permission.Config{Store: store}),
		Bus:                bus,
		DisableEventOutbox: true,
		// SQLite has no ILIKE: the explicit clause also drives List's search.
		SearchMatchClause: defaultSearchMatchClause,
	}
	if cfg != nil {
		cfg(&c)
	}
	svc := New(c)
	app := fiber.New()
	NewHandler(svc, headerUserResolver).Mount(app)

	org := uuid.New()
	fx := &crFixture{
		svc: svc, db: db, app: app, bus: bus, org: org,
		cashier: &fakeUser{id: uuid.New(), orgID: org, role: "cashier"},
		manager: &fakeUser{id: uuid.New(), orgID: org, role: "manager"},
	}
	ctx := context.Background()
	sup, err := svc.Create(ctx, "cr_suppliers", fx.manager, map[string]any{"name": "Acme", "rebate": 12.5})
	if err != nil {
		t.Fatalf("seed supplier: %v", err)
	}
	fx.supplier = sup["id"].(string)
	p, err := svc.Create(ctx, "cr_products", fx.manager, map[string]any{"name": "Widget", "cost": 777, "price": 1000, "supplier_id": fx.supplier})
	if err != nil {
		t.Fatalf("seed product: %v", err)
	}
	fx.product = p["id"].(string)
	return fx
}

func (fx *crFixture) get(t *testing.T, role, path string) (int, map[string]any, string) {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set("X-Org", fx.org.String())
	req.Header.Set("X-Role", role)
	resp, err := fx.app.Test(req, fiber.TestConfig{Timeout: 0})
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var env map[string]any
	_ = json.Unmarshal(raw, &env)
	return resp.StatusCode, env, string(raw)
}

func listItems(t *testing.T, env map[string]any) []map[string]any {
	t.Helper()
	raw, _ := env["data"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		m, _ := r.(map[string]any)
		out = append(out, m)
	}
	return out
}

// ---------------------------------------------------------------------------
// List / Get / write responses
// ---------------------------------------------------------------------------

func TestColumnRead_ListHidesColumn(t *testing.T) {
	fx := newCRFixture(t, nil)
	st, env, _ := fx.get(t, "cashier", "/dynamic/cr_products")
	if st != fiber.StatusOK {
		t.Fatalf("cashier list: %d %v", st, env)
	}
	items := listItems(t, env)
	if len(items) != 1 {
		t.Fatalf("cashier list: %v", env)
	}
	if _, ok := items[0]["cost"]; ok {
		t.Fatalf("cashier list leaked cost: %v", items[0])
	}
	if items[0]["price"] == nil || items[0]["name"] != "Widget" {
		t.Fatalf("cashier list lost readable columns: %v", items[0])
	}

	_, env, _ = fx.get(t, "manager", "/dynamic/cr_products")
	if items := listItems(t, env); len(items) != 1 || items[0]["cost"] != float64(777) {
		t.Fatalf("manager list must carry cost: %v", env)
	}
}

func TestColumnRead_GetAndWriteResponsesHideColumn(t *testing.T) {
	fx := newCRFixture(t, nil)
	ctx := context.Background()
	id := uuid.MustParse(fx.product)

	row, err := fx.svc.Get(ctx, "cr_products", fx.cashier, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := row["cost"]; ok {
		t.Fatalf("Get leaked cost: %v", row)
	}
	if row, _ := fx.svc.Get(ctx, "cr_products", fx.manager, id); row["cost"] != float64(777) {
		t.Fatalf("manager Get: %v", row)
	}

	created, err := fx.svc.Create(ctx, "cr_products", fx.cashier, map[string]any{"name": "Gadget", "cost": 5, "price": 9})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := created["cost"]; ok {
		t.Fatalf("Create response leaked cost: %v", created)
	}
	updated, err := fx.svc.Update(ctx, "cr_products", fx.cashier, id, map[string]any{"price": 1100})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := updated["cost"]; ok {
		t.Fatalf("Update response leaked cost: %v", updated)
	}

	// The in-process bus is server-side: every event keeps the full row.
	fx.bus.mu.Lock()
	defer fx.bus.mu.Unlock()
	var sawCashierEvent bool
	for _, ev := range fx.bus.events {
		if ev.ActorID == fx.cashier.GetID().String() && ev.After != nil {
			sawCashierEvent = true
			if _, ok := ev.After["cost"]; !ok {
				t.Fatalf("bus event must keep the full row: %v", ev.After)
			}
		}
	}
	if !sawCashierEvent {
		t.Fatal("no event captured for the cashier's writes")
	}
}

// ---------------------------------------------------------------------------
// Filter / sort / group / aggregate are refused, not ignored
// ---------------------------------------------------------------------------

func TestColumnRead_FilterSortGroupAggregateRejected(t *testing.T) {
	fx := newCRFixture(t, nil)
	for _, path := range []string{
		"/dynamic/cr_products?f_cost=gt:100",
		"/dynamic/cr_products?sortBy=cost&order=asc",
		"/dynamic/cr_products?group_by=cost",
		"/dynamic/cr_products?aggregate=sum(cost)",
		"/dynamic/cr_products?f_supplier.rebate=gt:1",
		"/dynamic/cr_products/export?f_cost=gt:100",
	} {
		st, env, raw := fx.get(t, "cashier", path)
		if st != fiber.StatusForbidden || env["code"] != "column_read_denied" {
			t.Fatalf("cashier %s: %d %s, want 403 column_read_denied", path, st, raw)
		}
		if st, _, raw := fx.get(t, "manager", path); st == fiber.StatusForbidden {
			t.Fatalf("manager %s must be allowed: %s", path, raw)
		}
	}
	_, env, _ := fx.get(t, "cashier", "/dynamic/cr_products?f_cost=gt:100")
	if env["column"] != "cost" || env["model"] != "cr_products" {
		t.Fatalf("error body: %v", env)
	}
}

func TestColumnRead_SearchNeverMatchesHiddenColumn(t *testing.T) {
	fx := newCRFixture(t, nil)
	_, env, _ := fx.get(t, "cashier", "/dynamic/cr_products?search=777")
	if items := listItems(t, env); len(items) != 0 {
		t.Fatalf("cashier search matched the hidden cost: %v", items)
	}
	_, env, _ = fx.get(t, "manager", "/dynamic/cr_products?search=777")
	if items := listItems(t, env); len(items) != 1 {
		t.Fatalf("manager search by cost: %v", env)
	}
	// A readable column still matches.
	_, env, _ = fx.get(t, "cashier", "/dynamic/cr_products?search=Widg")
	if items := listItems(t, env); len(items) != 1 {
		t.Fatalf("cashier search by name: %v", env)
	}
}

func TestColumnRead_AggregateFooterDropsHiddenColumn(t *testing.T) {
	fx := newCRFixture(t, nil)
	_, env, raw := fx.get(t, "cashier", "/dynamic/cr_products/aggregate")
	data, _ := env["data"].(map[string]any)
	if _, ok := data["cost"]; ok {
		t.Fatalf("cashier footer leaked the cost total: %s", raw)
	}
	if _, ok := data["price"]; !ok {
		t.Fatalf("cashier footer lost the price total: %s", raw)
	}
	_, env, raw = fx.get(t, "manager", "/dynamic/cr_products/aggregate")
	if data, _ := env["data"].(map[string]any); data["cost"] == nil {
		t.Fatalf("manager footer: %s", raw)
	}
}

// ---------------------------------------------------------------------------
// Relations / expansions
// ---------------------------------------------------------------------------

func TestColumnRead_PreloadedRelationIsMasked(t *testing.T) {
	fx := newCRFixture(t, nil)
	_, env, raw := fx.get(t, "cashier", "/dynamic/cr_products?with=Supplier")
	items := listItems(t, env)
	if len(items) != 1 {
		t.Fatalf("list: %s", raw)
	}
	sup, ok := items[0]["supplier"].(map[string]any)
	if !ok {
		t.Fatalf("supplier not preloaded: %s", raw)
	}
	if _, leaked := sup["rebate"]; leaked {
		t.Fatalf("cashier received the supplier's hidden rebate: %v", sup)
	}
	if sup["name"] != "Acme" {
		t.Fatalf("supplier lost readable columns: %v", sup)
	}
	_, env, _ = fx.get(t, "manager", "/dynamic/cr_products?with=Supplier")
	if sup, _ := listItems(t, env)[0]["supplier"].(map[string]any); sup["rebate"] != 12.5 {
		t.Fatalf("manager supplier: %v", sup)
	}
}

func TestColumnRead_RelationSearchSkipsHiddenColumn(t *testing.T) {
	fx := newCRFixture(t, nil)
	ctx := context.Background()
	meta, _ := fx.svc.TableMetadata(ctx, "cr_products")
	cols := func(u modelbase.AuthUser) []string {
		return fx.svc.refSearchFor(ctx, meta, "12.5", u)["supplier_id"].Columns
	}
	if got := cols(fx.cashier); len(got) != 1 || got[0] != "name" {
		t.Fatalf("cashier relation search columns: %v", got)
	}
	if got := cols(fx.manager); len(got) != 2 {
		t.Fatalf("manager relation search columns: %v", got)
	}
}

// ---------------------------------------------------------------------------
// Export / facets
// ---------------------------------------------------------------------------

func TestColumnRead_ExportOmitsHiddenColumn(t *testing.T) {
	fx := newCRFixture(t, nil)
	st, _, raw := fx.get(t, "cashier", "/dynamic/cr_products/export?columns=name,cost,price")
	if st != fiber.StatusOK {
		t.Fatalf("export: %d %s", st, raw)
	}
	header := strings.SplitN(strings.TrimPrefix(raw, "\ufeff"), "\n", 2)[0]
	if strings.Contains(strings.ToLower(header), "cost") || strings.Contains(raw, "777") {
		t.Fatalf("cashier export leaked cost: %q", raw)
	}
	_, _, raw = fx.get(t, "manager", "/dynamic/cr_products/export?columns=name,cost,price")
	if !strings.Contains(raw, "777") {
		t.Fatalf("manager export must carry cost: %q", raw)
	}
}

func TestColumnRead_FacetsRejected(t *testing.T) {
	fx := newCRFixture(t, nil)
	st, env, raw := fx.get(t, "cashier", "/dynamic/cr_products/facets?field=cost")
	if st != fiber.StatusForbidden || env["code"] != "column_read_denied" {
		t.Fatalf("cashier facets: %d %s", st, raw)
	}
	if st, _, raw := fx.get(t, "manager", "/dynamic/cr_products/facets?field=name"); st != fiber.StatusOK {
		t.Fatalf("manager facets: %d %s", st, raw)
	}
}

// ---------------------------------------------------------------------------
// Options / search pickers
// ---------------------------------------------------------------------------

func TestColumnRead_OptionsMaskSourceColumns(t *testing.T) {
	fx := newCRFixture(t, func(c *Config) {
		c.OptionsConfigResolver = func(_ context.Context, model string, _ any) (*OptionsConfig, error) {
			return &OptionsConfig{Fields: map[string]FieldOptionsConfig{
				"product_id": {Type: "dynamic", Source: "cr_products", Value: "id", Label: "name", Description: "cost", ExtraColumns: []string{"cost", "price"}},
				"by_cost":    {Type: "dynamic", Source: "cr_products", Value: "id", Label: "name", FilterBy: "cost"},
				"cost_label": {Type: "dynamic", Source: "cr_products", Value: "id", Label: "cost"},
			}}, nil
		}
		c.OptionDisplayResolver = func(_ context.Context, model string) (OptionDisplaySpec, bool) {
			return OptionDisplaySpec{Display: &v3.OptionDisplay{
				Subtitle: []string{"Costo {cost}"},
				Trailing: []v3.OptionTrailing{{Key: "cost", Field: "cost"}, {Key: "price", Field: "price"}},
			}}, true
		}
	})
	ctx := context.Background()
	res, err := fx.svc.Options(ctx, fx.cashier, OptionsQuery{Model: "cr_products", Field: "product_id"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Options) != 1 {
		t.Fatalf("options: %+v", res.Options)
	}
	o := res.Options[0]
	raw, _ := json.Marshal(o)
	if strings.Contains(string(raw), "777") {
		t.Fatalf("cashier option leaked cost: %s", raw)
	}
	if _, ok := o.Extra["price"]; !ok {
		t.Fatalf("cashier option lost the readable extra column: %s", raw)
	}
	if o.Display == nil || len(o.Display.Trailing) != 1 || o.Display.Trailing[0].Key != "price" {
		t.Fatalf("cashier display: %s", raw)
	}

	res, _ = fx.svc.Options(ctx, fx.manager, OptionsQuery{Model: "cr_products", Field: "product_id"})
	if raw, _ := json.Marshal(res.Options[0]); !strings.Contains(string(raw), "777") {
		t.Fatalf("manager option must carry cost: %s", raw)
	}

	// Filtering the picker by a hidden column is refused.
	_, err = fx.svc.Options(ctx, fx.cashier, OptionsQuery{Model: "cr_products", Field: "by_cost", FilterValue: "777"})
	var cre *ColumnReadDeniedError
	if !errors.As(err, &cre) || !errors.Is(err, ErrForbidden) {
		t.Fatalf("filter_value over cost: %v", err)
	}
	if _, err := fx.svc.Options(ctx, fx.manager, OptionsQuery{Model: "cr_products", Field: "by_cost", FilterValue: "777"}); err != nil {
		t.Fatalf("manager filter_value: %v", err)
	}

	// A hidden label falls back to the value column; q never matches the cost.
	res, err = fx.svc.Options(ctx, fx.cashier, OptionsQuery{Model: "cr_products", Field: "cost_label", Q: "777"})
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := json.Marshal(res.Options); strings.Contains(string(raw), "777") {
		t.Fatalf("hidden label leaked: %s", raw)
	}
}

func TestColumnRead_SearchPickerMasksColumns(t *testing.T) {
	fx := newCRFixture(t, func(c *Config) {
		c.SearchConfigResolver = func(_ context.Context, model string, _ any) (*SearchConfig, error) {
			return &SearchConfig{
				SearchIn: []string{"name", "cost"}, Value: "id", Label: "name",
				Description: "cost", ExtraFields: []string{"cost", "price"}, AllowFilters: []string{"cost"},
			}, nil
		}
	})
	ctx := context.Background()
	hits, err := fx.svc.Search(ctx, fx.cashier, SearchQuery{Model: "cr_products", Q: "777"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("cashier search matched the hidden cost: %+v", hits)
	}
	hits, _ = fx.svc.Search(ctx, fx.cashier, SearchQuery{Model: "cr_products", Q: "Widg"})
	if raw, _ := json.Marshal(hits); len(hits) != 1 || strings.Contains(string(raw), "777") {
		t.Fatalf("cashier search projection: %s", raw)
	}
	if hits, _ := fx.svc.Search(ctx, fx.manager, SearchQuery{Model: "cr_products", Q: "777"}); len(hits) != 1 {
		t.Fatalf("manager search by cost: %+v", hits)
	}
	_, err = fx.svc.Search(ctx, fx.cashier, SearchQuery{Model: "cr_products", Filters: map[string]string{"cost": "777"}})
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("search filter over cost: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Events forwarded to clients, mask decisions
// ---------------------------------------------------------------------------

func TestColumnRead_RedactEvent(t *testing.T) {
	fx := newCRFixture(t, nil)
	ctx := context.Background()
	ev := &CanonicalEvent{Model: "cr_products", Action: "updated",
		Before: map[string]any{"id": fx.product, "cost": 700.0, "price": 1},
		After:  map[string]any{"id": fx.product, "cost": 777.0, "price": 2}}

	for name, u := range map[string]modelbase.AuthUser{"cashier": fx.cashier, "no principal": nil} {
		out := fx.svc.RedactEvent(ctx, ev, u)
		if _, ok := out.After["cost"]; ok {
			t.Fatalf("%s: event after leaked cost: %v", name, out.After)
		}
		if _, ok := out.Before["cost"]; ok {
			t.Fatalf("%s: event before leaked cost: %v", name, out.Before)
		}
		if out.After["price"] != 2 {
			t.Fatalf("%s: event lost readable columns: %v", name, out.After)
		}
	}
	if out := fx.svc.RedactEvent(ctx, ev, fx.manager); out.After["cost"] != 777.0 {
		t.Fatalf("manager event: %v", out.After)
	}
	if ev.After["cost"] != 777.0 || ev.Before["cost"] != 700.0 {
		t.Fatalf("RedactEvent mutated the original: %v", ev)
	}
	if got := fx.svc.RedactRecord(ctx, "cr_products", fx.cashier, ev.After); got["cost"] != nil {
		t.Fatalf("RedactRecord: %v", got)
	}
}

func TestColumnRead_MaskDecisions(t *testing.T) {
	ctx := context.Background()

	t.Run("system caller reads everything", func(t *testing.T) {
		fx := newCRFixture(t, nil)
		if m := fx.svc.ColumnReadMask(ctx, "cr_products", NewSystemCaller(fx.org)); !m.Empty() {
			t.Fatalf("system caller mask: %v", m.Columns())
		}
	})
	t.Run("no permission service fails open by default", func(t *testing.T) {
		registerCRModels()
		svc := New(Config{DB: setupTestDB(t), Metadata: metadata.New(metadata.Config{CacheTTL: -1})})
		if m := svc.ColumnReadMask(ctx, "cr_products", &fakeUser{id: uuid.New(), orgID: uuid.New()}); !m.Empty() {
			t.Fatalf("mask: %v", m.Columns())
		}
	})
	t.Run("RequirePermissions without a service hides gated columns", func(t *testing.T) {
		registerCRModels()
		svc := New(Config{DB: setupTestDB(t), Metadata: metadata.New(metadata.Config{CacheTTL: -1}), RequirePermissions: true})
		m := svc.ColumnReadMask(ctx, "cr_products", &fakeUser{id: uuid.New(), orgID: uuid.New()})
		if !m.Denied("cost") || m.Denied("price") {
			t.Fatalf("mask: %v", m.Columns())
		}
	})
	t.Run("custom checker", func(t *testing.T) {
		fx := newCRFixture(t, func(c *Config) {
			c.ColumnReadChecker = func(_ context.Context, u modelbase.AuthUser, perm string) bool {
				return u.GetRole() == "cashier" && perm == permViewCost
			}
		})
		if m := fx.svc.ColumnReadMask(ctx, "cr_products", fx.cashier); m.Denied("cost") {
			t.Fatal("custom checker granted cost to the cashier")
		}
		if m := fx.svc.ColumnReadMask(ctx, "cr_products", fx.manager); !m.Denied("cost") {
			t.Fatal("custom checker denied cost to the manager")
		}
	})
	t.Run("extension columns", func(t *testing.T) {
		fx := newCRFixture(t, func(c *Config) {
			mustExec(t, c.DB, `CREATE TABLE cr_tires (id TEXT PRIMARY KEY, landed_cost REAL, size TEXT)`)
			c.ExtensionResolver = func(_ context.Context, model string) []ExtensionTable {
				if model != "cr_products" {
					return nil
				}
				return []ExtensionTable{{Key: "Tire", Table: "cr_tires", Columns: []manifest.ColumnDef{
					{Name: "landed_cost", ReadPermission: permViewCost},
					{Name: "size"},
				}}}
			}
		})
		m := fx.svc.ColumnReadMask(ctx, "cr_products", fx.cashier)
		if !m.Denied("Tire.landed_cost") || m.Denied("Tire.size") {
			t.Fatalf("extension mask: %v", m.Columns())
		}
		err := m.CheckParams(query.Params{RelationFilters: []query.RelationFilter{{Relation: "Tire", Field: "landed_cost"}}})
		if !errors.Is(err, ErrForbidden) {
			t.Fatalf("extension filter: %v", err)
		}
		exts := m.extensions([]ExtensionTable{{Key: "Tire", Columns: []manifest.ColumnDef{{Name: "landed_cost"}, {Name: "size"}}}})
		if len(exts[0].Columns) != 1 || exts[0].Columns[0].Name != "size" {
			t.Fatalf("masked extensions: %+v", exts)
		}
	})
	t.Run("masked metadata", func(t *testing.T) {
		fx := newCRFixture(t, nil)
		meta, _ := fx.svc.TableMetadata(ctx, "cr_products")
		meta.DefaultSortBy = "cost"
		got := fx.svc.ColumnReadMask(ctx, "cr_products", fx.cashier).TableMeta(meta)
		for _, c := range got.Columns {
			if c.Key == "cost" {
				t.Fatal("masked metadata kept cost")
			}
		}
		if got.DefaultSortBy != "" || len(got.SearchColumns) != 1 {
			t.Fatalf("masked metadata: %+v", got)
		}
		if len(meta.Columns) != 4 {
			t.Fatal("TableMeta mutated the served metadata")
		}
	})
}

func TestDeriveTableColumns_ReadPermission(t *testing.T) {
	cols := DeriveTableColumns(manifest.ModelDefinition{TableName: "products", Columns: []manifest.ColumnDef{
		{Name: "name", Type: "string"},
		{Name: "cost", Type: "decimal", ReadPermission: permViewCost},
	}})
	if cols[0].ReadPermission != "" || cols[1].ReadPermission != permViewCost {
		t.Fatalf("derived columns: %+v", cols)
	}
}

// gateUnitPrice marks the line models' unit_price as gated, readable by
// nobody (the checker always denies), on the reflect-built «crear desde»
// harness of document_lines_test.go.
func gateUnitPrice(svc *Service) {
	svc.meta.WithTableTransformer(func(_ context.Context, model string, meta *modelbase.TableMetadata) error {
		switch model {
		case "DlOrderItem", "DlOrder":
			meta.Columns = append(meta.Columns,
				modelbase.ColumnDef{Key: "unit_price", ReadPermission: permViewCost},
				modelbase.ColumnDef{Key: "number", ReadPermission: permViewCost})
		}
		return nil
	})
	svc.columnReadChecker = func(context.Context, modelbase.AuthUser, string) bool { return false }
}

func TestColumnRead_SourceLinesHideColumn(t *testing.T) {
	svc, _ := newDocLinesService(t, true)
	gateUnitPrice(svc)
	user := newUser(uuid.New())
	order, _ := seedOrder(t, svc, user)
	rows, err := svc.SourceLines(context.Background(), "DlInvoice", user, "sale", order, "")
	if err != nil || len(rows) != 1 {
		t.Fatalf("source lines: %v %v", rows, err)
	}
	if _, ok := rows[0]["unit_price"]; ok {
		t.Fatalf("source lines leaked unit_price: %v", rows[0])
	}
	if rows[0]["remaining_quantity"] != 5.0 {
		t.Fatalf("source lines lost the pending quantity: %v", rows[0])
	}
}

func TestColumnRead_RestoreResponseHidesColumn(t *testing.T) {
	svc, _ := newDocLinesService(t, false)
	gateUnitPrice(svc)
	bus := &capturingBus{}
	svc.bus = bus
	user := newUser(uuid.New())
	ctx := context.Background()
	order, _ := seedOrder(t, svc, user)
	id := uuid.MustParse(order)
	if err := svc.Delete(ctx, "DlOrder", user, id); err != nil {
		t.Fatal(err)
	}
	row, err := svc.Restore(ctx, "DlOrder", user, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := row["number"]; ok {
		t.Fatalf("restore response leaked a gated column: %v", row)
	}
	bus.mu.Lock()
	defer bus.mu.Unlock()
	last := bus.events[len(bus.events)-1]
	if _, ok := last.After["number"]; !ok {
		t.Fatalf("restore event must keep the full row: %v", last.After)
	}
}
