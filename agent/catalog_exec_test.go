package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/agent/tools"
	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// Semantic catalog against a real SQL engine (sqlite; CI runs with cgo).

func newCatalogCRUD(t *testing.T, mut func(*fakeCatalog)) (*CRUDService, uuid.UUID) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	org := uuid.New()
	stmts := []string{
		`CREATE TABLE customers (id text PRIMARY KEY, organization_id text, name text, email text, phone text, rfc text, branch_id text, created_at datetime, deleted_at datetime)`,
		`CREATE TABLE sales_orders (id text PRIMARY KEY, organization_id text, folio text, customer_id text, status text, total real, currency_id text, order_date date, notes text, created_at datetime, deleted_at datetime)`,
		`CREATE TABLE branches (id text PRIMARY KEY, organization_id text, name text, city text, created_at datetime, deleted_at datetime)`,
		`CREATE TABLE stock (id text PRIMARY KEY, organization_id text, product_id text, branch_id text, quantity real, reserved real, available real, created_at datetime, deleted_at datetime)`,
		`CREATE TABLE products (id text PRIMARY KEY, organization_id text, name text, sku text, price real, active bool, category text, created_at datetime, deleted_at datetime)`,
		`CREATE TABLE invoices (id text PRIMARY KEY, organization_id text, number text, customer_id text, state text, amount_total real, due_date date, paid_at datetime, created_at datetime, deleted_at datetime)`,
	}
	for _, s := range stmts {
		if err := db.Exec(s).Error; err != nil {
			t.Fatal(err)
		}
	}
	other := uuid.New()
	seed := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO customers (id, organization_id, name, email, created_at) VALUES (?,?,?,?,?)`, []any{"c1", org, "Juan Pérez", "juan@x.mx", "2026-11-01 10:00:00"}},
		{`INSERT INTO customers (id, organization_id, name, email, created_at) VALUES (?,?,?,?,?)`, []any{"c2", org, "Ana López", "ana@x.mx", "2026-11-02 10:00:00"}},
		{`INSERT INTO customers (id, organization_id, name, created_at) VALUES (?,?,?,?)`, []any{"cx", other, "Otra Org", "2026-11-02 10:00:00"}},
		{`INSERT INTO sales_orders (id, organization_id, folio, customer_id, status, total, created_at) VALUES (?,?,?,?,?,?,?)`, []any{"o1", org, "PV-1", "c1", "confirmed", 1500.0, "2026-11-15 09:30:00"}},
		{`INSERT INTO sales_orders (id, organization_id, folio, customer_id, status, total, created_at) VALUES (?,?,?,?,?,?,?)`, []any{"o2", org, "PV-2", "c1", "draft", 100.0, "2026-11-14 18:00:00"}},
		{`INSERT INTO sales_orders (id, organization_id, folio, customer_id, status, total, created_at) VALUES (?,?,?,?,?,?,?)`, []any{"o3", org, "PV-3", "c2", "confirmed", 250.0, "2026-11-15 20:00:00"}},
		{`INSERT INTO sales_orders (id, organization_id, folio, customer_id, status, total, created_at, deleted_at) VALUES (?,?,?,?,?,?,?,?)`, []any{"o4", org, "PV-4", "c2", "confirmed", 9999.0, "2026-11-15 20:00:00", "2026-11-15 21:00:00"}},
		{`INSERT INTO sales_orders (id, organization_id, folio, customer_id, status, total, created_at) VALUES (?,?,?,?,?,?,?)`, []any{"ox", other, "PV-X", "cx", "confirmed", 7777.0, "2026-11-15 20:00:00"}},
	}
	for _, s := range seed {
		if err := db.Exec(s.sql, s.args...).Error; err != nil {
			t.Fatal(err)
		}
	}
	cat := erpFixtureCatalog()
	if mut != nil {
		mut(&cat)
	}
	crud, err := NewCRUDService(CRUDConfig{DB: db, Catalog: cat, Permissions: fakePerms{deny: map[string]bool{}}})
	if err != nil {
		t.Fatal(err)
	}
	return crud, org
}

func runTree(t *testing.T, crud *CRUDService, org uuid.UUID, tree QueryTree) (any, error) {
	t.Helper()
	ix := NewCatalogIndex(crud.cfg.Catalog, CatalogOptions{})
	return crud.QueryTree(context.Background(), &AgentIdentity{OrganizationID: org, UserID: uuid.New()}, tree, QueryTreeOptions{Catalog: ix})
}

func TestCRUDQueryTreeRows(t *testing.T) {
	crud, org := newCatalogCRUD(t, nil)
	res, err := runTree(t, crud, org, QueryTree{
		Model: "pedidos", // resolved through the catalog? no: use the real key below
	})
	if err == nil {
		t.Fatalf("unknown model must fail, got %v", res)
	}

	res, err = runTree(t, crud, org, QueryTree{
		Model: "sales_orders",
		Where: []QueryCondition{{"status", "=", "Confirmado"}, {"created_at", "=", "2026-11-15"}},
		Order: "total desc",
	})
	if err != nil {
		t.Fatal(err)
	}
	qr := res.(*QueryResult)
	if qr.Count != 2 || len(qr.Records) != 2 || qr.Records[0]["id"] != "o1" {
		t.Fatalf("org-scoped, soft-delete aware, day-wide date: %+v", qr.Records)
	}
	for _, want := range []string{"Juan Pérez [c1]", "Ana López [c2]", "Confirmado", "2 registros"} {
		if !strings.Contains(qr.Text, want) {
			t.Errorf("text misses %q:\n%s", want, qr.Text)
		}
	}

	res, err = runTree(t, crud, org, QueryTree{Model: "sales_orders", Where: []QueryCondition{{"total", "between", []any{100, 300}}}, Limit: 1, Order: "folio"})
	if err != nil {
		t.Fatal(err)
	}
	qr = res.(*QueryResult)
	if qr.Count != 2 || !strings.Contains(qr.Text, "next: offset=1") {
		t.Fatalf("pagination hint: %d\n%s", qr.Count, qr.Text)
	}

	res, err = runTree(t, crud, org, QueryTree{Model: "customers", Search: "pérez"})
	if err != nil {
		t.Fatal(err)
	}
	qr = res.(*QueryResult)
	if qr.Count != 1 || !strings.Contains(qr.Text, "- Nombre: Juan Pérez") {
		t.Fatalf("single record renders as key-value: %s", qr.Text)
	}

	res, err = runTree(t, crud, org, QueryTree{Model: "sales_orders", Where: []QueryCondition{{"status", "not in", []any{"draft"}}, {"folio", "ilike", "pv"}}, Fields: []string{"folio", "customer_id"}})
	if err != nil {
		t.Fatal(err)
	}
	if qr = res.(*QueryResult); qr.Count != 2 || strings.Contains(qr.Text, "Estado") {
		t.Fatalf("fields select only those columns: %s", qr.Text)
	}
}

func TestCRUDQueryTreeAggregates(t *testing.T) {
	crud, org := newCatalogCRUD(t, nil)
	res, err := runTree(t, crud, org, QueryTree{
		Model: "sales_orders", GroupBy: []string{"customer_id"},
		Aggregates: []QueryAggregate{{Fn: "sum", Field: "total", As: "vendido"}, {Fn: "count"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ar := res.(*AggregateResult)
	if len(ar.Rows) != 2 {
		t.Fatalf("groups: %+v", ar.Rows)
	}
	for _, want := range []string{"| Cliente | vendido | count |", "Juan Pérez [c1] | 1600 | 2", "Ana López [c2] | 250 | 1"} {
		if !strings.Contains(ar.Text, want) {
			t.Errorf("aggregate text misses %q:\n%s", want, ar.Text)
		}
	}
	res, err = runTree(t, crud, org, QueryTree{Model: "sales_orders", Aggregates: []QueryAggregate{{Fn: "count"}}})
	if err != nil || !strings.Contains(res.(*AggregateResult).Text, "count = 3") {
		t.Fatalf("scalar count: %v %v", res, err)
	}
}

func TestCRUDQueryTreeHonorsColumnMask(t *testing.T) {
	crud, org := newCatalogCRUD(t, func(c *fakeCatalog) {
		for i := range c.models {
			if c.models[i].Table == "sales_orders" {
				c.models[i].ReadPermissions = map[string]string{"total": "sales.view_amounts"}
			}
		}
	})
	if _, err := runTree(t, crud, org, QueryTree{Model: "sales_orders", Where: []QueryCondition{{"total", ">", 1}}}); err == nil || !strings.Contains(err.Error(), "not readable") {
		t.Fatalf("filter on a hidden column must be refused: %v", err)
	}
	res, err := runTree(t, crud, org, QueryTree{Model: "sales_orders"})
	if err != nil {
		t.Fatal(err)
	}
	qr := res.(*QueryResult)
	if _, leaked := qr.Records[0]["total"]; leaked || strings.Contains(qr.Text, "1500") {
		t.Fatalf("hidden column leaked: %s", qr.Text)
	}
	card, err := crud.DescribeModelCard(&AgentIdentity{OrganizationID: org}, nil, "sales_orders")
	if err != nil || strings.Contains(card, "total:") {
		t.Fatalf("describe must hide the column: %v\n%s", err, card)
	}
}

func TestCompactListAndDescribe(t *testing.T) {
	crud, org := newCatalogCRUD(t, nil)
	id := &AgentIdentity{OrganizationID: org}
	ix := NewCatalogIndex(crud.cfg.Catalog, CatalogOptions{})

	list := crud.ListModelsCompact(id, ix)
	if !strings.Contains(list, "sales_orders \"Pedidos de venta\"\n") || !strings.HasSuffix(list, "6 modelos. Campos: catalog_search(consulta) o describe_model(model).") {
		t.Fatalf("list:\n%s", list)
	}
	legacy, _ := crud.ListModels(id)
	legacyJSON, _ := json.Marshal(map[string]any{"total": len(legacy), "models": legacy})
	t.Logf("list_models: %d -> %d bytes", len(legacyJSON), len(list))
	if len(list) >= len(legacyJSON) {
		t.Fatalf("compact list must be smaller than the JSON (%d vs %d)", len(list), len(legacyJSON))
	}

	card, err := crud.DescribeModelCard(id, ix, "SalesOrder")
	if err != nil || !strings.HasPrefix(card, goldenSalesOrderCard) || !strings.Contains(card, "Filtros de la vista: order_date") || !strings.Contains(card, "Ops:") {
		t.Fatalf("describe:\n%s %v", card, err)
	}
	legacyDesc, _ := crud.DescribeModel(id, "sales_orders")
	descJSON, _ := json.Marshal(legacyDesc)
	t.Logf("describe_model: %d -> %d bytes", len(descJSON), len(card))
	if len(card) >= len(descJSON) {
		t.Fatalf("card must be smaller than the schema JSON (%d vs %d)", len(card), len(descJSON))
	}
}

// ---------------------------------------------------------------------------
// Copilot bundle: prefetch placement, prefix stability, fewer calls.
// ---------------------------------------------------------------------------

func newSemanticCopilot(t *testing.T, ai AIService, semantic bool, base ...tools.BaseTool) (*CopilotService, uuid.UUID) {
	t.Helper()
	crud, org := newCatalogCRUD(t, nil)
	o := newTurnOrchestrator(t, ai, base...)
	svc, err := NewCopilotService(CopilotConfig{
		Orchestrator: o, CRUD: crud, Proposals: NewProposalStore(),
		Brand:           fakeBrand{Personality{Name: "Aby", SystemPrompt: "Eres Aby."}},
		Clock:           fixedClock{time.Date(2026, 11, 15, 9, 30, 0, 0, time.UTC)},
		SemanticCatalog: semantic,
	})
	if err != nil {
		t.Fatal(err)
	}
	return svc, org
}

func TestSemanticCatalogKeepsPrefixStableAndPrefetchesInTurnContext(t *testing.T) {
	ai := &captureAI{}
	svc, org := newSemanticCopilot(t, ai, true)
	user, session := uuid.New(), uuid.New()
	ctx := context.Background()
	for _, msg := range []string{"¿cuántos pedidos de venta confirmados hay?", "existencias por sucursal", "hola"} {
		if _, err := svc.ProcessCopilotMessage(ctx, org, user, session, msg, nil, false); err != nil {
			t.Fatal(err)
		}
	}
	if len(ai.prompts) != 3 {
		t.Fatalf("one call per turn, got %d", len(ai.prompts))
	}
	d0, _ := json.Marshal(ai.defs[0])
	for i := 1; i < 3; i++ {
		if ai.prompts[i] != ai.prompts[0] {
			t.Fatal("system prompt must be byte-identical across turns")
		}
		if di, _ := json.Marshal(ai.defs[i]); string(di) != string(d0) {
			t.Fatal("tool definitions must be byte-identical across turns")
		}
	}
	sys := ai.prompts[0]
	if !strings.Contains(sys, ERPCardLegend) || strings.Contains(sys, prefetchHeader) || strings.Contains(sys, `sales_orders "Pedidos`) {
		t.Fatalf("legend in the prompt, cards never:\n%s", sys)
	}
	u0, u1, u2 := lastUserContent(ai.messages[0]), lastUserContent(ai.messages[1]), lastUserContent(ai.messages[2])
	if !strings.HasPrefix(u0, "¿cuántos pedidos de venta confirmados hay?") || !strings.Contains(u0, "[Catálogo relevante]") || !strings.Contains(u0, `sales_orders "Pedidos de venta"`) {
		t.Fatalf("turn 1 prefetch:\n%s", u0)
	}
	if !strings.Contains(u1, `stock "Existencias"`) {
		t.Fatalf("turn 2 prefetch:\n%s", u1)
	}
	if strings.Contains(u2, "[Catálogo relevante]") {
		t.Fatalf("chitchat gets no cards:\n%s", u2)
	}
	if block := svc.CatalogPrefetch(ctx, org, user, "pedidos de venta, clientes y facturas"); EstimateTokens(block) > DefaultPrefetchTokenBudget {
		t.Fatalf("prefetch over budget: %d tokens", EstimateTokens(block))
	}
	names := map[string]bool{}
	for _, d := range ai.defs[0] {
		names[d["name"].(string)] = true
	}
	for _, n := range []string{"catalog_search", "list_models", "describe_model", "query_data"} {
		if !names[n] {
			t.Errorf("bundle tool %s missing", n)
		}
	}
}

