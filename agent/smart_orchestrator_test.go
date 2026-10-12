package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/agent/contextmgr"
	"github.com/asteby/metacore-kernel/agent/tools"
	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type fakeDirectory struct {
	agents []AgentProfile
	err    error
}

func (f fakeDirectory) AgentsFor(uuid.UUID) ([]AgentProfile, error) { return f.agents, f.err }

type fakeRunner struct {
	got AgentProfile
	err error
}

func (f *fakeRunner) RunAgent(_ context.Context, msg string, _ uuid.UUID, a AgentProfile) (*AgentResponse, error) {
	f.got = a
	if f.err != nil {
		return nil, f.err
	}
	return &AgentResponse{Message: "from " + a.Name + ": " + msg}, nil
}

func newSmart(t *testing.T, dir AgentDirectory, ai AIService) (*SmartOrchestrator, *fakeRunner) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	r := &fakeRunner{}
	return NewSmartOrchestrator(dir, r, contextmgr.NewManager(db), ai), r
}

func TestSmartRoutesToAgentChosenByModel(t *testing.T) {
	sales := AgentProfile{ID: uuid.New(), Name: "Ventas", Description: "Atiende ventas y precios", Tools: []string{"search_products"}}
	support := AgentProfile{ID: uuid.New(), Name: "Soporte", Description: "Soporte técnico"}
	ai := &captureAI{steps: []*AIResponse{{ToolCalls: []tools.ToolCall{{
		ID: "r", ToolName: "route_to_agent",
		Parameters: map[string]interface{}{"agent_id": support.ID.String(), "agent_name": "Soporte", "confidence": 0.9, "reasoning": "falla"},
	}}}}}
	so, runner := newSmart(t, fakeDirectory{agents: []AgentProfile{sales, support}}, ai)

	conv := uuid.New()
	resp, err := so.ProcessMessage(context.Background(), "no enciende", conv, OrchestratorRef{ID: uuid.New(), Name: "Orq"})
	if err != nil {
		t.Fatal(err)
	}
	if runner.got.ID != support.ID || resp.Message != "from Soporte: no enciende" {
		t.Fatalf("routed to %v: %q", runner.got.Name, resp.Message)
	}
	if resp.Metadata["routed_to_agent"] != "Soporte" || resp.Metadata["routing_confidence"] != 0.9 || resp.Metadata["orchestrator_name"] != "Orq" {
		t.Fatalf("%v", resp.Metadata)
	}
	if !strings.Contains(ai.prompts[0], "### Ventas") || !strings.Contains(ai.prompts[0], "search_products") || !strings.Contains(ai.prompts[0], "ventas") {
		t.Fatal("routing prompt must describe every agent")
	}

	// Next turn: history of the previous decision reaches the prompt and AI failure falls back to it.
	ai.steps = append(ai.steps, nil)
	so2 := NewSmartOrchestrator(fakeDirectory{agents: []AgentProfile{sales, support}}, runner, so.contextMgr, &failingAI{})
	if _, err := so2.ProcessMessage(context.Background(), "otra vez", conv, OrchestratorRef{ID: uuid.New()}); err != nil {
		t.Fatal(err)
	}
	if runner.got.ID != support.ID {
		t.Fatalf("fallback must continue with the previous agent, got %s", runner.got.Name)
	}
}

type failingAI struct{}

func (failingAI) GenerateChatWithTools(context.Context, []map[string]interface{}, string, []map[string]interface{}) (*AIResponse, error) {
	return nil, errors.New("down")
}

func TestSmartFallsBackToFirstAgentAndValidatesIDs(t *testing.T) {
	a := AgentProfile{ID: uuid.New(), Name: "Uno"}
	b := AgentProfile{ID: uuid.New(), Name: "Dos"}
	so, runner := newSmart(t, fakeDirectory{agents: []AgentProfile{a, b}}, failingAI{})
	if _, err := so.ProcessMessage(context.Background(), "hola", uuid.New(), OrchestratorRef{}); err != nil {
		t.Fatal(err)
	}
	if runner.got.ID != a.ID {
		t.Fatalf("default agent should be the first, got %s", runner.got.Name)
	}

	caps := so.buildAgentCapabilities([]AgentProfile{a, b})
	d, err := so.parseRoutingDecision(map[string]interface{}{"agent_id": uuid.NewString(), "confidence": 0.99}, caps)
	if err != nil || d.SelectedAgentID != a.ID || d.Confidence != 0.5 {
		t.Fatalf("unknown agent id must fall back to the first: %+v %v", d, err)
	}
	d, err = so.parseRoutingDecision(map[string]interface{}{"agent_id": "not-a-uuid", "agent_name": "dos"}, caps)
	if err != nil || d.SelectedAgentID != b.ID {
		t.Fatalf("name match by case-insensitive lookup: %+v %v", d, err)
	}
	if _, err := so.parseRoutingDecision(map[string]interface{}{"agent_id": "not-a-uuid"}, caps); err == nil {
		t.Fatal("invalid id without a matching name must error")
	}
	d, _ = so.parseRoutingFromMessage("creo que Dos es mejor", caps)
	if d.SelectedAgentID != b.ID {
		t.Fatal("name mention routing")
	}
	if _, err := so.parseRoutingFromMessage("x", nil); err == nil {
		t.Fatal("no agents must error")
	}
}

func TestSmartErrors(t *testing.T) {
	so, _ := newSmart(t, fakeDirectory{err: errors.New("db")}, failingAI{})
	if _, err := so.ProcessMessage(context.Background(), "x", uuid.New(), OrchestratorRef{}); err == nil {
		t.Fatal("directory error must surface")
	}
	so, _ = newSmart(t, fakeDirectory{}, failingAI{})
	if _, err := so.ProcessMessage(context.Background(), "x", uuid.New(), OrchestratorRef{Name: "Vacío"}); err == nil || !strings.Contains(err.Error(), "Vacío") {
		t.Fatalf("empty orchestrator must error, got %v", err)
	}
	a := AgentProfile{ID: uuid.New(), Name: "A"}
	so, runner := newSmart(t, fakeDirectory{agents: []AgentProfile{a}}, failingAI{})
	runner.err = errors.New("agent blew up")
	if _, err := so.ProcessMessage(context.Background(), "x", uuid.New(), OrchestratorRef{}); err == nil || !strings.Contains(err.Error(), "agent processing failed") {
		t.Fatalf("runner error must be wrapped, got %v", err)
	}
	stats := so.GetAgentStats(uuid.New())
	if stats["total_agents"] != 1 {
		t.Fatalf("%v", stats)
	}
	so.directory = fakeDirectory{err: errors.New("x")}
	if so.GetAgentStats(uuid.New())["error"] == nil {
		t.Fatal("stats error")
	}
}
