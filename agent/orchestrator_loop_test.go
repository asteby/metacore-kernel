package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/agent/contextmgr"
	"github.com/asteby/metacore-kernel/agent/tools"

	"github.com/google/uuid"
)

// scriptedAI replays one AIResponse (or error) per agent-loop iteration.
type scriptedAI struct {
	steps []scriptedStep
	calls int
}

type scriptedStep struct {
	resp *AIResponse
	err  error
}

func (s *scriptedAI) GenerateChatWithTools(_ context.Context, _ []map[string]interface{}, _ string, _ []map[string]interface{}) (*AIResponse, error) {
	if s.calls >= len(s.steps) {
		return &AIResponse{Message: ""}, nil
	}
	st := s.steps[s.calls]
	s.calls++
	return st.resp, st.err
}

// stubTool is a data tool whose result or failure the test decides.
type stubTool struct {
	name   string
	result interface{}
	err    error
	ran    int
}

func (t *stubTool) Name() string                       { return t.name }
func (t *stubTool) Description() string                { return "stub" }
func (t *stubTool) Category() string                   { return "knowledge" }
func (t *stubTool) Parameters() map[string]interface{} { return map[string]interface{}{} }
func (t *stubTool) Execute(context.Context, map[string]interface{}, *contextmgr.ConversationContext) (interface{}, error) {
	t.ran++
	return t.result, t.err
}

func finalAnswer(id, msg string) *AIResponse {
	return &AIResponse{ToolCalls: []tools.ToolCall{{
		ID:         id,
		ToolName:   "final_answer",
		Parameters: map[string]interface{}{"message": msg},
	}}}
}

func runLoop(t *testing.T, ai *scriptedAI, userMsg string, extra ...tools.BaseTool) *AgentResponse {
	t.Helper()
	reg := tools.NewToolRegistry()
	reg.Register(NewFinalAnswerTool())
	for _, tl := range extra {
		reg.Register(tl)
	}
	o := &AgentOrchestrator{aiService: ai, maxIterations: 12}
	convCtx := &contextmgr.ConversationContext{ConversationID: uuid.New()}
	messages := []map[string]interface{}{{"role": "user", "content": userMsg}}
	resp, err := o.agentLoop(context.Background(), messages, "", convCtx, reg)
	if err != nil {
		t.Fatalf("agentLoop: %v", err)
	}
	if o.maxIterations != 12 {
		t.Fatalf("turn budget leaked into the orchestrator: maxIterations=%d", o.maxIterations)
	}
	return resp
}

// Prod 2026-09-29 (ops.asteby.com/app/agent, Claude Code): "Hola" got a
// final_answer listing capabilities; "antes de que se guarde" matched the
// narration needle "antes de ", the answer was unwrapped, then discarded as
// narration and replaced by "No pude ejecutar la acción con las herramientas".
func TestAgentLoop_GreetingFinalAnswerIsDelivered(t *testing.T) {
	greeting := "¡Hola! 👋 Soy *Aby*, tu asistente en la plataforma.\n\n" +
		"Puedo ayudarte con cosas como:\n\n" +
		"• Consultar ventas, pedidos, facturas y clientes\n" +
		"• Registrar productos, clientes, compras y más (tú apruebas cada cambio antes de que se guarde)\n" +
		"• Enseñarte paso a paso cómo hacer algo en la pantalla\n\n" +
		"¿Qué necesitas hoy?"
	ai := &scriptedAI{steps: []scriptedStep{{resp: finalAnswer("call_final_answer_0", greeting)}}}
	resp := runLoop(t, ai, "Hola", &stubTool{name: "query_data"})

	if ai.calls != 1 {
		t.Fatalf("a direct final_answer must end the turn in one call, got %d", ai.calls)
	}
	if !strings.Contains(resp.Message, "Soy") || strings.Contains(resp.Message, "No pude ejecutar") {
		t.Fatalf("greeting must be delivered, got %q", resp.Message)
	}
}

