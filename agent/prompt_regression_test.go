package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/agent/contextmgr"
	"github.com/asteby/metacore-kernel/agent/tools"
	"github.com/google/uuid"
)

// Prompt budget regression: a realistic copilot turn (19 tools, 60 guides, 40
// models) must keep the cacheable prefix (system prompt + tool definitions)
// small and byte-identical across turns, whatever the clock and the guides
// the browser sends. Before the diet the same fixture cost ~3x more and
// changed every minute.

// fixtureTool is a host-like tool with a realistic description and schema.
type fixtureTool struct {
	name, desc string
	params     map[string]interface{}
}

func (f fixtureTool) Name() string                       { return f.name }
func (f fixtureTool) Description() string                { return f.desc }
func (f fixtureTool) Category() string                   { return "data" }
func (f fixtureTool) Parameters() map[string]interface{} { return f.params }
func (f fixtureTool) Execute(context.Context, map[string]interface{}, *contextmgr.ConversationContext) (interface{}, error) {
	return nil, nil
}

func p(typ, desc string, required bool) map[string]interface{} {
	return map[string]interface{}{"type": typ, "description": desc, "required": required}
}

// fixtureHostTools are 12 CRUD/navigation tools shaped like a host's.
func fixtureHostTools() []tools.BaseTool {
	model := p("string", "Clave exacta del modelo (models[].model de list_models), p. ej. customers.", true)
	return []tools.BaseTool{
		fixtureTool{"list_models", "Lista los modelos de datos que el usuario puede consultar, con su clave y título. Úsala cuando no sepas qué modelo corresponde a lo que pide.", map[string]interface{}{
			"search": p("string", "Texto para filtrar modelos por nombre o título.", false)}},
		fixtureTool{"describe_model", "Describe las columnas de un modelo: nombre, tipo, si es requerida, opciones de enums y relaciones. Úsala antes de filtrar o proponer cambios si no conoces los campos.", map[string]interface{}{
			"model": model}},
		fixtureTool{"query_data", "Consulta registros de un modelo con filtros, búsqueda de texto, orden y límite. Devuelve filas con las columnas visibles para el usuario. Para totales o conteos usa aggregate_data.", map[string]interface{}{
			"model":    model,
			"filters":  map[string]interface{}{"type": "object", "description": "Filtros campo → valor u operador ({\"gte\":…}). Fechas en ISO."},
			"search":   p("string", "Búsqueda de texto libre en las columnas buscables.", false),
			"order_by": p("string", "Columna de orden; prefijo - para descendente.", false),
			"limit":    p("integer", "Máximo de filas (por defecto 20, máximo 100).", false),
			"page":     p("integer", "Página (desde 1).", false)}},
		fixtureTool{"aggregate_data", "Calcula conteos, sumas, promedios, mínimos o máximos de un modelo, opcionalmente agrupados por una columna o por periodo de fecha.", map[string]interface{}{
			"model":     model,
			"operation": map[string]interface{}{"type": "string", "enum": []string{"count", "sum", "avg", "min", "max"}, "description": "Operación.", "required": true},
			"field":     p("string", "Columna numérica (no aplica a count).", false),
			"group_by":  p("string", "Columna o periodo (day, week, month) para agrupar.", false),
			"filters":   map[string]interface{}{"type": "object", "description": "Mismos filtros que query_data."}}},
		fixtureTool{"get_record", "Obtiene un registro completo por id, con sus relaciones principales.", map[string]interface{}{
			"model": model, "record_id": p("string", "Id del registro.", true)}},
		fixtureTool{"read_column", "Lee el valor completo de una columna larga (texto, JSON) de un registro, cuando query_data la recortó.", map[string]interface{}{
			"model": model, "record_id": p("string", "Id del registro.", true), "column": p("string", "Nombre de la columna.", true)}},
		fixtureTool{"propose_create", "Propone crear un registro. NO lo crea: muestra una tarjeta de aprobación y el usuario decide. Llena solo campos que el usuario dio o que se deducen sin duda.", map[string]interface{}{
			"model": model, "data": map[string]interface{}{"type": "object", "description": "Campos → valores del nuevo registro.", "required": true}}},
		fixtureTool{"propose_update", "Propone modificar campos de un registro existente; el usuario aprueba en el chat antes de aplicarlo.", map[string]interface{}{
			"model": model, "record_id": p("string", "Id del registro.", true), "data": map[string]interface{}{"type": "object", "description": "Solo los campos que cambian.", "required": true}}},
		fixtureTool{"propose_delete", "Propone eliminar un registro; requiere aprobación explícita del usuario.", map[string]interface{}{
			"model": model, "record_id": p("string", "Id del registro.", true), "reason": p("string", "Motivo breve que verá el usuario.", false)}},
		fixtureTool{"propose_batch", "Propone varias altas o cambios del mismo modelo en una sola aprobación (máximo 20).", map[string]interface{}{
			"model": model, "items": map[string]interface{}{"type": "array", "description": "Lista de {action, record_id?, data}.", "required": true}}},
		fixtureTool{"navigate_to", "Abre una pantalla de la aplicación (lista, registro o reporte) en el navegador del usuario.", map[string]interface{}{
			"model": model, "record_id": p("string", "Id para abrir el detalle.", false)}},
		fixtureTool{"run_action", "Ejecuta una acción declarada por un módulo (p. ej. timbrar factura) sobre un registro. Las acciones con riesgo piden aprobación.", map[string]interface{}{
			"action": p("string", "Id de la acción.", true), "model": model, "record_id": p("string", "Id del registro.", true)}},
	}
}

