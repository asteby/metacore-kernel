package agent

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/agent/contextmgr"
	"github.com/asteby/metacore-kernel/agent/tools"

	"github.com/google/uuid"
)

// funcTool is a configurable tool for execution-policy tests.
type funcTool struct {
	name     string
	category string
	timeout  time.Duration
	label    string
	fn       func(ctx context.Context) (interface{}, error)
}

func (t *funcTool) Name() string                       { return t.name }
func (t *funcTool) Description() string                { return "test" }
func (t *funcTool) Category() string                   { return t.category }
func (t *funcTool) Parameters() map[string]interface{} { return map[string]interface{}{} }
func (t *funcTool) Execute(ctx context.Context, _ map[string]interface{}, _ *contextmgr.ConversationContext) (interface{}, error) {
	return t.fn(ctx)
}
func (t *funcTool) ToolTimeout() time.Duration { return t.timeout }
func (t *funcTool) ProgressLabel(map[string]interface{}) string {
	return t.label
}

func newTestOrch(p toolPolicy) *AgentOrchestrator {
	return &AgentOrchestrator{maxIterations: 12, policy: p}
}

func calls(names ...string) []tools.ToolCall {
	out := make([]tools.ToolCall, len(names))
	for i, n := range names {
		out[i] = tools.ToolCall{ID: "c" + string(rune('a'+i)), ToolName: n}
	}
	return out
}

func TestRunToolCalls_ParallelSafeRunConcurrentlyInOrder(t *testing.T) {
	reg := tools.NewToolRegistry()
	sleep := func(v string) func(context.Context) (interface{}, error) {
		return func(context.Context) (interface{}, error) { time.Sleep(200 * time.Millisecond); return v, nil }
	}
	reg.Register(&funcTool{name: "a", category: "data", fn: sleep("A")})
	reg.Register(&funcTool{name: "b", category: "data", fn: sleep("B")})
	reg.Register(&funcTool{name: "c", category: "knowledge", fn: sleep("C")})
	o := newTestOrch(toolPolicy{})
	start := time.Now()
	res, tr := o.runToolCalls(context.Background(), calls("a", "b", "c"), &contextmgr.ConversationContext{}, reg, 1)
	if el := time.Since(start); el > 450*time.Millisecond {
		t.Fatalf("three 200ms read-only calls must overlap, took %v", el)
	}
	for i, want := range []string{"A", "B", "C"} {
		if res[i].Result != want || !tr[i].Parallel || tr[i].Iteration != 1 {
			t.Fatalf("result %d = %v (trace %+v), want %s in call order", i, res[i].Result, tr[i], want)
		}
	}
}

func TestRunToolCalls_ActionToolsStaySequential(t *testing.T) {
	reg := tools.NewToolRegistry()
	var running, maxSeen int32
	fn := func(context.Context) (interface{}, error) {
		n := atomic.AddInt32(&running, 1)
		if n > atomic.LoadInt32(&maxSeen) {
			atomic.StoreInt32(&maxSeen, n)
		}
		time.Sleep(30 * time.Millisecond)
		atomic.AddInt32(&running, -1)
		return "ok", nil
	}
	reg.Register(&funcTool{name: "w1", category: "action", fn: fn})
	reg.Register(&funcTool{name: "w2", category: "action", fn: fn})
	o := newTestOrch(toolPolicy{})
	o.runToolCalls(context.Background(), calls("w1", "w2"), &contextmgr.ConversationContext{}, reg, 1)
	if maxSeen != 1 {
		t.Fatalf("mutating tools must not overlap, saw %d at once", maxSeen)
	}
}

func TestRunOne_TimeoutAbandonsToolThatIgnoresContext(t *testing.T) {
	reg := tools.NewToolRegistry()
	reg.Register(&funcTool{name: "slow", category: "data", timeout: 50 * time.Millisecond,
		fn: func(context.Context) (interface{}, error) { time.Sleep(2 * time.Second); return "late", nil }})
	o := newTestOrch(toolPolicy{})
	start := time.Now()
	res, tr := o.runOne(context.Background(), tools.ToolCall{ID: "x", ToolName: "slow"}, nil, reg)
	if time.Since(start) > time.Second {
		t.Fatalf("the loop must not wait for a tool past its timeout")
	}
	if res.Success || !tr.TimedOut || tr.Attempts != 1 {
		t.Fatalf("want timed-out failure without retry, got %+v / %+v", res, tr)
	}
}

