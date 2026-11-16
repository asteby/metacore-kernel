package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/agent/tools"
)

// ---------------------------------------------------------------------------
// ERP-Card rendering
// ---------------------------------------------------------------------------

func TestERPCardLegendIsConstantAndSmall(t *testing.T) {
	if n := EstimateTokens(ERPCardLegend); n > 180 {
		t.Fatalf("legend must stay ~150 tokens, got %d", n)
	}
	for _, want := range []string{"->M", "[]M", "<>M", "* requerido", "ro solo lectura", "enum(a|b)", "money(c)", "dt=fecha-hora"} {
		if !strings.Contains(ERPCardLegend, want) {
			t.Errorf("legend misses %q", want)
		}
	}
	if !strings.Contains(CatalogPromptClause, ERPCardLegend) {
		t.Fatal("prompt clause must carry the legend")
	}
}

const goldenSalesOrderCard = `sales_orders "Pedidos de venta" (pedido, orden, venta, pedido de venta)
  folio:str* ro | customer_id:->customers "Cliente"* | status:enum(draft=Borrador|confirmed=Confirmado|cancelled=Cancelado) "Estado" | total:money(currency_id) | currency_id:str "Moneda" | order_date:date "Fecha" | notes:text "Notas" | lines:[]sales_order_lines "Partidas" ro`

func TestRenderCardGolden(t *testing.T) {
	got := RenderCard(erpFixtureModel("sales_orders"), CardOptions{})
	if got != goldenSalesOrderCard {
		t.Fatalf("card drifted:\n got: %s\nwant: %s", got, goldenSalesOrderCard)
	}
	stock := RenderCard(erpFixtureModel("stock"), CardOptions{})
	for _, want := range []string{`stock "Existencias" (inventario, existencia)`, "product_id:->products \"Producto\"", "available:num \"Disponible\" ro"} {
		if !strings.Contains(stock, want) {
			t.Errorf("stock card misses %q:\n%s", want, stock)
		}
	}
	cust := RenderCard(erpFixtureModel("customers"), CardOptions{})
	if !strings.HasPrefix(cust, `customers "Clientes" (cliente)`) || !strings.Contains(cust, `name:str "Nombre"*`) {
		t.Errorf("customers card:\n%s", cust)
	}
	if strings.Contains(cust, "organization_id") || strings.Contains(cust, "created_at") {
		t.Errorf("system columns must not be on the card:\n%s", cust)
	}
}

func TestCatalogCardsAreDeterministic(t *testing.T) {
	a := NewCatalogIndex(erpFixtureCatalog(), CatalogOptions{})
	rev := erpFixtureCatalog()
	for i, j := 0, len(rev.models)-1; i < j; i, j = i+1, j-1 {
		rev.models[i], rev.models[j] = rev.models[j], rev.models[i]
	}
	b := NewCatalogIndex(rev, CatalogOptions{})
	if a.Version() != b.Version() || a.Version() == "" {
		t.Fatalf("version must not depend on catalog order: %s vs %s", a.Version(), b.Version())
	}
	am, bm := a.Models(), b.Models()
	if len(am) != 6 || len(am) != len(bm) {
		t.Fatalf("models: %d vs %d", len(am), len(bm))
	}
	for i := range am {
		ca, cb := RenderCard(am[i], CardOptions{}), RenderCard(bm[i], CardOptions{})
		if ca != cb || ca != RenderCard(am[i], CardOptions{}) {
			t.Fatalf("card %s not deterministic", am[i].Name)
		}
	}
}

