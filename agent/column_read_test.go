package agent

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/vector"
)

const agentPermViewCost = "inventory.product.view_cost"

type fakeNamedPerms map[uuid.UUID][]string

func (f fakeNamedPerms) HasPermission(_, userID uuid.UUID, perm string) (bool, error) {
	for _, p := range f[userID] {
		if p == perm {
			return true, nil
		}
	}
	return false, nil
}

type fakeIndex struct{}

func (fakeIndex) VectorSearch(string, string, uuid.UUID, int) ([]vector.SearchResult, error) {
	return []vector.SearchResult{{ID: "1", Data: map[string]any{"name": "Widget", "cost": 777}}}, nil
}
func (fakeIndex) EnqueueRecord(string, uuid.UUID) {}

type colFixture struct {
	s        *CRUDService
	org      uuid.UUID
	cashier  *AgentIdentity
	manager  *AgentIdentity
	recordID string
}

func newColFixture(t *testing.T, mut func(*CRUDConfig)) colFixture {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TABLE products (id text PRIMARY KEY, organization_id text, name text, cost real, price real, created_at datetime DEFAULT CURRENT_TIMESTAMP, deleted_at datetime)`).Error; err != nil {
		t.Fatal(err)
	}
	org := uuid.New()
	id := uuid.NewString()
	if err := db.Exec(`INSERT INTO products (id, organization_id, name, cost, price) VALUES (?, ?, 'Widget', 777, 1000)`, id, org.String()).Error; err != nil {
		t.Fatal(err)
	}
	cashier := &AgentIdentity{OrganizationID: org, UserID: uuid.New()}
	manager := &AgentIdentity{OrganizationID: org, UserID: uuid.New()}
	cfg := CRUDConfig{
		DB: db,
		Catalog: fakeCatalog{models: []ModelMeta{{
			Key: "products", Table: "products",
			Columns:  []string{"name", "cost", "price"},
			SearchIn: []string{"name", "cost"},
			Schema: map[string]any{
				"columns":   []any{map[string]any{"key": "name"}, map[string]any{"key": "cost"}},
				"search_in": []string{"name", "cost"},
				"title":     "Products",
			},
			ReadPermissions: map[string]string{"cost": agentPermViewCost},
		}}},
		Permissions:       fakePerms{deny: map[string]bool{}},
		ColumnPermissions: fakeNamedPerms{manager.UserID: {agentPermViewCost}},
		Index:             fakeIndex{},
	}
	if mut != nil {
		mut(&cfg)
	}
	s, err := NewCRUDService(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return colFixture{s: s, org: org, cashier: cashier, manager: manager, recordID: id}
}

func TestAgentColumnRead_QueryAndGetHideColumn(t *testing.T) {
	fx := newColFixture(t, nil)
	res, err := fx.s.Query(fx.cashier, QueryRequest{Model: "products"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Records) != 1 {
		t.Fatalf("records: %v", res.Records)
	}
	if _, ok := res.Records[0]["cost"]; ok {
		t.Fatalf("cashier query leaked cost: %v", res.Records[0])
	}
	if res.Records[0]["price"] == nil {
		t.Fatalf("cashier query lost readable columns: %v", res.Records[0])
	}
	if res, _ := fx.s.Query(fx.manager, QueryRequest{Model: "products"}); res.Records[0]["cost"] == nil {
		t.Fatalf("manager query: %v", res.Records[0])
	}

	row, err := fx.s.GetRecord(fx.cashier, "products", fx.recordID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := row["cost"]; ok {
		t.Fatalf("cashier get leaked cost: %v", row)
	}

	// Search never matches the hidden column.
	if res, _ := fx.s.Query(fx.cashier, QueryRequest{Model: "products", Search: "777"}); res != nil && len(res.Records) != 0 {
		t.Fatalf("cashier search matched cost: %v", res.Records)
	}
}

func TestAgentColumnRead_FilterOrderAggregateRejected(t *testing.T) {
	fx := newColFixture(t, nil)
	checks := map[string]func(*AgentIdentity) error{
		"filter": func(id *AgentIdentity) error {
			_, err := fx.s.Query(id, QueryRequest{Model: "products", Filters: map[string]any{"cost": 777}})
			return err
		},
		"order": func(id *AgentIdentity) error {
			_, err := fx.s.Query(id, QueryRequest{Model: "products", OrderBy: "cost desc"})
			return err
		},
		"aggregate": func(id *AgentIdentity) error {
			_, err := fx.s.Aggregate(id, AggregateRequest{Model: "products", Function: "sum", Field: "cost"})
			return err
		},
		"group_by": func(id *AgentIdentity) error {
			_, err := fx.s.Aggregate(id, AggregateRequest{Model: "products", Function: "count", GroupBy: "cost"})
			return err
		},
		"aggregate filter": func(id *AgentIdentity) error {
			_, err := fx.s.Aggregate(id, AggregateRequest{Model: "products", Function: "count", Filters: map[string]any{"cost": 777}})
			return err
		},
	}
	for name, run := range checks {
		if err := run(fx.cashier); err == nil || !strings.Contains(err.Error(), "not readable") {
			t.Fatalf("cashier %s: err %v", name, err)
		}
		if err := run(fx.manager); err != nil {
			t.Fatalf("manager %s: %v", name, err)
		}
	}
}

func TestAgentColumnRead_CatalogAndVector(t *testing.T) {
	fx := newColFixture(t, nil)
	list, err := fx.s.ListModels(fx.cashier)
	if err != nil {
		t.Fatal(err)
	}
	if cols, _ := list[0]["columns"].([]string); len(cols) != 2 || strings.Contains(strings.Join(cols, ","), "cost") {
		t.Fatalf("list_models columns: %v", list[0]["columns"])
	}
	desc, err := fx.s.DescribeModel(fx.cashier, "products")
	if err != nil {
		t.Fatal(err)
	}
	if cols, _ := desc["columns"].([]any); len(cols) != 1 {
		t.Fatalf("describe columns: %v", desc["columns"])
	}
	if si, _ := desc["search_in"].([]string); len(si) != 1 || si[0] != "name" {
		t.Fatalf("describe search_in: %v", desc["search_in"])
	}
	if desc, _ := fx.s.DescribeModel(fx.manager, "products"); len(desc["columns"].([]any)) != 2 {
		t.Fatalf("manager describe: %v", desc)
	}

	out, err := fx.s.VectorSearch(fx.cashier, "products", "widget", 5)
	if err != nil {
		t.Fatal(err)
	}
	if res := out.([]vector.SearchResult); res[0].Data["cost"] != nil || res[0].Data["name"] != "Widget" {
		t.Fatalf("cashier vector result: %v", res[0].Data)
	}
}

func TestAgentColumnRead_FailClosedWithoutChecker(t *testing.T) {
	fx := newColFixture(t, func(c *CRUDConfig) { c.ColumnPermissions = nil })
	res, err := fx.s.Query(fx.manager, QueryRequest{Model: "products"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Records[0]["cost"]; ok {
		t.Fatalf("no checker must hide gated columns: %v", res.Records[0])
	}
	dev := newColFixture(t, func(c *CRUDConfig) { c.ColumnPermissions = nil; c.DevAllowAll = true })
	if res, _ := dev.s.Query(dev.cashier, QueryRequest{Model: "products"}); res.Records[0]["cost"] == nil {
		t.Fatalf("DevAllowAll reads everything: %v", res.Records[0])
	}
}
