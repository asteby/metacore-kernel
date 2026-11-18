package agent

import (
	"regexp"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/agent/tools"
)

// El caso de producción: Gobi llamó briefing_actual (hubo herramientas en el
// turno) y escribió "Aquí está el gráfico…" sin llamar show_chart. El chequeo
// de datos de la organización no lo veía porque sí hubo resultados de tools.

func chartCall(id string) *AIResponse {
	return &AIResponse{ToolCalls: []tools.ToolCall{{
		ID:       id,
		ToolName: "show_chart",
		Parameters: map[string]interface{}{
			"title": "Hallazgos por eje", "kind": "bar", "x_key": "eje",
			"series": []interface{}{map[string]interface{}{"key": "n", "label": "Hallazgos"}},
			"rows": []interface{}{
				map[string]interface{}{"eje": "Salud", "n": 55.0},
				map[string]interface{}{"eje": "Empleo", "n": 33.0},
			},
		},
	}}}
}

func toolCall(id, name string) *AIResponse {
	return &AIResponse{ToolCalls: []tools.ToolCall{{ID: id, ToolName: name, Parameters: map[string]interface{}{}}}}
}

func hasChartEmbed(r *AgentResponse) bool {
	for _, e := range r.Embeds {
		if e.Type == "chart" {
			return true
		}
	}
	return false
}

func claimsMatch(tool tools.OutcomeClaimer, s string) bool {
	for _, res := range tool.OutcomeClaims() {
		for _, re := range res {
			if re.MatchString(s) {
				return true
			}
		}
	}
	return false
}

func TestChartClaims_MatchAssertionsNotOffersOrRefusals(t *testing.T) {
	tool := NewShowChartTool()
	for _, s := range []string{
		"Aquí está el gráfico de hallazgos por eje:",
		"Aquí tienes la gráfica de menciones por semana.",
		"Te muestro el gráfico con los totales",
		"Ya generé el gráfico de participación.",
		"He creado una gráfica con los datos",
		"Acabo de preparar el gráfico",
		"Aquí están los gráficos solicitados",
		"Here is the chart of findings",
		"Here's your bar chart by axis:",
		"I have created a chart with the totals",
		"Aqui está o gráfico de achados por eixo:",
		"Segue o gráfico com os totais",
		"Já gerei o gráfico de participação.",
	} {
		if !claimsMatch(tool, s) {
			t.Errorf("debe contar como afirmación: %q", s)
		}
	}
	for _, s := range []string{
		"Puedo mostrarte un gráfico si quieres.",
		"¿Quieres que arme un gráfico de esto?",
		"No pude generar el gráfico porque no hay datos.",
		"Los gráficos de barras sirven para comparar categorías.",
		"El eje más activo es Salud con 55 hallazgos.",
		"I can show you a chart if you like.",
		"I could not build the chart: no data.",
		"Posso mostrar um gráfico se quiser.",
		"",
	} {
		if claimsMatch(tool, s) {
			t.Errorf("no debe contar como afirmación: %q", s)
		}
	}
}

// Un host añade un idioma a cualquier herramienta sin tocar el kernel.
func TestRegisterOutcomeClaims_HostAddsALanguage(t *testing.T) {
	reg := tools.NewToolRegistry()
	reg.Register(NewShowChartTool())
	msg := "Voici le graphique des résultats par axe :"
	if got := unbackedClaim(reg, msg, map[string]bool{}); got != "" {
		t.Fatalf("sin registrar el francés no debe detectarse: %q", got)
	}
	RegisterOutcomeClaims("show_chart", "fr", regexp.MustCompile(`(?i)\bvoici\s+(?:le|un)\s+graphique\b`))
	if got := unbackedClaim(reg, msg, map[string]bool{}); got != "show_chart" {
		t.Fatalf("tras registrarlo debe detectarse, got %q", got)
	}
	if got := unbackedClaim(reg, msg, map[string]bool{"show_chart": true}); got != "" {
		t.Fatalf("si la herramienta corrió, no hay afirmación sin respaldo: %q", got)
	}
}

// El envoltorio del host (arnés, guarda de permisos) no debe esconder las
// interfaces opcionales: tools.As las alcanza por Unwrap.
type wrapperTool struct{ tools.BaseTool }

func (w wrapperTool) Unwrap() tools.BaseTool { return w.BaseTool }

func TestAs_ReachesThroughWrappers(t *testing.T) {
	var wrapped tools.BaseTool = wrapperTool{wrapperTool{NewShowChartTool()}}
	if _, ok := wrapped.(tools.OutcomeClaimer); ok {
		t.Fatal("el envoltorio no debería exponer la interfaz por sí mismo (es lo que se prueba)")
	}
	c, ok := tools.As[tools.OutcomeClaimer](wrapped)
	if !ok || len(c.OutcomeClaims()) == 0 {
		t.Fatal("As debe atravesar dos envoltorios hasta show_chart")
	}
	if _, ok := tools.As[tools.OutcomeClaimer](&stubTool{name: "x"}); ok {
		t.Fatal("una herramienta sin la interfaz no debe devolver ok")
	}
	reg := tools.NewToolRegistry()
	reg.Register(wrapped)
	if got := unbackedClaim(reg, "Aquí está el gráfico", map[string]bool{}); got != "show_chart" {
		t.Fatalf("la afirmación debe detectarse aun con la herramienta envuelta, got %q", got)
	}
}