func TestRenderCardMaxFieldsKeepsTheInformativeOnes(t *testing.T) {
	got := RenderCard(erpFixtureModel("sales_orders"), CardOptions{MaxFields: 3})
	for _, want := range []string{"folio:", "customer_id:", "+5 campos (describe_model)"} {
		if !strings.Contains(got, want) {
			t.Errorf("trimmed card misses %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "notes:") || strings.Contains(got, "lines:") {
		t.Errorf("low-value fields must go first:\n%s", got)
	}
}

func TestCardHidesUnreadableColumns(t *testing.T) {
	m := erpFixtureModel("sales_orders").withoutFields(columnMask{"total": {}})
	if strings.Contains(RenderCard(m, CardOptions{}), "total:") {
		t.Fatal("a masked column must not reach the card")
	}
}

// ---------------------------------------------------------------------------
// Retrieval
// ---------------------------------------------------------------------------

func searchNames(ix *CatalogIndex, q string, k int) []string {
	var out []string
	for _, h := range ix.Search(context.Background(), q, k, nil) {
		out = append(out, h.Model.Name)
	}
	return out
}

func TestLexicalRetrieverRanksSpanishFixture(t *testing.T) {
	ix := NewCatalogIndex(erpFixtureCatalog(), CatalogOptions{})
	cases := map[string]string{
		"¿Cuántos clientes tengo?":          "customers",
		"pedidos de venta de hoy":           "sales_orders",
		"Muéstrame las órdenes canceladas":  "sales_orders",
		"cuánto vendimos este mes":          "sales_orders",
		"existencias en la sucursal centro": "stock",
		"inventario de llantas":             "stock",
		"facturas pendientes":               "invoices",
		"FACTURA":                           "invoices",
		"sucursales":                        "branches",
		"artículos activos":                 "products",
		"productos":                         "products",
	}
	for q, want := range cases {
		got := searchNames(ix, q, 3)
		if len(got) == 0 || got[0] != want {
			t.Errorf("%q: want %s first, got %v", q, want, got)
		}
	}
	if got := searchNames(ix, "existencias en la sucursal centro", 3); !containsString(got, "branches") {
		t.Errorf("branches must be in the top 3: %v", got)
	}
	if got := searchNames(ix, "hola", 3); len(got) != 0 {
		t.Errorf("chitchat must retrieve nothing, got %v", got)
	}
}

func TestSearchFiltersBeforeCutting(t *testing.T) {
	ix := NewCatalogIndex(erpFixtureCatalog(), CatalogOptions{})
	hits := ix.Search(context.Background(), "existencias en la sucursal", 1, func(m CatalogModel) bool { return m.Name != "stock" })
	if len(hits) != 1 || hits[0].Model.Name == "stock" {
		t.Fatalf("a hidden model must not take the slot: %+v", hits)
	}
}

func TestStemLiteMergesSpanishInflections(t *testing.T) {
	pairs := [][2]string{{"clientes", "cliente"}, {"sucursales", "sucursal"}, {"facturas", "factura"}, {"órdenes", "orden"}, {"productos", "producto"}}
	for _, p := range pairs {
		a, b := catalogStems(p[0]), catalogStems(p[1])
		if len(a) != 1 || len(b) != 1 || a[0] != b[0] {
			t.Errorf("%s / %s: %v vs %v", p[0], p[1], a, b)
		}
	}
	if got := catalogTokens("salesOrder_line.total"); strings.Join(got, ",") != "sales,order,line,total" {
		t.Errorf("tokens: %v", got)
	}
}

// conceptEmbedder embeds by concept counts, so tests control similarity
// without a network: dim 0 = stock concepts, 1 = customers, 2 = sales.
type conceptEmbedder struct {
	calls int
	texts int
	fail  bool
}

func (e *conceptEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.calls++
	e.texts += len(texts)
	if e.fail {
		return nil, errors.New("down")
	}
	concepts := [][]string{{"inventario", "mercancia", "disponible", "bodega"}, {"cliente"}, {"venta", "pedido"}}
	out := make([][]float32, len(texts))
	for i, s := range texts {
		f := foldText(s)
		v := make([]float32, len(concepts))
		for d, words := range concepts {
			for _, w := range words {
				v[d] += float32(strings.Count(f, w))
			}
		}
		out[i] = v
	}
	return out, nil
}

func TestHybridRetrieverFusesEmbeddingsAndCachesCards(t *testing.T) {
	emb := &conceptEmbedder{}
	ix := NewCatalogIndex(erpFixtureCatalog(), CatalogOptions{Embedder: emb})
	// No lexical overlap: only the embedding ranking can find stock.
	if lex := searchNames(NewCatalogIndex(erpFixtureCatalog(), CatalogOptions{}), "mercancía en bodega", 3); len(lex) != 0 {
		t.Fatalf("precondition: lexical must miss, got %v", lex)
	}
	if got := searchNames(ix, "mercancía en bodega", 3); len(got) == 0 || got[0] != "stock" {
		t.Fatalf("hybrid must find stock, got %v", got)
	}
	cardTexts := emb.texts - 1 // minus the query
	if cardTexts != 6 {
		t.Fatalf("every card embedded once, got %d", cardTexts)
	}
	_ = searchNames(ix, "pedidos de clientes", 3)
	if emb.texts != cardTexts+2 {
		t.Fatalf("cards must come from the cache; embedded %d texts", emb.texts)
	}
	// Repeated query: cached too.
	_ = searchNames(ix, "pedidos de clientes", 3)
	if emb.texts != cardTexts+2 {
		t.Fatalf("query embedding must be cached; embedded %d texts", emb.texts)
	}
}

func TestHybridRetrieverReembedsOnlyChangedCards(t *testing.T) {
	emb := &conceptEmbedder{}
	h := NewHybridRetriever(emb, 0)
	docs := make([]CatalogDoc, 0)
	for _, m := range NewCatalogIndex(erpFixtureCatalog(), CatalogOptions{}).Models() {
		docs = append(docs, CatalogDoc{Model: m, Card: RenderCard(m, CardOptions{})})
	}
	ctx := context.Background()
	_ = h.Index(ctx, docs)
	if _, err := h.Retrieve(ctx, "inventario", 3); err != nil {
		t.Fatal(err)
	}
	before := emb.texts
	docs[0].Card += " | extra:str"
	_ = h.Index(ctx, docs)
	if _, err := h.Retrieve(ctx, "inventario", 3); err != nil {
		t.Fatal(err)
	}
	if emb.texts-before != 1 {
		t.Fatalf("only the changed card is re-embedded, got %d texts", emb.texts-before)
	}
}

func TestHybridRetrieverFallsBackToLexical(t *testing.T) {
	ix := NewCatalogIndex(erpFixtureCatalog(), CatalogOptions{Embedder: &conceptEmbedder{fail: true}})
	if got := searchNames(ix, "facturas pendientes", 3); len(got) == 0 || got[0] != "invoices" {
		t.Fatalf("embedder down must keep lexical results, got %v", got)
	}
}

func TestFuseRRF(t *testing.T) {
	m := func(n string) CatalogHit { return CatalogHit{Model: CatalogModel{Name: n}} }
	got := FuseRRF([]CatalogHit{m("x"), m("y")}, []CatalogHit{m("y"), m("z")})
	var names []string
	for _, h := range got {
		names = append(names, h.Model.Name)
	}
	if strings.Join(names, ",") != "y,x,z" {
		t.Fatalf("rrf order: %v", names)
	}
}

// ---------------------------------------------------------------------------
// Query tree
// ---------------------------------------------------------------------------

func TestValidateQueryTreeNormalizes(t *testing.T) {
	m := erpFixtureModel("sales_orders")
	got, err := ValidateQueryTree(m, QueryTree{
		Model: "SalesOrder",
		Where: []QueryCondition{
			{"cliente", "=", "c1"},
			{"Status", "in", []any{"borrador", "cancelled"}},
			{"status", "!=", "Confirmado"},
			{"total", ">", "100"},
			{"folio", "like", "PV-1"},
			{"notes", "==", nil},
		},
		Order: "-total",
		Limit: 500,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != "sales_orders" || got.Limit != MaxQueryTreeLimit || got.Order != "total desc" {
		t.Fatalf("normalized: %+v", got)
	}
	w := got.Where
	if w[0].Field != "customer_id" {
		t.Errorf("label Cliente must resolve to customer_id, got %s", w[0].Field)
	}
	if l, _ := w[1].Value.([]any); w[1].Field != "status" || len(l) != 2 || l[0] != "draft" {
		t.Errorf("enum labels map to values: %+v", w[1])
	}
	if w[2].Value != "confirmed" || w[3].Value != 100.0 || w[4].Value != "%PV-1%" || w[5].Op != "is null" {
		t.Errorf("values: %+v", w)
	}
	if d, _ := ValidateQueryTree(m, QueryTree{Model: "sales_orders"}); d.Limit != DefaultQueryTreeLimit {
		t.Errorf("default limit: %d", d.Limit)
	}
}

func TestValidateQueryTreeSuggests(t *testing.T) {
	m := erpFixtureModel("sales_orders")
	_, err := ValidateQueryTree(m, QueryTree{Model: "sales_orders", Where: []QueryCondition{
		{"clienet", "=", "x"},
		{"status", "=", "perdido"},
		{"total", "ilike", "x"},
		{"folio", "~", "x"},
	}})
	var qe *QueryTreeError
	if !errors.As(err, &qe) || len(qe.Problems) != 4 {
		t.Fatalf("want 4 problems, got %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"¿quisiste decir `customer_id` (Cliente)", "draft=Borrador", "solo aplica a campos de texto", "operador \"~\" no soportado"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error misses %q:\n%s", want, msg)
		}
	}
}

func TestValidateQueryTreeAggregates(t *testing.T) {
	m := erpFixtureModel("sales_orders")
	got, err := ValidateQueryTree(m, QueryTree{Model: "sales_orders", GroupBy: []string{"status"}, Order: "count desc"})
	if err != nil || len(got.Aggregates) != 1 || got.Aggregates[0].As != "count" || got.Order != "count desc" {
		t.Fatalf("group_by alone counts: %+v %v", got, err)
	}
	got, err = ValidateQueryTree(m, QueryTree{Model: "sales_orders", Aggregates: []QueryAggregate{{Fn: "SUM", Field: "total"}}})
	if err != nil || got.Aggregates[0].As != "sum_total" {
		t.Fatalf("default alias: %+v %v", got, err)
	}
	for _, bad := range []QueryTree{
		{Aggregates: []QueryAggregate{{Fn: "sum", Field: "folio"}}},
		{Aggregates: []QueryAggregate{{Fn: "median", Field: "total"}}},
		{Aggregates: []QueryAggregate{{Fn: "avg"}}},
		{Aggregates: []QueryAggregate{{Fn: "count"}}, Fields: []string{"folio"}},
		{Aggregates: []QueryAggregate{{Fn: "count"}}, Order: "folio"},
	} {
		bad.Model = "sales_orders"
		if _, err := ValidateQueryTree(m, bad); err == nil {
			t.Errorf("must fail: %+v", bad)
		}
	}
}

func TestParseQueryTree(t *testing.T) {
	tr, err := ParseQueryTree(map[string]any{
		"model": "sales_orders", "where": []any{"status", "=", "draft"}, "fields": "folio, total",
		"limit": 5.0, "order": nil, "group_by": nil,
	})
	if err != nil || len(tr.Where) != 1 || tr.Where[0].Op != "=" || len(tr.Fields) != 2 || tr.Limit != 5 {
		t.Fatalf("parse: %+v %v", tr, err)
	}
	tr, err = ParseQueryTree(map[string]any{"model": "x", "where": `[["total",">",10],{"field":"status","op":"is null"}]`})
	if err != nil || len(tr.Where) != 2 || tr.Where[1].Op != "is null" {
		t.Fatalf("string where: %+v %v", tr, err)
	}
	if _, err := ParseQueryTree(map[string]any{"model": "x", "filters": map[string]any{}}); err == nil || !strings.Contains(err.Error(), "filters") {
		t.Fatalf("unknown keys must be rejected: %v", err)
	}
	b, _ := json.Marshal(QueryCondition{"a", "is null", nil})
	if string(b) != `["a","is null"]` {
		t.Fatalf("marshal: %s", b)
	}
}

func TestQueryTreeToLegacyPorts(t *testing.T) {
	m := erpFixtureModel("sales_orders")
	tr, _ := ValidateQueryTree(m, QueryTree{Model: "sales_orders", Where: []QueryCondition{{"status", "=", "draft"}}, Order: "total desc", Search: "PV"})
	req, err := tr.ToQueryRequest()
	if err != nil || req.Model != "sales_orders" || req.Filters["status"] != "draft" || req.OrderBy != "total desc" || req.Limit != 20 || req.Search != "PV" {
		t.Fatalf("query request: %+v %v", req, err)
	}
	tr, _ = ValidateQueryTree(m, QueryTree{Model: "sales_orders", Where: []QueryCondition{{"total", ">", 5}}})
	if _, err := tr.ToQueryRequest(); err == nil {
		t.Fatal("> is not expressible in the legacy port")
	}
	tr, _ = ValidateQueryTree(m, QueryTree{Model: "sales_orders", GroupBy: []string{"status"}, Aggregates: []QueryAggregate{{Fn: "sum", Field: "total"}}})
	agg, err := tr.ToAggregateRequest()
	if err != nil || agg.Function != "sum" || agg.Field != "total" || agg.GroupBy != "status" {
		t.Fatalf("aggregate request: %+v %v", agg, err)
	}
}

func TestQueryTreeSchemaIsStrictFriendly(t *testing.T) {
	s := QueryTreeSchema(true)
	if s["additionalProperties"] != false || len(s["required"].([]string)) != 9 {
		t.Fatalf("strict schema: %+v", s)
	}
	props := s["properties"].(map[string]any)
	if ty, _ := props["fields"].(map[string]any)["type"].([]string); len(ty) != 2 || ty[1] != "null" {
		t.Fatalf("optional must be nullable: %v", props["fields"])
	}
	reg := tools.NewToolRegistry()
	for _, tl := range (CatalogTools{}).Tools() {
		reg.Register(tl)
	}
	for _, def := range reg.ToOpenAIFormat() {
		params := def["parameters"].(map[string]any)
		if params["additionalProperties"] != false {
			t.Errorf("%s must keep additionalProperties false", def["name"])
		}
		if def["name"] == "query_data" {
			if req := params["required"].([]string); len(req) != 1 || req[0] != "model" {
				t.Errorf("portable query_data requires only model: %v", req)
			}
		}
	}
	if !strings.Contains(QueryDataDescription, `"group_by"`) || strings.Count(QueryDataDescription, `{"model"`) != 3 {
		t.Fatal("description carries 3 examples")
	}
}

// ---------------------------------------------------------------------------
// Compact results
// ---------------------------------------------------------------------------

func fixtureRows() []map[string]any {
	return []map[string]any{
		{"id": "o1", "folio": "PV-1", "customer_id": "c1", "status": "confirmed", "total": 1500.0, "order_date": "2026-11-15T00:00:00Z", "notes": "x"},
		{"id": "o2", "folio": "PV-2", "customer_id": "c2", "status": "draft", "total": 99.5, "order_date": "2026-11-14T00:00:00Z"},
		{"id": "o3", "folio": "PV|3", "customer_id": nil, "status": "cancelled", "total": 0.0},
	}
}

func TestRenderRowsTable(t *testing.T) {
	m := erpFixtureModel("sales_orders")
	got := RenderRowsTable(m, fixtureRows(), TableOptions{Total: 45, RefNames: map[string]map[string]string{"customer_id": {"c1": "Juan Pérez"}}})
	for _, want := range []string{"| id | folio | Cliente | Estado | total | Fecha |", "Juan Pérez [c1]", "| c2 |", "Confirmado", "2026-11-15", "99.50", "PV/3", "1–3 de 45. next: offset=3"} {
		if !strings.Contains(got, want) {
			t.Errorf("table misses %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Notas") {
		t.Errorf("long text columns are not auto-picked:\n%s", got)
	}
	empty := RenderRowsTable(m, nil, TableOptions{})
	if !strings.Contains(empty, "0 registros") {
		t.Fatal(empty)
	}
}

func TestRenderRowsTableRespectsByteCap(t *testing.T) {
	m := erpFixtureModel("sales_orders")
	var rows []map[string]any
	for i := 0; i < 200; i++ {
		rows = append(rows, map[string]any{"id": strings.Repeat("x", 36), "folio": strings.Repeat("F", 40), "status": "draft", "total": 1.0})
	}
	got := RenderRowsTable(m, rows, TableOptions{MaxRows: 200, MaxBytes: 2000, Total: 200})
	if len(got) > 2000 || !strings.Contains(got, "next: offset=") {
		t.Fatalf("cap %d bytes:\n%s", len(got), got)
	}
}

func TestRenderRecordKVAndAggregates(t *testing.T) {
	m := erpFixtureModel("sales_orders")
	kv := RenderRecordKV(m, fixtureRows()[0], map[string]map[string]string{"customer_id": {"c1": "Juan Pérez"}}, 0)
	for _, want := range []string{"Pedidos de venta (sales_orders) [o1]", "- Cliente: Juan Pérez [c1]", "- Estado: Confirmado", "- total: 1500"} {
		if !strings.Contains(kv, want) {
			t.Errorf("kv misses %q:\n%s", want, kv)
		}
	}
	tr := QueryTree{GroupBy: []string{"status"}, Aggregates: []QueryAggregate{{Fn: "count", As: "count"}}}
	agg := RenderAggregateTable(m, tr, []map[string]any{{"status": "draft", "count": int64(3)}}, nil, 0)
	if !strings.Contains(agg, "| Estado | count |") || !strings.Contains(agg, "| Borrador | 3 |") {
		t.Fatalf("aggregate table:\n%s", agg)
	}
	one := RenderAggregateTable(m, QueryTree{Aggregates: []QueryAggregate{{Fn: "sum", Field: "total", As: "total"}}}, []map[string]any{{"total": 1599.5}}, nil, 0)
	if one != "Pedidos de venta (sales_orders): total = 1599.50" {
		t.Fatalf("scalar aggregate: %s", one)
	}
}

func TestLLMTexterReplacesJSON(t *testing.T) {
	r := tools.ToolResult{ToolName: "query_data", Success: true, Result: &QueryResult{Model: "x", Text: "| a |"}}
	if got, _ := r.FormatForLLMLimit(0); got != "Result of query_data:\n| a |" {
		t.Fatalf("text result: %q", got)
	}
	r.Result = &QueryResult{Model: "x"}
	if got, _ := r.FormatForLLMLimit(0); !strings.Contains(got, `"model":"x"`) {
		t.Fatalf("no text falls back to JSON: %q", got)
	}
	if got := (tools.ToolResult{ToolName: "t", Success: true, Result: &TextResult{Text: "hola"}}).FormatForLLM(); got != "Result of t:\nhola" {
		t.Fatalf("FormatForLLM: %q", got)
	}
}