func TestAgentLoop_QuestionThatNeedsATool(t *testing.T) {
	q := &stubTool{name: "query_data", result: map[string]interface{}{"total": 3}}
	ai := &scriptedAI{steps: []scriptedStep{
		{resp: &AIResponse{ToolCalls: []tools.ToolCall{{ID: "c1", ToolName: "query_data", Parameters: map[string]interface{}{"model": "branches"}}}}},
		{resp: finalAnswer("c2", "Tienes 3 sucursales.")},
	}}
	resp := runLoop(t, ai, "¿cuántas sucursales tengo?", q)

	if q.ran != 1 {
		t.Fatalf("query_data should run once, ran %d", q.ran)
	}
	if resp.Message != "Tienes 3 sucursales." {
		t.Fatalf("answer grounded on the tool must be delivered, got %q", resp.Message)
	}
}

func TestAgentLoop_NarrationThenTool(t *testing.T) {
	q := &stubTool{name: "query_data", result: map[string]interface{}{"total": 12}}
	ai := &scriptedAI{steps: []scriptedStep{
		// Narration with no tool: nudged once, not published.
		{resp: &AIResponse{Message: "Voy a consultar las facturas de este mes."}},
		{resp: &AIResponse{ToolCalls: []tools.ToolCall{{ID: "c1", ToolName: "query_data", Parameters: map[string]interface{}{"model": "invoices"}}}}},
		{resp: finalAnswer("c2", "Este mes hay 12 facturas.")},
	}}
	resp := runLoop(t, ai, "cuántas facturas hay este mes", q)

	if q.ran != 1 {
		t.Fatalf("the nudge must lead to the tool call, ran %d", q.ran)
	}
	if resp.Message != "Este mes hay 12 facturas." {
		t.Fatalf("got %q", resp.Message)
	}
}

func TestAgentLoop_RepeatedNarrationStillFallsBack(t *testing.T) {
	ai := &scriptedAI{steps: []scriptedStep{
		{resp: &AIResponse{Message: "Voy a consultar las facturas."}},
		{resp: &AIResponse{Message: "Voy a consultar las facturas ahora."}},
	}}
	resp := runLoop(t, ai, "cuántas facturas hay este mes", &stubTool{name: "query_data"})
	if strings.Contains(resp.Message, "Voy a") {
		t.Fatalf("narration after the nudge must not be published, got %q", resp.Message)
	}
}

func TestAgentLoop_ProviderErrorNamesFailedTool(t *testing.T) {
	q := &stubTool{name: "query_data", err: errors.New("column \"foo\" does not exist")}
	ai := &scriptedAI{steps: []scriptedStep{
		{resp: &AIResponse{ToolCalls: []tools.ToolCall{{ID: "c1", ToolName: "query_data", Parameters: map[string]interface{}{}}}}},
		{err: errors.New("boom")},
	}}
	reg := tools.NewToolRegistry()
	reg.Register(NewFinalAnswerTool())
	reg.Register(q)
	o := &AgentOrchestrator{aiService: ai, maxIterations: 12}
	convCtx := &contextmgr.ConversationContext{ConversationID: uuid.New()}
	_, err := o.agentLoop(context.Background(), []map[string]interface{}{{"role": "user", "content": "ventas de hoy"}}, "", convCtx, reg)
	var tte *ToolTurnError
	if !errors.As(err, &tte) || tte.Tool != "query_data" {
		t.Fatalf("want ToolTurnError for query_data, got %v", err)
	}
	pe := ClassifyProviderError(err, "claude_code")
	if pe.Kind != ProviderErrTool || !strings.Contains(pe.Message, "query_data") {
		t.Fatalf("got %+v", pe)
	}
}

func TestIsDeferredNarration_FinishedAnswersAreNot(t *testing.T) {
	no := []string{
		"¡Hola! Soy Aby.\n\n• Registrar productos (tú apruebas cada cambio antes de que se guarde)\n\n¿Qué necesitas hoy?",
		"Listo, ya está.\n\nPrimero abre Configuración y luego pulsa Nueva.",
		"¿Quieres que primero revise el inventario?",
	}
	for _, c := range no {
		if isDeferredNarration(c) {
			t.Errorf("finished answer flagged as narration: %q", c)
		}
	}
}
