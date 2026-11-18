package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/agent/contextmgr"
	"github.com/asteby/metacore-kernel/agent/tools"
)

// plainTool is a tool with no PromptHint: it must add nothing to the prompt.
type plainTool struct{}

func (plainTool) Name() string                       { return "plain" }
func (plainTool) Description() string                { return "plain" }
func (plainTool) Category() string                   { return "data" }
func (plainTool) Parameters() map[string]interface{} { return nil }
func (plainTool) Execute(context.Context, map[string]interface{}, *contextmgr.ConversationContext) (interface{}, error) {
	return nil, nil
}

func TestToolHintsSection_FollowsRegisteredTools(t *testing.T) {
	if got := toolHintsSection(nil); got != "" {
		t.Fatalf("registry nil debe dar vacío, dio %q", got)
	}

	reg := tools.NewToolRegistry()
	reg.Register(plainTool{})
	if got := toolHintsSection(reg); got != "" {
		t.Fatalf("una herramienta sin pista no debe agregar nada, dio %q", got)
	}

	reg.Register(NewShowChartTool())
	got := toolHintsSection(reg)
	for _, must := range []string{"USO DE HERRAMIENTAS ESPECÍFICAS", "show_chart", "mismo turno", "rows"} {
		if !strings.Contains(got, must) {
			t.Fatalf("falta %q en la sección de pistas: %q", must, got)
		}
	}
	if strings.Contains(got, "plain") {
		t.Fatalf("la herramienta sin pista no debe aparecer: %q", got)
	}
}

func TestTurnSystemPrompt_AppendsHintsBeforeClosingRules(t *testing.T) {
	reg := tools.NewToolRegistry()
	reg.Register(NewShowChartTool())
	o := &AgentOrchestrator{}

	got := o.turnSystemPrompt("PROMPT DEL HOST", reg)
	iHost := strings.Index(got, "PROMPT DEL HOST")
	iHint := strings.Index(got, "show_chart:")
	iClose := strings.Index(got, closingRules)
	if iHost != 0 || iHint < 0 || iClose < 0 || !(iHost < iHint && iHint < iClose) {
		t.Fatalf("orden esperado host < pista < reglas de cierre, got %d/%d/%d\n%s", iHost, iHint, iClose, got)
	}

	// Sin show_chart registrada el prompt queda como antes.
	empty := o.turnSystemPrompt("PROMPT DEL HOST", tools.NewToolRegistry())
	if empty != "PROMPT DEL HOST\n\n"+closingRules {
		t.Fatalf("sin herramientas con pista el prompt no debe cambiar: %q", empty)
	}
}