func TestRunOne_RetriesOnlyTransientErrors(t *testing.T) {
	reg := tools.NewToolRegistry()
	var n int32
	reg.Register(&funcTool{name: "flaky", category: "data", fn: func(context.Context) (interface{}, error) {
		if atomic.AddInt32(&n, 1) == 1 {
			return nil, tools.Transient(errors.New("HTTP 503"))
		}
		return "ok", nil
	}})
	var m int32
	reg.Register(&funcTool{name: "broken", category: "data", fn: func(context.Context) (interface{}, error) {
		atomic.AddInt32(&m, 1)
		return nil, errors.New("territorio no encontrado")
	}})
	o := newTestOrch(toolPolicy{})
	res, tr := o.runOne(context.Background(), tools.ToolCall{ID: "1", ToolName: "flaky"}, nil, reg)
	if !res.Success || tr.Attempts != 2 {
		t.Fatalf("transient error must be retried once: %+v %+v", res, tr)
	}
	res, tr = o.runOne(context.Background(), tools.ToolCall{ID: "2", ToolName: "broken"}, nil, reg)
	if res.Success || tr.Attempts != 1 || m != 1 {
		t.Fatalf("a final error must not be retried: %+v %+v (ran %d)", res, tr, m)
	}
	o = newTestOrch(toolPolicy{ToolRetries: -1})
	atomic.StoreInt32(&n, 0)
	if res, _ := o.runOne(context.Background(), tools.ToolCall{ID: "3", ToolName: "flaky"}, nil, reg); res.Success {
		t.Fatalf("ToolRetries<0 disables retries")
	}
}

func TestRunOne_PanicBecomesError(t *testing.T) {
	reg := tools.NewToolRegistry()
	reg.Register(&funcTool{name: "boom", category: "data", fn: func(context.Context) (interface{}, error) { panic("nil map") }})
	res, _ := newTestOrch(toolPolicy{}).runOne(context.Background(), tools.ToolCall{ID: "1", ToolName: "boom"}, nil, reg)
	if res.Success || !strings.Contains(res.Error, "boom") {
		t.Fatalf("panic must surface as a tool error, got %+v", res)
	}
}

func TestFormatForLLMLimit_CompactAndTruncatedWithNotice(t *testing.T) {
	r := tools.ToolResult{ToolName: "q", Success: true, Result: map[string]interface{}{"rows": strings.Repeat("ñ", 5000)}}
	text, cut := r.FormatForLLMLimit(1000)
	if !cut || !strings.Contains(text, "RESULTADO TRUNCADO") || len(text) > 1300 {
		t.Fatalf("want a capped result with notice, got %d bytes cut=%v", len(text), cut)
	}
	small, cut := tools.ToolResult{ToolName: "q", Success: true, Result: map[string]int{"total": 3}}.FormatForLLMLimit(1000)
	if cut || strings.Contains(small, "\n  ") {
		t.Fatalf("small results stay whole and compact: %q", small)
	}
}

func TestRegistry_ListIsSortedForStablePrompts(t *testing.T) {
	reg := tools.NewToolRegistry()
	for _, n := range []string{"zeta", "alfa", "mu", "beta"} {
		reg.Register(&funcTool{name: n, category: "data"})
	}
	var got []string
	for _, tl := range reg.List() {
		got = append(got, tl.Name())
	}
	if strings.Join(got, ",") != "alfa,beta,mu,zeta" {
		t.Fatalf("List must be sorted, got %v", got)
	}
	defs := reg.ToOpenAIFormat()
	if defs[0]["name"] != "alfa" || defs[3]["name"] != "zeta" {
		t.Fatalf("tool definitions must follow the same order")
	}
}

// recordingAI records whether tools were offered on each call.
type recordingAI struct {
	steps     []*AIResponse
	toolsSeen []bool
}

