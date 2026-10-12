package agent

import (
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/agent/tools"
)

func TestBuildFallbackFromToolResults_RoleTool(t *testing.T) {
	o := &AgentOrchestrator{}
	reg := tools.NewToolRegistry()
	messages := []map[string]interface{}{
		{"role": "user", "content": "analisis de ventas de este mes"},
		{
			"role":    "tool",
			"name":    "aggregate_data",
			"content": "Resultado de aggregate_data:\n{\n  \"value\": 49\n}",
		},
		{
			"role":    "tool",
			"name":    "aggregate_data",
			"content": "Resultado de aggregate_data:\n{\n  \"value\": 4301\n}",
		},
		{
			"role": "tool",
			"name": "aggregate_data",
			"content": `Resultado de aggregate_data:
[
  {"state": "confirmed", "value": 4000},
  {"state": "delivered", "value": 301}
]`,
		},
	}
	fb := o.buildFallbackFromToolResults(messages, reg)
	if fb == "" {
		t.Fatal("expected non-empty fallback from role=tool aggregate results")
	}
	if !strings.Contains(fb, "4301") && !strings.Contains(fb, "49") && !strings.Contains(fb, "confirmed") {
		t.Fatalf("fallback missing aggregate numbers: %q", fb)
	}
}

func TestParseToolResultPayload(t *testing.T) {
	data, ok := parseToolResultPayload("Resultado de aggregate_data:\n{\n  \"value\": 12\n}")
	if !ok {
		t.Fatal("expected parse ok")
	}
	m, ok := data.(map[string]interface{})
	if !ok {
		t.Fatalf("want map, got %T", data)
	}
	if m["value"].(float64) != 12 {
		t.Fatalf("value=%v", m["value"])
	}
}

func TestUnwrapSyntheticFinalAnswer_DeferredAfterTools(t *testing.T) {
	messages := []map[string]interface{}{
		{"role": "user", "content": "cuanto vendimos"},
		{"role": "tool", "content": "Resultado de list_models: ..."},
	}
	ai := &AIResponse{
		Message: "",
		ToolCalls: []tools.ToolCall{{
			ID:       "call_auto_final",
			ToolName: "final_answer",
			Parameters: map[string]interface{}{
				"message": "Ahora voy a consultar las facturas de este mes (agosto 2026):",
			},
		}},
	}
	out := unwrapSyntheticFinalAnswer(ai, messages)
	if len(out.ToolCalls) != 0 {
		t.Fatalf("expected unwrap, got %+v", out.ToolCalls)
	}
	if !isDeferredNarration(out.Message) {
		t.Fatalf("message should stay deferred for nudge: %q", out.Message)
	}
}

func TestFormatDescribeModelNoGoDump(t *testing.T) {
	o := &AgentOrchestrator{}
	data := map[string]interface{}{
		"title": "Productos",
		"model": "products",
		"columns": []interface{}{
			map[string]interface{}{"name": "sku", "type": "string"},
			map[string]interface{}{"name": "name", "type": "string"},
		},
	}
	out := o.formatToolResultForUser("describe_model", data)
	if strings.Contains(out, "map[") {
		t.Fatalf("leaked Go dump: %q", out)
	}
	if !strings.Contains(out, "Productos") {
		t.Fatalf("expected human summary: %q", out)
	}
}

func TestBuildFallbackSkipsDescribeModel(t *testing.T) {
	o := &AgentOrchestrator{}
	reg := tools.NewToolRegistry()
	messages := []map[string]interface{}{
		{"role": "user", "content": "crea producto"},
		{
			"role": "tool",
			"name": "describe_model",
			"content": `Resultado de describe_model:
{"title":"X","columns":[{"name":"a","type":"string"}]}`,
		},
	}
	fb := o.buildFallbackFromToolResults(messages, reg)
	if strings.Contains(fb, "map[") || strings.Contains(fb, "columns") {
		t.Fatalf("should skip describe_model dumps, got %q", fb)
	}
}

func TestIsDeferredNarration_AhoraVoy(t *testing.T) {
	cases := []string{
		"Ahora voy a consultar las facturas de este mes (agosto 2026):",
		"Ahora voy a describir el modelo de clientes:",
		"Tienes razón. Voy a obtener la estructura del modelo de clientes ahora mismo.",
		"Déjame listar los modelos disponibles para encontrar el correcto:",
		// Prod bug: after describe_model, Haiku said this and it was published as final.
		"Ahora creo el cliente Rodrigo:",
		"Perfecto, voy a crear el cliente **Rodrigo**.",
		"Creo un cliente con nombre Ana:",
		"Estoy creando el registro ahora.",
	}
	for _, c := range cases {
		if !isDeferredNarration(c) {
			t.Errorf("expected deferred: %q", c)
		}
	}
	// "creo que…" is opinion, not a pending mutation announcement.
	if isDeferredNarration("Creo que no hay clientes con ese nombre.") {
		t.Errorf("did not expect deferred for opinion: creo que…")
	}
}

func TestUnwrapSyntheticFinalAnswer_IdentitySticks(t *testing.T) {
	messages := []map[string]interface{}{
		{"role": "user", "content": "usas cursor agent?"},
	}
	ai := &AIResponse{
		ToolCalls: []tools.ToolCall{{
			ID:       "call_final_answer_0",
			ToolName: "final_answer",
			Parameters: map[string]interface{}{
				"message": "No. Soy Aby, el asistente de esta plataforma. Puedo ayudarte con sucursales.",
			},
		}},
	}
	out := unwrapSyntheticFinalAnswer(ai, messages)
	if len(out.ToolCalls) != 1 || out.ToolCalls[0].ToolName != "final_answer" {
		t.Fatalf("explanatory answer must stick on the first turn, got %+v", out.ToolCalls)
	}
}

