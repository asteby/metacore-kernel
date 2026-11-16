package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/agent/contextmgr"
	"github.com/asteby/metacore-kernel/agent/tools"
	"github.com/google/uuid"
)

func TestEstimateTokens(t *testing.T) {
	if EstimateTokens("") != 0 || EstimateTokens("abc") != 1 || EstimateTokens(strings.Repeat("a", 35)) != 10 || EstimateTokens(strings.Repeat("a", 36)) != 11 {
		t.Fatal("chars/3.5 rounded up")
	}
	if EstimateTokens(strings.Repeat("ñ", 7)) != 2 {
		t.Fatal("estimate counts characters, not bytes")
	}
}

func msg(role, content string) map[string]interface{} {
	return map[string]interface{}{"role": role, "content": content}
}

func TestTrimHistoryToBudget(t *testing.T) {
	big := strings.Repeat("x", 3500) // 1000 tokens
	history := []map[string]interface{}{
		msg("user", big), msg("assistant", big), // oldest pair
		msg("user", big), msg("assistant", big),
		msg("user", "¿y hoy?"),
	}
	got := trimHistoryToBudget(history, 2500)
	if len(got) != 3 || got[0]["role"] != "user" || got[2]["content"] != "¿y hoy?" {
		t.Fatalf("want the newest pair + current, got %d msgs", len(got))
	}

	// An assistant reply must never open the kept history.
	got = trimHistoryToBudget(history, 1500)
	if len(got) != 1 || got[0]["content"] != "¿y hoy?" {
		t.Fatalf("want only the current message, got %d", len(got))
	}

	// The current message survives even when it alone exceeds the budget.
	huge := []map[string]interface{}{msg("user", big), msg("assistant", "ok"), msg("user", big+big)}
	got = trimHistoryToBudget(huge, 100)
	if len(got) != 1 || got[0]["content"] != big+big {
		t.Fatal("current user message must always be kept")
	}

	if got := trimHistoryToBudget(history, 0); len(got) != len(history) {
		t.Fatal("no budget keeps everything")
	}
	if got := trimHistoryToBudget(history, 1_000_000); len(got) != len(history) {
		t.Fatal("a roomy budget keeps everything")
	}
}

func TestWithTurnContextDoesNotTouchLoopMessages(t *testing.T) {
	loop := []map[string]interface{}{msg("user", "antes"), msg("assistant", "ok"), msg("user", "hola")}
	out := withTurnContext(loop, 2, "[Contexto del turno] Hoy es lunes.")
	if out[2]["content"] != "hola\n\n[Contexto del turno] Hoy es lunes." {
		t.Fatalf("sent content %q", out[2]["content"])
	}
	if loop[2]["content"] != "hola" {
		t.Fatal("loop message mutated")
	}
	if got := withTurnContext(loop, -1, "x"); &got[0] != &loop[0] {
		t.Fatal("no index: same slice")
	}
	if got := withTurnContext(loop, 2, ""); got[2]["content"] != "hola" {
		t.Fatal("empty context: unchanged")
	}
}

