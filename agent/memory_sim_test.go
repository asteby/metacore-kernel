package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/asteby/metacore-kernel/agent/contextmgr"

	"github.com/asteby/metacore-kernel/agent/tools"
)

var (
	simBranches = []string{"Centro", "Norte", "Sur", "Oriente", "Poniente"}
	simMonths   = []string{"diciembre", "enero", "febrero", "marzo", "abril", "mayo", "junio", "julio", "agosto", "septiembre", "octubre", "noviembre"}
)

func simUserMessage(k int) string {
	if k == 30 {
		return "Regresando a lo primero que te pregunté: ¿cuánto vendió la sucursal Norte en enero?"
	}
	return fmt.Sprintf("¿Cuánto vendió la sucursal %s en %s? Desglosa por categoría de producto y compáralo contra el mes anterior, por favor.",
		simBranches[k%5], simMonths[k%12])
}

func simAnswer(k int) string {
	b, m := simBranches[k%5], simMonths[k%12]
	return fmt.Sprintf("La sucursal *%s* vendió *%d MXN* en %s, en %d órdenes. Por categoría: • Llantas: %d MXN • Servicio: %d MXN • Refacciones: %d MXN. "+
		"Contra el mes anterior subió %d%%, sobre todo por llantas de rin 16 y alineaciones. La orden más grande fue de %d MXN "+
		"y el ticket promedio quedó en %d MXN. Si quieres, te preparo el detalle por vendedor o por día de la semana.",
		b, 4000+k*137, m, 20+k, 2000+k*50, 1200+k*40, 800+k*47, 3+k%7, 900+k*11, 180+k*3)
}

// simAI plays a two-call turn: one data tool, then final_answer with the
// memory fields. It records what each call sent.
type simAI struct {
	turn     int
	call     int
	prompts  []string
	defs     []string
	messages [][]map[string]interface{}
}

func (s *simAI) GenerateChatWithTools(_ context.Context, msgs []map[string]interface{}, system string, defs []map[string]interface{}) (*AIResponse, error) {
	raw, _ := json.Marshal(defs)
	s.prompts = append(s.prompts, system)
	s.defs = append(s.defs, string(raw))
	s.messages = append(s.messages, msgs)
	s.call++
	k := s.turn
	if s.call%2 == 1 {
		return &AIResponse{ToolCalls: []tools.ToolCall{{ID: fmt.Sprintf("q%d", k), ToolName: "sales_report",
			Parameters: map[string]interface{}{"branch": simBranches[k%5], "month": simMonths[k%12]}}}}, nil
	}
	return &AIResponse{ToolCalls: []tools.ToolCall{{ID: fmt.Sprintf("f%d", k), ToolName: "final_answer", Parameters: map[string]interface{}{
		"message": simAnswer(k),
		"title":   fmt.Sprintf("Ventas %s %s", simBranches[k%5], simMonths[k%12]),
		"summary": fmt.Sprintf("Vendió %d MXN en %d órdenes; subió %d%% contra el mes anterior", 4000+k*137, 20+k, 3+k%7),
		"facts":   []interface{}{fmt.Sprintf("Ventas %s %s: %d MXN", simBranches[k%5], simMonths[k%12], 4000+k*137)},
	}}}}, nil
}

// simReport is what each mode costs per turn (estimated tokens).
type simReport struct {
	total    []int // first call of each turn
	budgets  []PromptBudget
	uncached []int // first call minus the prefix shared with the previous turn's last call
	last     string
	lastCtx  string
	ai       *simAI
}

func sameMessage(a, b map[string]interface{}) bool {
	return a["role"] == b["role"] && a["content"] == b["content"] && reflect.DeepEqual(a["tool_calls"], b["tool_calls"])
}

