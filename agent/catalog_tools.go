package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/asteby/metacore-kernel/agent/contextmgr"
	"github.com/asteby/metacore-kernel/agent/tools"
)

// Kernel-owned data tools of the semantic catalog. They replace the host's
// list_models / describe_model / query_data for copilot turns when
// CopilotConfig.SemanticCatalog is on, and add catalog_search. All of them
// go through CRUDService (permissions, module gate, column read masks) and
// answer with compact text: ERP-Cards, one-line listings, Markdown tables.

// CatalogTools builds the catalog tool set over a CRUDService and an index.
type CatalogTools struct {
	CRUD  *CRUDService
	Index *CatalogIndex
	// Identity maps the conversation to the caller. Default: the
	// conversation's organization and user (the copilot's human).
	Identity func(*contextmgr.ConversationContext) *AgentIdentity
	// StrictSchema renders query_data's schema for strict structured-output
	// providers: every property required, optional ones nullable.
	StrictSchema bool
	// MaxResultBytes is the text budget of each result (default
	// DefaultCompactMaxBytes).
	MaxResultBytes int
}

// Tools returns catalog_search, list_models, describe_model and query_data.
func (c CatalogTools) Tools() []tools.BaseTool {
	return []tools.BaseTool{
		&catalogSearchTool{c},
		&catalogListModelsTool{c},
		&catalogDescribeModelTool{c},
		&catalogQueryTool{c},
	}
}

func (c CatalogTools) identity(agentCtx *contextmgr.ConversationContext) *AgentIdentity {
	if c.Identity != nil {
		return c.Identity(agentCtx)
	}
	if agentCtx == nil {
		return &AgentIdentity{}
	}
	return &AgentIdentity{AgentID: agentCtx.AgentID, OrganizationID: agentCtx.OrganizationID, UserID: agentCtx.UserID}
}

func (c CatalogTools) maxBytes() int {
	if c.MaxResultBytes > 0 {
		return c.MaxResultBytes
	}
	return DefaultCompactMaxBytes
}

// TextResult is a tool result that is only text for the model.
type TextResult struct {
	Text string `json:"text"`
}

// LLMText implements tools.LLMTexter.
func (t *TextResult) LLMText() string {
	if t == nil {
		return ""
	}
	return t.Text
}

// visibleModel reports whether identity may read m, and m without the
// columns it may not see.
func (s *CRUDService) visibleModel(identity *AgentIdentity, m CatalogModel) (CatalogModel, bool) {
	if s.CheckPermission(identity, m.Name, "index") != nil {
		return m, false
	}
	return m.withoutFields(s.readMaskFor(identity, m.Name)), true
}

// SearchCards returns the ERP-Cards of the k models most relevant to query
// that identity may read, with at most fields fields each (0 = index
// default).
func (s *CRUDService) SearchCards(ctx context.Context, identity *AgentIdentity, ix *CatalogIndex, query string, k, fields int) []string {
	if ix == nil {
		return nil
	}
	if fields <= 0 {
		fields = ix.opts.CardFields
	}
	hits := ix.Search(ctx, query, k, func(m CatalogModel) bool {
		_, ok := s.visibleModel(identity, m)
		return ok
	})
	cards := make([]string, 0, len(hits))
	for _, h := range hits {
		m, _ := s.visibleModel(identity, h.Model)
		cards = append(cards, RenderCard(m, CardOptions{MaxFields: fields}))
	}
	return cards
}

// ListModelsCompact lists the models identity may read, one line each:
// `name "Etiqueta"`. Details come from catalog_search / describe_model.
func (s *CRUDService) ListModelsCompact(identity *AgentIdentity, ix *CatalogIndex) string {
	var models []CatalogModel
	if ix != nil {
		models = ix.Models()
	} else {
		models = NewCatalogIndex(s.cfg.Catalog, CatalogOptions{}).Models()
	}
	var b strings.Builder
	n := 0
	for _, m := range models {
		if s.CheckPermission(identity, m.Name, "index") != nil {
			continue
		}
		n++
		b.WriteString(m.Name)
		if m.Label != "" {
			b.WriteString(` "` + m.Label + `"`)
		}
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "%d modelos. Campos: catalog_search(consulta) o describe_model(model).", n)
	return b.String()
}