func TestSemanticCatalogOffChangesNothing(t *testing.T) {
	ai := &captureAI{}
	svc, org := newSemanticCopilot(t, ai, false)
	if _, err := svc.ProcessCopilotMessage(context.Background(), org, uuid.New(), uuid.New(), "pedidos de venta", nil, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ai.prompts[0], "ERP-Card") || strings.Contains(lastUserContent(ai.messages[0]), "[Catálogo relevante]") {
		t.Fatal("bundle off must not touch prompt or turn context")
	}
	if svc.CatalogIndex() != nil {
		t.Fatal("no index when off")
	}
}

// simModel plays a model that follows the prompt: with the card in context it
// queries directly; without it, it walks list_models -> describe_model ->
// query_data. It answers once query_data returned.
type simModel struct{ calls int }

func (s *simModel) GenerateChatWithTools(_ context.Context, msgs []map[string]interface{}, _ string, _ []map[string]interface{}) (*AIResponse, error) {
	s.calls++
	last := msgs[len(msgs)-1]
	call := func(name string, params map[string]interface{}) *AIResponse {
		return &AIResponse{ToolCalls: []tools.ToolCall{{ID: name + "-1", ToolName: name, Parameters: params}}}
	}
	count := map[string]interface{}{"model": "sales_orders", "where": []interface{}{[]interface{}{"status", "=", "confirmed"}}, "aggregates": []interface{}{map[string]interface{}{"fn": "count"}}}
	if last["role"] == "tool" {
		switch last["name"] {
		case "list_models":
			return call("describe_model", map[string]interface{}{"model": "sales_orders"}), nil
		case "describe_model", "catalog_search":
			return call("query_data", count), nil
		default:
			return finalAnswer("fa", "Hay 2 pedidos confirmados."), nil
		}
	}
	if strings.Contains(lastUserContent(msgs), `sales_orders "Pedidos de venta"`) {
		return call("query_data", count), nil
	}
	return call("list_models", map[string]interface{}{}), nil
}

func TestSemanticCatalogCutsCallsPerLookup(t *testing.T) {
	stubs := []tools.BaseTool{
		&stubTool{name: "list_models", result: map[string]any{"models": []any{}}},
		&stubTool{name: "describe_model", result: map[string]any{"model": "sales_orders"}},
		&stubTool{name: "query_data", result: map[string]any{"count": 2}},
	}
	run := func(semantic bool) int {
		ai := &simModel{}
		svc, org := newSemanticCopilot(t, ai, semantic, stubs...)
		res, err := svc.ProcessCopilotMessage(context.Background(), org, uuid.New(), uuid.New(), "¿cuántos pedidos de venta confirmados hay?", nil, false)
		if err != nil || res.Message != "Hay 2 pedidos confirmados." {
			t.Fatalf("semantic=%v: %v %+v", semantic, err, res)
		}
		return ai.calls
	}
	before, after := run(false), run(true)
	t.Logf("LLM calls per lookup: %d -> %d", before, after)
	if before != 4 || after != 2 {
		t.Fatalf("want 4 -> 2 calls, got %d -> %d", before, after)
	}
}