func (r *recordingAI) GenerateChatWithTools(_ context.Context, _ []map[string]interface{}, _ string, defs []map[string]interface{}) (*AIResponse, error) {
	i := len(r.toolsSeen)
	r.toolsSeen = append(r.toolsSeen, defs != nil)
	if i < len(r.steps) {
		return r.steps[i], nil
	}
	return &AIResponse{Message: "fin"}, nil
}

func TestAgentLoop_TraceStatsAndHook(t *testing.T) {
	reg := tools.NewToolRegistry()
	reg.Register(NewFinalAnswerTool())
	reg.Register(&funcTool{name: "buscar", category: "data", label: "search|Buscando en hallazgos…",
		fn: func(context.Context) (interface{}, error) { return map[string]int{"total": 7}, nil }})
	ai := &recordingAI{steps: []*AIResponse{
		{ToolCalls: []tools.ToolCall{{ID: "1", ToolName: "buscar"}, {ID: "2", ToolName: "buscar"}}},
		finalAnswer("f", "Hay **7** hallazgos."),
	}}
	var hooked int
	var labels []string
	o := &AgentOrchestrator{aiService: ai, maxIterations: 12, policy: toolPolicy{
		OnToolTrace: func(context.Context, *contextmgr.ConversationContext, ToolTrace) { hooked++ },
	}}
	o.SetStreamCallback(func(_ uuid.UUID, ev AIActivityEvent) {
		if ev.Type == "tool_call" {
			labels = append(labels, ev.Message)
		}
	})
	resp, err := o.agentLoop(context.Background(), []map[string]interface{}{{"role": "user", "content": "¿cuántos hallazgos?"}}, "", &contextmgr.ConversationContext{ConversationID: uuid.New()}, reg)
	if err != nil {
		t.Fatal(err)
	}
	trs, _ := resp.Metadata["tool_trace"].([]ToolTrace)
	st, _ := resp.Metadata["turn_stats"].(TurnStats)
	if len(trs) != 2 || hooked != 2 || st.LLMCalls != 2 || st.Iterations != 2 || trs[0].ResultBytes == 0 {
		t.Fatalf("want 2 traces/hooks and 2 llm calls, got traces=%+v hooked=%d stats=%+v", trs, hooked, st)
	}
	if len(labels) != 2 || labels[0] != "search|Buscando en hallazgos…" {
		t.Fatalf("progress must use the tool's own label, got %v", labels)
	}
}

func TestAgentLoop_TurnBudgetForcesAnswerWithoutTools(t *testing.T) {
	reg := tools.NewToolRegistry()
	reg.Register(NewFinalAnswerTool())
	reg.Register(&funcTool{name: "lento", category: "data",
		fn: func(context.Context) (interface{}, error) { time.Sleep(30 * time.Millisecond); return "dato", nil }})
	ai := &recordingAI{steps: []*AIResponse{
		{ToolCalls: []tools.ToolCall{{ID: "1", ToolName: "lento"}}},
		{Message: "Con lo obtenido: dato."},
	}}
	o := &AgentOrchestrator{aiService: ai, maxIterations: 12, policy: toolPolicy{MaxTurnDuration: 10 * time.Millisecond}}
	resp, err := o.agentLoop(context.Background(), []map[string]interface{}{{"role": "user", "content": "investiga a fondo"}}, "", &contextmgr.ConversationContext{ConversationID: uuid.New()}, reg)
	if err != nil {
		t.Fatal(err)
	}
	if len(ai.toolsSeen) != 2 || !ai.toolsSeen[0] || ai.toolsSeen[1] {
		t.Fatalf("after the budget the model must answer without tools, saw %v", ai.toolsSeen)
	}
	if st := resp.Metadata["turn_stats"].(TurnStats); !st.BudgetForced {
		t.Fatalf("stats must record the forced answer: %+v", st)
	}
}

func TestIsDeferredNarration_TrailingColonAnnouncement(t *testing.T) {
	for _, m := range []string{"Ahora traigo el resumen de Zona Norte:", "Enseguida reviso las alertas."} {
		if !isDeferredNarration(m) {
			t.Fatalf("%q announces work it did not do", m)
		}
	}
	if isDeferredNarration("Resumen:\n- Zona Sur: 12 hallazgos\n- Zona Norte: 9") {
		t.Fatalf("a finished multi-line answer is not narration")
	}
}