// fixtureRegistry is the 17 base tools; the turn adds start_guide and
// search_guides (19 in total).
func fixtureRegistry() *tools.ToolRegistry {
	reg := tools.NewToolRegistry()
	for _, t := range []tools.BaseTool{
		NewFinalAnswerTool(), NewShowChartTool(), NewGetConversationContextTool(),
		NewSearchKnowledgeTool(nil), NewGenerateImageTool(nil),
	} {
		reg.Register(t)
	}
	for _, t := range fixtureHostTools() {
		reg.Register(t)
	}
	return reg
}

// fixtureGuides is a browser catalog of n guides; variant changes the ids and
// texts, as different users/screens/deploys would.
func fixtureGuides(n int, variant string) []GuideDescriptor {
	verbs := []string{"Crear", "Editar", "Ver", "Exportar", "Importar"}
	out := make([]GuideDescriptor, n)
	for i := range out {
		v := verbs[i%len(verbs)]
		out[i] = GuideDescriptor{
			ID:          fmt.Sprintf("synth.%s.%s.model_%02d", variant, strings.ToLower(v), i),
			Title:       fmt.Sprintf("%s registro de Modelo %02d (%s)", v, i, variant),
			Description: fmt.Sprintf("Te guío paso a paso para %s un registro de Modelo %02d desde el menú lateral, llenando los campos obligatorios y guardando los cambios al final.", strings.ToLower(v), i),
		}
	}
	return out
}

// fixtureCatalog has 40 models.
func fixtureCatalog() fakeCatalog {
	c := fakeCatalog{}
	for i := 0; i < 40; i++ {
		c.models = append(c.models, ModelMeta{Key: fmt.Sprintf("model_%02d", i), Table: fmt.Sprintf("model_%02d", i), Title: fmt.Sprintf("Modelo %02d", i)})
	}
	return c
}

// fixtureBasePrompt is a host base prompt of realistic size (~2.3k chars).
var fixtureBasePrompt = "Eres Aby, la asistente integrada de la plataforma. Ayudas a las personas de la organización a consultar sus datos, " +
	"entender cifras y preparar cambios que ellas aprueban. " + strings.Repeat("Responde en español claro y breve; usa las herramientas para cualquier dato de la organización y nunca inventes registros, cifras ni fechas. ", 14)