func TestUnwrapSyntheticFinalAnswer_DropsInventedCountAfterNudge(t *testing.T) {
	messages := []map[string]interface{}{
		{"role": "user", "content": "cuantas sucursales tengo"},
		{"role": "user", "content": "[sistema] No afirmes cifras sin una herramienta."},
	}
	ai := &AIResponse{
		ToolCalls: []tools.ToolCall{{
			ID:       "call_final_answer_0",
			ToolName: "final_answer",
			Parameters: map[string]interface{}{
				"message": "Tienes 3 sucursales.",
			},
		}},
	}
	out := unwrapSyntheticFinalAnswer(ai, messages)
	if len(out.ToolCalls) != 0 {
		t.Fatalf("invented count must be stripped so it is not published, got %+v", out.ToolCalls)
	}
}

func TestClaimsUngroundedOrgData(t *testing.T) {
	yes := []string{
		"Tienes 3 sucursales.",
		"Hay 12 facturas vencidas.",
		"Creé el cliente Rodrigo.",
		"En total 40 ventas este mes.",
		"Tienes dos sucursales.",
	}
	no := []string{
		"No. Soy Aby, el asistente de esta plataforma. Puedo ayudarte con sucursales.",
		"Creo que no hay clientes con ese nombre.",
		"¿Qué necesitas?",
		"Para crear una sucursal abre Configuración y pulsa Nueva.",
	}
	for _, c := range yes {
		if !claimsUngroundedOrgData(c) {
			t.Errorf("expected ungrounded claim: %q", c)
		}
	}
	for _, c := range no {
		if claimsUngroundedOrgData(c) {
			t.Errorf("expected no claim: %q", c)
		}
	}
}

func TestUnwrapSyntheticFinalAnswer_AcceptsAnswerAfterNudge(t *testing.T) {
	messages := []map[string]interface{}{
		{"role": "user", "content": "usas cursor agent?"},
		{"role": "assistant", "content": "No. Soy Aby."},
		{"role": "user", "content": "[sistema] DEBES llamar ahora una o más herramientas."},
	}
	ai := &AIResponse{
		ToolCalls: []tools.ToolCall{{
			ID:       "call_final_answer_0",
			ToolName: "final_answer",
			Parameters: map[string]interface{}{
				"message": "No. Soy Aby, el asistente de esta plataforma.",
			},
		}},
	}
	out := unwrapSyntheticFinalAnswer(ai, messages)
	if len(out.ToolCalls) != 1 || out.ToolCalls[0].ToolName != "final_answer" {
		t.Fatalf("answer after one nudge must stick, got %+v", out.ToolCalls)
	}
}

func TestUnwrapSyntheticFinalAnswer_ForcesToolsOnce(t *testing.T) {
	messages := []map[string]interface{}{
		{"role": "user", "content": "cuantas sucursales tengo"},
	}
	ai := &AIResponse{
		ToolCalls: []tools.ToolCall{{
			ID:       "call_final_answer_0",
			ToolName: "final_answer",
			Parameters: map[string]interface{}{
				"message": "Tienes 3 sucursales.",
			},
		}},
	}
	out := unwrapSyntheticFinalAnswer(ai, messages)
	if len(out.ToolCalls) != 0 {
		t.Fatalf("first answer without tools should be unwrapped, got %+v", out.ToolCalls)
	}
}

func TestUnwrapSyntheticFinalAnswer_AhoraCreoAfterDescribe(t *testing.T) {
	messages := []map[string]interface{}{
		{"role": "user", "content": "que se llame Rodrigo el cliente"},
		{"role": "tool", "name": "describe_model", "content": "Resultado de describe_model:\n{}"},
	}
	ai := &AIResponse{
		Message: "Ahora creo el cliente Rodrigo:",
		ToolCalls: []tools.ToolCall{{
			ID:       "call_auto_final",
			ToolName: "final_answer",
			Parameters: map[string]interface{}{
				"message": "Ahora creo el cliente Rodrigo:",
			},
		}},
	}
	out := unwrapSyntheticFinalAnswer(ai, messages)
	if len(out.ToolCalls) != 0 {
		t.Fatalf("expected unwrap after describe_model+ahora creo, got %+v", out.ToolCalls)
	}
	if !isDeferredNarration(out.Message) {
		t.Fatalf("message should stay deferred for nudge: %q", out.Message)
	}
}

func TestFormatQueryResultReadsAsProse(t *testing.T) {
	o := &AgentOrchestrator{}
	one := map[string]interface{}{
		"model": "warehouses",
		"count": float64(1),
		"records": []interface{}{
			map[string]interface{}{"id": "w-1", "name": "Almacén Uno", "code": "ALM001"},
		},
	}
	out := o.formatToolResultForUser("query_data", one)
	for _, leak := range []string{"Count", "Records", "Model", "warehouses:"} {
		if strings.Contains(out, leak) {
			t.Fatalf("raw tool key %q leaked: %q", leak, out)
		}
	}
	if !strings.Contains(out, "**1** registro") || !strings.Contains(out, "[Almacén Uno · ALM001](/m/") {
		t.Fatalf("expected count + record link: %q", out)
	}

	empty := map[string]interface{}{"model": "products", "count": float64(0), "records": []interface{}{}}
	if out := o.formatToolResultForUser("query_data", empty); !strings.HasPrefix(out, "No encontré registros en **") {
		t.Fatalf("expected empty-result sentence: %q", out)
	}
}