// newSimOrchestrator is newTurnOrchestrator without cgo: the context manager
// gets a sqlmock-backed handle, so the copilot_messages read fails and the
// history stays in memory, as with the sqlite handle of the other tests.
func newSimOrchestrator(t *testing.T, ai AIService, base ...tools.BaseTool) *AgentOrchestrator {
	t.Helper()
	mockDB, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mockDB.Close() })
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: mockDB, PreferSimpleProtocol: true}), &gorm.Config{SkipDefaultTransaction: true, Logger: logger.Discard})
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

// runMemorySim plays 30 turns through CopilotService. The history the
// context manager keeps is cut to the last 20 messages after every turn, as
// refreshCopilotHistory does in production.
func runMemorySim(t *testing.T, layered bool) simReport {
	t.Helper()
	ai := &simAI{}
	rows := make([]interface{}, 25)
	for i := range rows {
		rows[i] = map[string]interface{}{"categoria": "Llantas", "importe": 1234.5, "ordenes": 7, "sucursal": "Centro"}
	}
	report := &stubTool{name: "sales_report", result: map[string]interface{}{"data": rows, "total": float64(25), "importe_total": 30862.5}}
	o := newSimOrchestrator(t, ai, report)
	var budgets []PromptBudget
	o.onBudget = func(_ context.Context, b PromptBudget) { budgets = append(budgets, b) }
	svc, err := NewCopilotService(CopilotConfig{
		Orchestrator:  o,
		Brand:         fakeBrand{Personality{Name: "Aby", SystemPrompt: strings.Repeat("Eres Aby, la asistente del ERP. ", 40), PlatformName: "Pitsline"}},
		Clock:         fixedClock{time.Date(2026, 11, 15, 10, 0, 0, 0, mx)},
		LayeredMemory: layered,
	})
	if err != nil {
		t.Fatal(err)
	}
	org, user, session := uuid.New(), uuid.New(), uuid.New()
	rep := simReport{ai: ai}
	prevLastCall := -1
	for k := 1; k <= 30; k++ {
		ai.turn = k
		firstCall := len(ai.messages)
		firstBudget := len(budgets)
		res, err := svc.ProcessCopilotMessage(context.Background(), org, user, session, simUserMessage(k), nil, false)
		if err != nil || res.Message == "" {
			t.Fatalf("turn %d: %v %q", k, err, res.Message)
		}
		b := budgets[firstBudget]
		rep.total = append(rep.total, b.Total)
		rep.budgets = append(rep.budgets, b)
		cached := b.System + b.Tools
		if prevLastCall >= 0 && ai.prompts[firstCall] == ai.prompts[prevLastCall] && ai.defs[firstCall] == ai.defs[prevLastCall] {
			prev, cur := ai.messages[prevLastCall], ai.messages[firstCall]
			for i := 0; i < len(prev) && i < len(cur) && sameMessage(prev[i], cur[i]); i++ {
				cached += estimateMessageTokens(cur[i])
			}
		} else if prevLastCall >= 0 {
			cached = 0
		}
		rep.uncached = append(rep.uncached, b.Total-cached)
		prevLastCall = len(ai.messages) - 1
		sent := ai.messages[firstCall]
		var all []string
		for _, m := range sent {
			c, _ := m["content"].(string)
			all = append(all, c)
		}
		rep.last = strings.Join(all, "\n---\n")
		rep.lastCtx, _ = sent[len(sent)-1]["content"].(string)

		cc := o.contextMgr.GetCopilotContext(session, org, user)
		if n := len(cc.History); n > 20 {
			cc.History = cc.History[n-20:]
		}
	}
	return rep
}