// DescribeModelCard renders one model as a full ERP-Card plus what can be
// filtered and searched, without the columns identity may not read.
func (s *CRUDService) DescribeModelCard(identity *AgentIdentity, ix *CatalogIndex, model string) (string, error) {
	key := s.resolve(model)
	if err := guardResolvedModel(model, key); err != nil {
		return "", err
	}
	if err := s.CheckPermission(identity, key, "index"); err != nil {
		return "", err
	}
	var cm CatalogModel
	var ok bool
	if ix != nil {
		cm, ok = ix.Model(key)
	}
	if !ok {
		meta, found := s.cfg.Catalog.Lookup(key)
		if !found {
			return "", s.unknownModelError(model, QueryTreeOptions{Catalog: ix})
		}
		cm = BuildCatalogModel(s.cfg.Catalog, meta, nil)
	}
	cm = cm.withoutFields(s.readMaskFor(identity, key))
	var b strings.Builder
	b.WriteString(RenderCard(cm, CardOptions{MaxEnum: 20}))
	var filterable []string
	for _, f := range cm.Fields {
		if f.Filterable {
			filterable = append(filterable, f.Name)
		}
	}
	if len(filterable) > 0 {
		b.WriteString("\nFiltros de la vista: " + strings.Join(filterable, ", "))
	}
	if len(cm.SearchIn) > 0 {
		b.WriteString("\nsearch busca en: " + strings.Join(cm.SearchIn, ", "))
	}
	b.WriteString("\n" + queryOpsLine)
	return b.String(), nil
}

// queryOpsLine says which where operators apply to which card types.
const queryOpsLine = "Ops: str/text: = != in like ilike | int/num/money/date/dt: = != > >= < <= between in | enum/->M/bool/id: = != in \"not in\" | todos: \"is null\" \"is not null\"."

// ---------------------------------------------------------------------------

type catalogSearchTool struct{ c CatalogTools }

func (t *catalogSearchTool) Name() string     { return "catalog_search" }
func (t *catalogSearchTool) Category() string { return "data" }
func (t *catalogSearchTool) Description() string {
	return "Encuentra los modelos de datos que corresponden a una petición (ej. \"pedidos de venta\", \"existencias por sucursal\") y devuelve sus tarjetas ERP-Card con campos reales. Úsala cuando el modelo no venga en [Catálogo relevante]; con la tarjeta llama query_data directo."
}
func (t *catalogSearchTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{"type": "string", "description": "Qué datos buscas, en palabras del usuario"},
			"k":     map[string]any{"type": "integer", "description": "Cuántos modelos (1-5, default 3)"},
		},
		"required":             []string{"query"},
		"additionalProperties": false,
	}
}
func (t *catalogSearchTool) Execute(ctx context.Context, params map[string]any, agentCtx *contextmgr.ConversationContext) (any, error) {
	query := strings.TrimSpace(strParamAny(params, "query"))
	if query == "" {
		return nil, fmt.Errorf("query es requerido")
	}
	k := intParamAny(params, "k", 3)
	if k < 1 {
		k = 1
	}
	if k > 5 {
		k = 5
	}
	cards := t.c.CRUD.SearchCards(ctx, t.c.identity(agentCtx), t.c.Index, query, k, 0)
	if len(cards) == 0 {
		return &TextResult{Text: "Sin modelos para \"" + query + "\". Prueba otras palabras o list_models."}, nil
	}
	return &TextResult{Text: clipBytes(strings.Join(cards, "\n"), t.c.maxBytes())}, nil
}

type catalogListModelsTool struct{ c CatalogTools }

func (t *catalogListModelsTool) Name() string     { return "list_models" }
func (t *catalogListModelsTool) Category() string { return "data" }
func (t *catalogListModelsTool) Description() string {
	return "Lista los modelos de datos disponibles (nombre y etiqueta, uno por línea). Para campos usa catalog_search o describe_model."
}
func (t *catalogListModelsTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}
}
func (t *catalogListModelsTool) Execute(_ context.Context, _ map[string]any, agentCtx *contextmgr.ConversationContext) (any, error) {
	return &TextResult{Text: clipBytes(t.c.CRUD.ListModelsCompact(t.c.identity(agentCtx), t.c.Index), t.c.maxBytes())}, nil
}

type catalogDescribeModelTool struct{ c CatalogTools }

func (t *catalogDescribeModelTool) Name() string     { return "describe_model" }
func (t *catalogDescribeModelTool) Category() string { return "data" }
func (t *catalogDescribeModelTool) Description() string {
	return "Tarjeta ERP-Card completa de un modelo (todos los campos, valores de enum, filtros y operadores). Solo si la tarjeta que ya tienes no trae el campo que necesitas."
}
func (t *catalogDescribeModelTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"model": map[string]any{"type": "string", "description": "Nombre real del modelo (ej. sales_orders)"},
		},
		"required":             []string{"model"},
		"additionalProperties": false,
	}
}
func (t *catalogDescribeModelTool) Execute(_ context.Context, params map[string]any, agentCtx *contextmgr.ConversationContext) (any, error) {
	model := strings.TrimSpace(strParamAny(params, "model"))
	if model == "" {
		return nil, fmt.Errorf("model es requerido")
	}
	text, err := t.c.CRUD.DescribeModelCard(t.c.identity(agentCtx), t.c.Index, model)
	if err != nil {
		return nil, err
	}
	return &TextResult{Text: clipBytes(text, t.c.maxBytes())}, nil
}

type catalogQueryTool struct{ c CatalogTools }

