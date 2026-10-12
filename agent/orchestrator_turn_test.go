package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/agent/contextmgr"
	"github.com/asteby/metacore-kernel/agent/tools"
	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// captureAI records the prompt and tools of each call and replays steps.
type captureAI struct {
	steps   []*AIResponse
	prompts []string
	defs    [][]map[string]interface{}
}

func (c *captureAI) GenerateChatWithTools(_ context.Context, _ []map[string]interface{}, system string, defs []map[string]interface{}) (*AIResponse, error) {
	c.prompts = append(c.prompts, system)
	c.defs = append(c.defs, defs)
	i := len(c.prompts) - 1
	if i >= len(c.steps) {
		return &AIResponse{}, nil
	}
	return c.steps[i], nil
}

func newTurnOrchestrator(t *testing.T, ai AIService, base ...tools.BaseTool) *AgentOrchestrator {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	reg := tools.NewToolRegistry()
	reg.Register(NewFinalAnswerTool())
	for _, b := range base {
		reg.Register(b)
	}
	return NewAgentOrchestrator(OrchestratorConfig{Tools: reg, Context: contextmgr.NewManager(db), AI: ai})
}

func TestProcessMessageDeliversFinalAnswerAndKeepsHistory(t *testing.T) {
	ai := &captureAI{steps: []*AIResponse{finalAnswer("c1", "Hola, soy tu asistente.")}}
	o := newTurnOrchestrator(t, ai)
	conv, agentID := uuid.New(), uuid.New()

	resp, err := o.ProcessMessage(context.Background(), Turn{
		ConversationID: conv, AgentID: agentID, UserMessage: "Hola", SystemPrompt: "Eres Aby.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message != "Hola, soy tu asistente." {
		t.Fatalf("message %q", resp.Message)
	}
	if resp.Metadata["conversation_id"] != conv || resp.Metadata["agent_id"] != agentID || resp.Metadata["sender_type"] != "agent" {
		t.Fatalf("metadata %v", resp.Metadata)
	}
	p := ai.prompts[0]
	if !strings.HasPrefix(p, "Eres Aby.") || !strings.Contains(p, "## HERRAMIENTAS DISPONIBLES") || !strings.Contains(p, "**final_answer**") || !strings.Contains(p, "*REQUERIDO*") {
		t.Fatalf("system prompt missing host prompt or tools section:\n%s", p)
	}
	ctx := o.contextMgr.GetContext(conv)
	if len(ctx.History) != 2 || ctx.History[0].Role != "user" || ctx.History[1].Content != "Hola, soy tu asistente." {
		t.Fatalf("history %+v", ctx.History)
	}
	if ctx.AgentID != agentID {
		t.Fatal("agent id not stamped on context")
	}
}

func TestExtraToolsAreTurnScoped(t *testing.T) {
	ai := &captureAI{steps: []*AIResponse{finalAnswer("c1", "ok"), finalAnswer("c2", "ok")}}
	o := newTurnOrchestrator(t, ai)
	extra := &stubTool{name: "per_agent_tool", result: "x"}

	if _, err := o.ProcessMessage(context.Background(), Turn{ConversationID: uuid.New(), UserMessage: "a", ExtraTools: []tools.BaseTool{extra}}); err != nil {
		t.Fatal(err)
	}
	if _, err := o.ProcessMessage(context.Background(), Turn{ConversationID: uuid.New(), UserMessage: "b"}); err != nil {
		t.Fatal(err)
	}
	has := func(defs []map[string]interface{}, name string) bool {
		for _, d := range defs {
			if d["name"] == name {
				return true
			}
		}
		return false
	}
	if !has(ai.defs[0], "per_agent_tool") || has(ai.defs[1], "per_agent_tool") {
		t.Fatal("per-turn tool leaked into the shared registry")
	}
	if _, ok := o.toolRegistry.Get("per_agent_tool"); ok {
		t.Fatal("base registry mutated")
	}
}

func TestProcessMessageRunsToolThenAnswers(t *testing.T) {
	tool := &stubTool{name: "lookup", result: map[string]interface{}{"n": 3}}
	ai := &captureAI{steps: []*AIResponse{
		{ToolCalls: []tools.ToolCall{{ID: "t1", ToolName: "lookup", Parameters: map[string]interface{}{}}}},
		finalAnswer("c1", "Hay 3."),
	}}
	o := newTurnOrchestrator(t, ai, tool)
	var events []string
	o.SetStreamCallback(func(_ uuid.UUID, e AIActivityEvent) { events = append(events, e.Type) })

	resp, err := o.ProcessMessage(context.Background(), Turn{ConversationID: uuid.New(), UserMessage: "cuántos"})
	if err != nil {
		t.Fatal(err)
	}
	if tool.ran != 1 || resp.Message != "Hay 3." {
		t.Fatalf("ran=%d msg=%q", tool.ran, resp.Message)
	}
	joined := strings.Join(events, ",")
	if !strings.Contains(joined, "tool_call") || !strings.Contains(joined, "tool_result") {
		t.Fatalf("activity events %v", events)
	}
}

func TestModelLabelWithoutPorts(t *testing.T) {
	o := &AgentOrchestrator{}
	if o.modelLabel("") != "datos" || o.modelLabel("sales_orders") != "Sales Orders" || o.modelIcon("x") != "database" {
		t.Fatal("nil-port fallbacks wrong")
	}
	o = NewAgentOrchestrator(OrchestratorConfig{Catalog: testCatalog(), Models: CatalogRegistry{Catalog: testCatalog()}})
	if o.modelLabel("stock") != "Existencias" || o.modelIcon("stock") != "warehouse" || o.resolveKey("cliente") != "customers" {
		t.Fatal("catalog-backed labels wrong")
	}
}