func TestLayeredMemorySimulation30Turns(t *testing.T) {
	base := runMemorySim(t, false)
	lay := runMemorySim(t, true)

	t.Logf("turn | budgeted history: input / uncached | layered memory: input / uncached")
	sum := func(xs []int, from int) (s int) {
		for _, x := range xs[from:] {
			s += x
		}
		return s
	}
	for _, k := range []int{1, 2, 3, 5, 8, 10, 12, 15, 20, 25, 28, 29, 30} {
		t.Logf("%4d | %6d / %6d | %6d / %6d", k, base.total[k-1], base.uncached[k-1], lay.total[k-1], lay.uncached[k-1])
	}
	t.Logf("sum turns 1-30 | %6d / %6d | %6d / %6d", sum(base.total, 0), sum(base.uncached, 0), sum(lay.total, 0), sum(lay.uncached, 0))

	// Flat: once the window and the index are full, a turn costs about the
	// same as the one before (no growth with the conversation length).
	minL, maxL := lay.total[9], lay.total[9]
	for _, v := range lay.total[9:] {
		minL, maxL = min(minL, v), max(maxL, v)
	}
	t.Logf("layered turns 10-30: min %d, max %d", minL, maxL)
	t.Logf("turn 30 sections, budgeted history: %+v", base.budgets[29])
	t.Logf("turn 30 sections, layered memory:   %+v", lay.budgets[29])
	if maxL-minL > 1200 {
		t.Fatalf("layered input not flat: %d..%d", minL, maxL)
	}
	if lay.total[29] > lay.total[9]+1000 {
		t.Fatalf("layered input grows with the conversation: turn 10 %d, turn 30 %d", lay.total[9], lay.total[29])
	}

	// The last question refers to turn 1: only layered memory still has it.
	if strings.Contains(base.last, "Ventas Norte enero") || strings.Contains(base.last, "sucursal Norte en enero? Desglosa") {
		t.Fatal("baseline unexpectedly kept turn 1")
	}
	if !strings.Contains(lay.last, "#1 · Ventas Norte enero → Vendió 4137 MXN") && !strings.Contains(lay.lastCtx, "[Turno relacionado #1]") {
		t.Fatalf("layered memory lost turn 1:\n%s", lay.last)
	}
	if !strings.Contains(lay.lastCtx, "[Turno relacionado #1]") || !strings.Contains(lay.lastCtx, "Ventas Norte enero: 4137 MXN") {
		t.Fatalf("turn 1 not prefetched for a question about it:\n%s", lay.lastCtx)
	}

	// System prompt and tools stay byte-identical across every call.
	for i := range lay.ai.prompts {
		if lay.ai.prompts[i] != lay.ai.prompts[0] || lay.ai.defs[i] != lay.ai.defs[0] {
			t.Fatalf("call %d: system prompt or tools changed", i)
		}
	}
	for _, name := range []string{`"recall_turn"`, `"remember_fact"`, `"forget_fact"`, `"facts"`} {
		if !strings.Contains(lay.ai.defs[0], name) {
			t.Fatalf("tools miss %s", name)
		}
	}
	if strings.Contains(base.ai.defs[0], "recall_turn") || strings.Contains(base.ai.defs[0], `"facts"`) {
		t.Fatal("memory tools must be opt-in")
	}
}

// Within a window step the sent history is append-only: everything before the
// previous turn's user message is re-sent byte-identical.
func TestLayeredMemoryHistoryIsAppendOnlyWithinAStep(t *testing.T) {
	lay := runMemorySim(t, true)
	ai := lay.ai
	first := func(k int) []map[string]interface{} { return ai.messages[(k-1)*2] }
	// Turns 7 and 8 (n=6 and 7 done) share the step that starts after turn 3.
	t7, t8 := first(7), first(8)
	if t7[0]["content"] != t8[0]["content"] || !strings.HasPrefix(t7[0]["content"].(string), memoryIndexHeader) {
		t.Fatal("index preamble must be identical within a step")
	}
	for i := 0; i < len(t7)-1; i++ {
		if !sameMessage(t7[i], t8[i]) {
			t.Fatalf("message %d changed within a step", i)
		}
	}
	if len(t8) != len(t7)+2 {
		t.Fatalf("one exchange appended: %d -> %d", len(t7), len(t8))
	}
}