func fixtureCopilot(t *testing.T, at time.Time, mut func(*CopilotConfig)) (*CopilotService, *AgentOrchestrator) {
	t.Helper()
	crud, err := NewCRUDService(CRUDConfig{Catalog: fixtureCatalog(), DevAllowAll: true})
	if err != nil {
		t.Fatal(err)
	}
	o := NewAgentOrchestrator(OrchestratorConfig{Tools: fixtureRegistry()})
	cfg := CopilotConfig{
		Orchestrator: o,
		Capabilities: &CapabilityRegistry{CRUD: crud},
		Brand:        fakeBrand{Personality{Name: "Aby", SystemPrompt: fixtureBasePrompt, PlatformName: "Pitsline", Addendum: "Taller de llantas: servicios, inventario de llantas y citas."}},
		Clock:        fixedClock{at},
	}
	if mut != nil {
		mut(&cfg)
	}
	svc, err := NewCopilotService(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return svc, o
}

// cacheablePrefix is exactly what a copilot turn sends as system prompt and
// tool definitions.
func cacheablePrefix(t *testing.T, svc *CopilotService, o *AgentOrchestrator, guides []GuideDescriptor) (system, toolsJSON string) {
	t.Helper()
	org, user := uuid.MustParse("00000000-0000-0000-0000-0000000000a1"), uuid.MustParse("00000000-0000-0000-0000-0000000000b2")
	prompt, _ := svc.SystemPrompt(org, user, false)
	reg := o.turnRegistry(WithGuideTurn(context.Background(), NewGuideTurn(guides)), Turn{})
	raw, err := json.Marshal(reg.ToOpenAIFormat())
	if err != nil {
		t.Fatal(err)
	}
	return o.turnSystemPrompt(prompt, reg), string(raw)
}

// legacyStartGuideTool reproduces the pre-diet start_guide (catalog listed in
// the description plus an enum of ids), for the "before" estimate.
type legacyStartGuideTool struct{ catalog []GuideDescriptor }

func (t legacyStartGuideTool) Name() string     { return "start_guide" }
func (t legacyStartGuideTool) Category() string { return "workflow" }
func (t legacyStartGuideTool) Description() string {
	var b strings.Builder
	b.WriteString("Inicia una guía visual paso a paso en la pantalla del usuario (ilumina el menú, los botones y los campos). ")
	b.WriteString("Úsala SOLO cuando el usuario quiere aprender o que le muestres cómo se hace algo en la interfaz. ")
	b.WriteString("Si pide que TÚ hagas la acción (crear, registrar, modificar… aunque todavía no dé los datos), NO la uses: usa las tools de datos. ")
	b.WriteString("Guías disponibles en esta sesión:")
	for _, g := range t.catalog {
		b.WriteString("\n- " + g.ID + ": " + g.Title)
		if g.Description != "" {
			b.WriteString(" — " + g.Description)
		}
	}
	return b.String()
}
func (t legacyStartGuideTool) Parameters() map[string]interface{} {
	ids := make([]string, 0, len(t.catalog))
	for _, g := range t.catalog {
		ids = append(ids, g.ID)
	}
	return map[string]interface{}{"guide_id": map[string]interface{}{
		"type": "string", "enum": ids, "description": "Id de la guía a iniciar (de la lista de guías disponibles).", "required": true}}
}
func (t legacyStartGuideTool) Execute(context.Context, map[string]interface{}, *contextmgr.ConversationContext) (interface{}, error) {
	return nil, nil
}

// legacyPrefix is what the same turn cost before: minute clock and the
// capability clause in the system prompt, every tool rendered twice (text +
// native) and the guide catalog inside start_guide.
func legacyPrefix(t *testing.T, at time.Time, guides []GuideDescriptor) (system, toolsJSON string) {
	t.Helper()
	svc, o := fixtureCopilot(t, at, func(c *CopilotConfig) { c.ClockInSystemPrompt, c.InjectCapabilities = true, true })
	org, user := uuid.MustParse("00000000-0000-0000-0000-0000000000a1"), uuid.MustParse("00000000-0000-0000-0000-0000000000b2")
	prompt, _ := svc.SystemPrompt(org, user, false)
	reg := o.toolRegistry.Clone()
	reg.Register(legacyStartGuideTool{NewGuideTurn(guides).catalog})
	raw, err := json.Marshal(reg.ToOpenAIFormat())
	if err != nil {
		t.Fatal(err)
	}
	return AppendToolsSection(prompt, reg), string(raw)
}

const maxCacheablePrefixTokens = 4000

func TestPromptBudgetRegression(t *testing.T) {
	t1 := time.Date(2026, 10, 12, 9, 30, 0, 0, mx)
	t2 := t1.Add(17 * time.Minute)
	guidesA, guidesB := fixtureGuides(60, "a"), fixtureGuides(35, "b")

	svc1, o1 := fixtureCopilot(t, t1, nil)
	svc2, o2 := fixtureCopilot(t, t2, nil)
	sys1, tools1 := cacheablePrefix(t, svc1, o1, guidesA)
	sys2, tools2 := cacheablePrefix(t, svc2, o2, guidesB)

	if sys1 != sys2 {
		t.Fatalf("system prompt differs across turns (clock/guides leaked in):\n%s\n---\n%s", sys1, sys2)
	}
	if tools1 != tools2 {
		t.Fatal("tool definitions differ across turns (guides leaked in)")
	}
	var defs []map[string]interface{}
	if err := json.Unmarshal([]byte(tools1), &defs); err != nil || len(defs) != 19 {
		t.Fatalf("fixture must offer 19 tools, got %d (%v)", len(defs), err)
	}

	after := EstimateTokens(sys1) + EstimateTokens(tools1)
	legacySys, legacyTools := legacyPrefix(t, t1, guidesA)
	before := EstimateTokens(legacySys) + EstimateTokens(legacyTools)
	t.Logf("cacheable prefix, estimated tokens — before: %d (system %d + tools %d); after: %d (system %d + tools %d); turn context: %d",
		before, EstimateTokens(legacySys), EstimateTokens(legacyTools),
		after, EstimateTokens(sys1), EstimateTokens(tools1), EstimateTokens(svc1.TurnContext(uuid.Nil, uuid.Nil)))

	if after > maxCacheablePrefixTokens {
		t.Fatalf("system + tools = %d estimated tokens, budget %d", after, maxCacheablePrefixTokens)
	}
	if before < 2*after {
		t.Fatalf("fixture no longer shows the diet: before %d, after %d", before, after)
	}
	if legacySys2, _ := legacyPrefix(t, t2, guidesA); legacySys2 == legacySys {
		t.Fatal("sanity: the legacy prompt changed every minute")
	}
}