func (t *catalogQueryTool) Name() string     { return "query_data" }
func (t *catalogQueryTool) Category() string { return "data" }
func (t *catalogQueryTool) Description() string {
	return QueryDataDescription
}
func (t *catalogQueryTool) Parameters() map[string]any { return QueryTreeSchema(t.c.StrictSchema) }
func (t *catalogQueryTool) Execute(ctx context.Context, params map[string]any, agentCtx *contextmgr.ConversationContext) (any, error) {
	tree, err := ParseQueryTree(params)
	if err != nil {
		return nil, err
	}
	return t.c.CRUD.QueryTree(ctx, t.c.identity(agentCtx), tree, QueryTreeOptions{Catalog: t.c.Index, MaxBytes: t.c.maxBytes()})
}

// QueryDataDescription is the description of the query-tree query_data tool,
// with three compact examples.
const QueryDataDescription = `Lee datos de un modelo (registros, conteos, totales, agrupados) en UNA llamada. Usa los nombres reales de la tarjeta ERP-Card.
where: lista de [campo, op, valor]; ops: = != > >= < <= in "not in" like ilike between "is null" "is not null". Fechas YYYY-MM-DD (en dt cubre el día). limit default 20 (máx 200); el resultado trae "next: offset=N" si hay más.
Ejemplos:
{"model":"sales_orders","where":[["status","=","confirmed"],["created_at",">=","2026-11-01"]],"order":"total desc","limit":5}
{"model":"customers","search":"juan","fields":["name","email","phone"]}
{"model":"invoices","where":[["state","in",["posted","paid"]]],"group_by":["customer_id"],"aggregates":[{"fn":"sum","field":"amount_total","as":"total"}],"order":"total desc","limit":10}`

// QueryTreeSchema is the JSON Schema of query_data. Both forms set
// additionalProperties false on every object. strict lists every property in
// required and makes the optional ones nullable (OpenAI strict mode); the
// default form only requires model, which every provider accepts.
func QueryTreeSchema(strict bool) map[string]any {
	opt := func(s map[string]any) map[string]any {
		if !strict {
			return s
		}
		out := map[string]any{}
		for k, v := range s {
			out[k] = v
		}
		if t, ok := s["type"].(string); ok {
			out["type"] = []string{t, "null"}
		}
		return out
	}
	scalar := []any{
		map[string]any{"type": "string"},
		map[string]any{"type": "number"},
		map[string]any{"type": "boolean"},
		map[string]any{"type": "null"},
		map[string]any{"type": "array", "items": map[string]any{"anyOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "number"}}}},
	}
	condition := map[string]any{
		"type":        "array",
		"description": "[campo, op, valor]",
		"items":       map[string]any{"anyOf": scalar},
	}
	if !strict {
		condition["minItems"], condition["maxItems"] = 2, 3
	}
	aggRequired := []string{"fn"}
	aggField := map[string]any{"type": "string", "description": "Campo (vacío para count)"}
	aggAs := map[string]any{"type": "string", "description": "Alias de la columna"}
	if strict {
		aggRequired = []string{"fn", "field", "as"}
		aggField = opt(aggField)
		aggAs = opt(aggAs)
	}
	props := map[string]any{
		"model":  map[string]any{"type": "string", "description": "Nombre real del modelo (ej. sales_orders)"},
		"fields": opt(map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Columnas a mostrar (vacío = las principales)"}),
		"where":  opt(map[string]any{"type": "array", "items": condition, "description": "Condiciones AND"}),
		"order":  opt(map[string]any{"type": "string", "description": "\"campo asc|desc\" o un alias de aggregates"}),
		"limit":  opt(map[string]any{"type": "integer", "description": "Filas (default 20, máx 200)"}),
		"offset": opt(map[string]any{"type": "integer", "description": "Para paginar (next: offset=N)"}),
		"group_by": opt(map[string]any{"type": "array", "items": map[string]any{"type": "string"},
			"description": "Agrupa por estos campos (con aggregates)"}),
		"aggregates": opt(map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"fn":    map[string]any{"type": "string", "enum": []string{"count", "count_distinct", "sum", "avg", "min", "max"}},
					"field": aggField,
					"as":    aggAs,
				},
				"required":             aggRequired,
				"additionalProperties": false,
			},
			"description": "Conteos y totales",
		}),
		"search": opt(map[string]any{"type": "string", "description": "Texto libre sobre las columnas de búsqueda"}),
	}
	required := []string{"model"}
	if strict {
		required = []string{"aggregates", "fields", "group_by", "limit", "model", "offset", "order", "search", "where"}
	}
	return map[string]any{
		"type":                 "object",
		"properties":           props,
		"required":             required,
		"additionalProperties": false,
	}
}

func strParamAny(params map[string]any, key string) string {
	v, _ := params[key].(string)
	return v
}

func intParamAny(params map[string]any, key string, def int) int {
	switch v := params[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return def
}