// The turn context rides on the turn's user message in every call of the
// turn (also after tool results and nudges), never in the system prompt, and
// the budget of each call is reported with its sections.
func TestAgentLoopTurnSendsTurnContextAndReportsBudget(t *testing.T) {
	lookup := &stubTool{name: "lookup", result: map[string]interface{}{"rows": strings.Repeat("r", 5000)}}
	ai := &captureAI{steps: []*AIResponse{
		{ToolCalls: []tools.ToolCall{{ID: "t1", ToolName: "lookup", Parameters: map[string]interface{}{}}}},
		finalAnswer("c1", "Hay datos."),
	}}
	reg := tools.NewToolRegistry()
	reg.Register(NewFinalAnswerTool())
	reg.Register(lookup)
	var budgets []PromptBudget
	o := &AgentOrchestrator{aiService: ai, maxIterations: 12, onBudget: func(_ context.Context, b PromptBudget) { budgets = append(budgets, b) }}
	conv := uuid.New()
	messages := []map[string]interface{}{msg("user", "previo"), msg("assistant", "ok"), msg("user", "cuántos hay")}
	const tc = "[Contexto del turno] Hoy es lunes 2026-10-12 (zona UTC)."

	resp, err := o.agentLoopTurn(context.Background(), messages, "Eres Aby.", &contextmgr.ConversationContext{ConversationID: conv}, reg,
		turnOptions{turnContext: tc, userIndex: 2, maxResultBytes: 1000})
	if err != nil || resp.Message != "Hay datos." {
		t.Fatalf("resp=%v err=%v", resp, err)
	}
	if len(ai.messages) != 2 {
		t.Fatalf("calls %d", len(ai.messages))
	}
	for i, sent := range ai.messages {
		if sent[2]["content"] != "cuántos hay\n\n"+tc {
			t.Fatalf("call %d: turn context missing on the user message: %q", i, sent[2]["content"])
		}
		if sent[0]["content"] != "previo" {
			t.Fatalf("call %d: earlier history must stay untouched", i)
		}
		if strings.Contains(ai.prompts[i], "Contexto del turno") {
			t.Fatal("turn context leaked into the system prompt")
		}
	}
	// The per-turn result cap applies (5000-byte result, 1000-byte cap).
	toolMsg := ai.messages[1][len(ai.messages[1])-1]
	if c := toolMsg["content"].(string); toolMsg["role"] != "tool" || !strings.Contains(c, "RESULTADO TRUNCADO") || len(c) > 1500 {
		t.Fatalf("tool result not capped by the turn: %d bytes", len(c))
	}

	if len(budgets) != 2 {
		t.Fatalf("budgets %d", len(budgets))
	}
	first, second := budgets[0], budgets[1]
	defs, _ := json.Marshal(reg.ToOpenAIFormat())
	if first.ConversationID != conv || first.Iteration != 1 || first.System != EstimateTokens("Eres Aby.") ||
		first.Tools != EstimateTokens(string(defs)) || first.ToolCount != 2 || first.TurnContext != EstimateTokens(tc) ||
		first.ToolResults != 0 || first.HistoryMessages != 3 {
		t.Fatalf("first call budget %+v", first)
	}
	if second.Iteration != 2 || second.ToolResults == 0 || second.ToolResults > 450 || second.History <= first.History {
		t.Fatalf("second call budget %+v", second)
	}
	if second.Total != second.System+second.Tools+second.History+second.TurnContext+second.ToolResults {
		t.Fatal("total must add the sections")
	}
}

func TestResultCapOverride(t *testing.T) {
	o := &AgentOrchestrator{policy: toolPolicy{MaxToolResultBytes: 5000}}
	if o.resultCap(0) != 5000 || o.resultCap(800) != 800 || o.resultCap(-1) != 0 {
		t.Fatal("turn override")
	}
	if (&AgentOrchestrator{}).resultCap(0) != DefaultMaxToolResultBytes {
		t.Fatal("default")
	}
}

func TestToolsInPromptIsOptIn(t *testing.T) {
	reg := tools.NewToolRegistry()
	reg.Register(NewFinalAnswerTool())
	off := NewAgentOrchestrator(OrchestratorConfig{}).turnSystemPrompt("Eres Aby.", reg)
	if strings.Contains(off, "HERRAMIENTAS DISPONIBLES") || !strings.HasSuffix(off, closingRules) {
		t.Fatalf("default must not list the tools as text:\n%s", off)
	}
	on := NewAgentOrchestrator(OrchestratorConfig{ToolsInPrompt: true}).turnSystemPrompt("Eres Aby.", reg)
	if !strings.Contains(on, "## HERRAMIENTAS DISPONIBLES") || !strings.Contains(on, "**final_answer**") || !strings.HasSuffix(on, closingRules) {
		t.Fatalf("opt-in must render the legacy section:\n%s", on)
	}
}