func TestLoop_ClaimWithoutToolSendsModelBackThenChartAppears(t *testing.T) {
	briefing := &stubTool{name: "briefing_actual", result: map[string]interface{}{"hallazgos": 201}}
	ai := &scriptedAI{steps: []scriptedStep{
		{resp: toolCall("c1", "briefing_actual")},
		{resp: &AIResponse{Message: "Aquí está el gráfico de hallazgos por eje:"}}, // promete sin llamar
		{resp: chartCall("c2")},                                                    // tras el empujón la llama
		{resp: finalAnswer("c3", "Salud lidera con 55 hallazgos.")},
	}}
	resp := runLoop(t, ai, "dame el briefing con un gráfico por eje", briefing, NewShowChartTool())

	if !hasChartEmbed(resp) {
		t.Fatalf("tras el empujón debe haber un gráfico real; embeds=%+v", resp.Embeds)
	}
	if strings.Contains(resp.Message, "no pude generar") {
		t.Fatalf("no debe avisar de fallo si el gráfico sí se generó: %q", resp.Message)
	}
	if ai.calls != 4 {
		t.Fatalf("esperaba 4 llamadas al modelo (tool, promesa, show_chart, cierre), hubo %d", ai.calls)
	}
}

func TestLoop_ModelRewritesInUsersLanguageWhenToolCannotRun(t *testing.T) {
	briefing := &stubTool{name: "briefing_actual", result: map[string]interface{}{"hallazgos": 201}}
	ai := &scriptedAI{steps: []scriptedStep{
		{resp: toolCall("c1", "briefing_actual")},
		{resp: &AIResponse{Message: "Here is the chart of findings by axis:"}},
		{resp: &AIResponse{Message: "Here is the chart of findings by axis:"}},                           // insiste tras el empujón
		{resp: &AIResponse{Message: "I could not build the chart, but here are the totals: Health 55."}}, // reescribe
	}}
	resp := runLoop(t, ai, "show me the briefing with a chart", briefing, NewShowChartTool())

	if hasChartEmbed(resp) {
		t.Fatal("no hubo gráfico: no debe fingirse")
	}
	if !strings.Contains(resp.Message, "could not build the chart") {
		t.Fatalf("debe entregar la reescritura del modelo (en su idioma): %q", resp.Message)
	}
	if strings.Contains(resp.Message, "no ejecutada") {
		t.Fatalf("si el modelo corrige, no debe añadirse la nota de último recurso: %q", resp.Message)
	}
	if ai.calls != 4 {
		t.Fatalf("tool, promesa, empujón, reescritura = 4 llamadas, hubo %d", ai.calls)
	}
}

func TestLoop_ClaimThatSurvivesRewriteGetsNeutralNote(t *testing.T) {
	claim := "Aquí está el gráfico de hallazgos por eje:"
	briefing := &stubTool{name: "briefing_actual", result: map[string]interface{}{"hallazgos": 201}}
	ai := &scriptedAI{steps: []scriptedStep{
		{resp: toolCall("c1", "briefing_actual")},
		{resp: &AIResponse{Message: claim}},
		{resp: &AIResponse{Message: claim}},
		{resp: &AIResponse{Message: claim}}, // ni reescribiendo deja de afirmarlo
	}}
	resp := runLoop(t, ai, "dame el briefing con un gráfico por eje", briefing, NewShowChartTool())
	if hasChartEmbed(resp) {
		t.Fatal("no hubo gráfico: no debe fingirse")
	}
	if !strings.Contains(resp.Message, "show_chart") || !strings.Contains(resp.Message, "not executed") {
		t.Fatalf("debe marcar la afirmación como no respaldada: %q", resp.Message)
	}
	if ai.calls != 4 {
		t.Fatalf("a lo sumo empujón + reescritura por turno, hubo %d llamadas", ai.calls)
	}
}

func TestLoop_ClaimInsideFinalAnswerIsAlsoChecked(t *testing.T) {
	ai := &scriptedAI{steps: []scriptedStep{
		{resp: toolCall("c1", "briefing_actual")},
		{resp: finalAnswer("c2", "Aquí está el gráfico de hallazgos por eje.")}, // el host responde por final_answer
		{resp: chartCall("c3")},
		{resp: finalAnswer("c4", "Listo.")},
	}}
	briefing := &stubTool{name: "briefing_actual", result: map[string]interface{}{"hallazgos": 201}}
	resp := runLoop(t, ai, "dame el briefing con un gráfico por eje", briefing, NewShowChartTool())
	if !hasChartEmbed(resp) {
		t.Fatalf("una afirmación dentro de final_answer también debe empujar a llamar la herramienta: %+v", resp.Embeds)
	}
}

func TestLoop_NoFalsePositivesWhenChartRanOrOnlyOffered(t *testing.T) {
	// show_chart sí corrió: afirmarlo es verdad.
	ai := &scriptedAI{steps: []scriptedStep{
		{resp: chartCall("c1")},
		{resp: finalAnswer("c2", "Aquí está el gráfico de hallazgos por eje.")},
	}}
	resp := runLoop(t, ai, "muéstrame un gráfico de hallazgos", NewShowChartTool())
	if ai.calls != 2 || strings.Contains(resp.Message, "no pude generar") {
		t.Fatalf("show_chart corrió: no debe empujar ni avisar (calls=%d, msg=%q)", ai.calls, resp.Message)
	}

	// Solo una oferta: no es una afirmación.
	ai2 := &scriptedAI{steps: []scriptedStep{
		{resp: finalAnswer("c1", "Puedo mostrarte un gráfico si lo necesitas.")},
	}}
	resp2 := runLoop(t, ai2, "¿qué puedes hacer?", NewShowChartTool())
	if ai2.calls != 1 || strings.Contains(resp2.Message, "no pude generar") {
		t.Fatalf("una oferta no debe disparar el guardarraíl (calls=%d, msg=%q)", ai2.calls, resp2.Message)
	}
}
