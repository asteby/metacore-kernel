package flow

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// memStore is an in-memory Store used by tests.
type memStore struct {
	mu         sync.Mutex
	executions map[uuid.UUID]*FlowExecution
	completed  []uuid.UUID
	failed     []uuid.UUID
}

func newMemStore() *memStore {
	return &memStore{executions: map[uuid.UUID]*FlowExecution{}}
}

func (m *memStore) SaveExecution(_ context.Context, e *FlowExecution) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *e
	m.executions[e.ID] = &cp
	return nil
}

func (m *memStore) UpdateExecution(_ context.Context, e *FlowExecution) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *e
	m.executions[e.ID] = &cp
	return nil
}

func (m *memStore) RecordCompletion(_ context.Context, flowID uuid.UUID, _ int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.completed = append(m.completed, flowID)
	return nil
}

func (m *memStore) RecordFailure(_ context.Context, flowID uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failed = append(m.failed, flowID)
	return nil
}

func (m *memStore) snapshot(id uuid.UUID) *FlowExecution {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.executions[id]; ok {
		cp := *e
		return &cp
	}
	return nil
}

// progressRecorder collects ProgressSink events.
type progressRecorder struct {
	mu     sync.Mutex
	events []string
}

func (p *progressRecorder) OnProgress(_ uuid.UUID, _ uuid.UUID, _ uuid.UUID, nodeID, status string) {
	p.mu.Lock()
	p.events = append(p.events, nodeID+":"+status)
	p.mu.Unlock()
}

// waitForExecution polls the store until the execution leaves Running, or
// the timeout expires.
func waitForExecution(t *testing.T, store *memStore, id uuid.UUID, timeout time.Duration) *FlowExecution {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if e := store.snapshot(id); e != nil && e.Status != ExecutionRunning {
			return e
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("execution %s did not finish within %s", id, timeout)
	return nil
}

func newSimpleFlow(nodes []FlowNode, edges []FlowEdge) *Flow {
	return &Flow{
		ID:             uuid.New(),
		OrganizationID: uuid.New(),
		Name:           "test",
		Status:         FlowStatusActive,
		TriggerType:    TriggerManual,
		Nodes:          nodes,
		Edges:          edges,
	}
}

// ─── tests ─────────────────────────────────────────────────────────────────

func TestEngineExecuteSequential(t *testing.T) {
	store := newMemStore()
	engine := NewEngine(Config{Store: store})

	flow := newSimpleFlow(
		[]FlowNode{
			{ID: "trigger", Type: NodeTypeTrigger},
			{ID: "set", Type: NodeTypeSetVariable, Data: FlowNodeData{Config: map[string]interface{}{
				"name": "greeting", "value": "hello {{$trigger.data.name}}",
			}}},
		},
		[]FlowEdge{{ID: "e1", Source: "trigger", Target: "set"}},
	)

	exec, err := engine.ExecuteFlow(flow, TriggerManual, map[string]interface{}{"name": "world"}, nil)
	if err != nil {
		t.Fatalf("ExecuteFlow: %v", err)
	}

	final := waitForExecution(t, store, exec.ID, 2*time.Second)
	if final.Status != ExecutionCompleted {
		t.Fatalf("expected completed, got %s (err=%q)", final.Status, final.ErrorMessage)
	}
	if got := final.Variables["greeting"]; got != "hello world" {
		t.Fatalf("greeting = %v, want 'hello world'", got)
	}
	if len(final.NodeExecutions) != 2 {
		t.Fatalf("expected 2 node logs, got %d", len(final.NodeExecutions))
	}
}

func TestEngineConditionalBranching(t *testing.T) {
	engine := NewEngine(Config{})
	flow := newSimpleFlow(
		[]FlowNode{
			{ID: "trigger", Type: NodeTypeTrigger},
			{ID: "cond", Type: NodeTypeCondition, Data: FlowNodeData{Config: map[string]interface{}{
				"field":    "$trigger.data.score",
				"operator": "gt",
				"value":    "50",
			}}},
			{ID: "high", Type: NodeTypeSetVariable, Data: FlowNodeData{Config: map[string]interface{}{"name": "tier", "value": "high"}}},
			{ID: "low", Type: NodeTypeSetVariable, Data: FlowNodeData{Config: map[string]interface{}{"name": "tier", "value": "low"}}},
		},
		[]FlowEdge{
			{ID: "e1", Source: "trigger", Target: "cond"},
			{ID: "e2", Source: "cond", Target: "high", SourceHandle: "true"},
			{ID: "e3", Source: "cond", Target: "low", SourceHandle: "false"},
		},
	)

	got, err := engine.TestFlowInline(flow, map[string]interface{}{"score": 75}, nil)
	if err != nil {
		t.Fatalf("TestFlowInline: %v", err)
	}
	if got.Status != "completed" {
		t.Fatalf("status %s err=%s", got.Status, got.Error)
	}
	if got.Variables["tier"] != "high" {
		t.Fatalf("tier = %v want high", got.Variables["tier"])
	}

	got, _ = engine.TestFlowInline(flow, map[string]interface{}{"score": 25}, nil)
	if got.Variables["tier"] != "low" {
		t.Fatalf("tier = %v want low", got.Variables["tier"])
	}
}

func TestEngineLoopCount(t *testing.T) {
	engine := NewEngine(Config{})
	flow := newSimpleFlow(
		[]FlowNode{
			{ID: "trigger", Type: NodeTypeTrigger},
			{ID: "loop", Type: NodeTypeLoop, Data: FlowNodeData{Config: map[string]interface{}{
				"source": "$trigger.data.items",
			}}},
		},
		[]FlowEdge{{ID: "e1", Source: "trigger", Target: "loop"}},
	)

	got, err := engine.TestFlowInline(flow, map[string]interface{}{
		"items": []interface{}{"a", "b", "c"},
	}, nil)
	if err != nil {
		t.Fatalf("TestFlowInline: %v", err)
	}
	if got.Status != "completed" {
		t.Fatalf("status %s err=%s", got.Status, got.Error)
	}
	if got.Variables["$loop.count"] != 3 {
		t.Fatalf("$loop.count = %v want 3", got.Variables["$loop.count"])
	}
}

func TestEngineHTTPRequestNode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Caller") != "metacore" {
			t.Errorf("missing X-Caller header")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"echo":"` + r.URL.Query().Get("name") + `"}`))
	}))
	defer srv.Close()

	engine := NewEngine(Config{})
	flow := newSimpleFlow(
		[]FlowNode{
			{ID: "trigger", Type: NodeTypeTrigger},
			{ID: "http", Type: NodeTypeHTTPRequest, Data: FlowNodeData{Config: map[string]interface{}{
				"method": "GET",
				"url":    srv.URL + "/?name={{$trigger.data.who}}",
				"headers": map[string]interface{}{
					"X-Caller": "metacore",
				},
			}}},
		},
		[]FlowEdge{{ID: "e1", Source: "trigger", Target: "http"}},
	)

	got, err := engine.TestFlowInline(flow, map[string]interface{}{"who": "ada"}, nil)
	if err != nil {
		t.Fatalf("TestFlowInline: %v", err)
	}
	if got.Status != "completed" {
		t.Fatalf("status %s err=%s", got.Status, got.Error)
	}
	if got.Variables["$http_status"] != 200 {
		t.Fatalf("$http_status = %v want 200", got.Variables["$http_status"])
	}
	resp, ok := got.Variables["$response"].(map[string]interface{})
	if !ok {
		t.Fatalf("$response not a map: %T", got.Variables["$response"])
	}
	if resp["echo"] != "ada" {
		t.Fatalf("echo = %v want ada", resp["echo"])
	}
}

// customNode is a NodeExecutor that records the variables it observed.
type customNode struct {
	mu       sync.Mutex
	observed []string
}

func (c *customNode) Name() string { return "custom" }
func (c *customNode) Execute(_ context.Context, node *FlowNode, ec *ExecutionContext) (*NodeResult, error) {
	c.mu.Lock()
	c.observed = append(c.observed, ec.GetString("greeting"))
	c.mu.Unlock()
	return &NodeResult{Output: map[string]interface{}{"ran": true}}, nil
}

func TestEngineRegistryCustomNode(t *testing.T) {
	custom := &customNode{}
	engine := NewEngine(Config{})
	engine.RegisterNode("custom_record", custom)

	flow := newSimpleFlow(
		[]FlowNode{
			{ID: "trigger", Type: NodeTypeTrigger},
			{ID: "set", Type: NodeTypeSetVariable, Data: FlowNodeData{Config: map[string]interface{}{
				"name": "greeting", "value": "hi",
			}}},
			{ID: "rec", Type: "custom_record"},
		},
		[]FlowEdge{
			{ID: "e1", Source: "trigger", Target: "set"},
			{ID: "e2", Source: "set", Target: "rec"},
		},
	)

	got, _ := engine.TestFlowInline(flow, nil, nil)
	if got.Status != "completed" {
		t.Fatalf("status %s err=%s", got.Status, got.Error)
	}
	if len(custom.observed) != 1 || custom.observed[0] != "hi" {
		t.Fatalf("custom observed = %v", custom.observed)
	}
}

// failingNode always errors. Used to exercise error-propagation paths.
type failingNode struct{ msg string }

func (f failingNode) Name() string { return "failing" }
func (f failingNode) Execute(_ context.Context, _ *FlowNode, _ *ExecutionContext) (*NodeResult, error) {
	return nil, errors.New(f.msg)
}

func TestEngineErrorPropagation(t *testing.T) {
	store := newMemStore()
	engine := NewEngine(Config{Store: store})
	engine.RegisterNode("failing", failingNode{msg: "boom"})

	flow := newSimpleFlow(
		[]FlowNode{
			{ID: "trigger", Type: NodeTypeTrigger},
			{ID: "fail", Type: "failing"},
			{ID: "after", Type: NodeTypeSetVariable, Data: FlowNodeData{Config: map[string]interface{}{
				"name": "reached", "value": "yes",
			}}},
		},
		[]FlowEdge{
			{ID: "e1", Source: "trigger", Target: "fail"},
			{ID: "e2", Source: "fail", Target: "after"},
		},
	)

	exec, err := engine.ExecuteFlow(flow, TriggerManual, nil, nil)
	if err != nil {
		t.Fatalf("ExecuteFlow: %v", err)
	}
	final := waitForExecution(t, store, exec.ID, 2*time.Second)
	if final.Status != ExecutionFailed {
		t.Fatalf("expected failed, got %s", final.Status)
	}
	if final.ErrorNode != "fail" {
		t.Fatalf("errorNode = %s want fail", final.ErrorNode)
	}
	if final.ErrorMessage != "boom" {
		t.Fatalf("errorMessage = %q want boom", final.ErrorMessage)
	}
	if len(store.failed) != 1 {
		t.Fatalf("expected 1 failure recorded, got %d", len(store.failed))
	}
}

func TestEngineContinueOnError(t *testing.T) {
	engine := NewEngine(Config{})
	engine.RegisterNode("failing", failingNode{msg: "boom"})

	flow := newSimpleFlow(
		[]FlowNode{
			{ID: "trigger", Type: NodeTypeTrigger},
			{ID: "fail", Type: "failing", Data: FlowNodeData{ContinueOnError: true}},
			{ID: "errpath", Type: NodeTypeSetVariable, Data: FlowNodeData{Config: map[string]interface{}{
				"name": "handled", "value": "true",
			}}},
		},
		[]FlowEdge{
			{ID: "e1", Source: "trigger", Target: "fail"},
			{ID: "e2", Source: "fail", Target: "errpath", SourceHandle: "error"},
		},
	)

	got, _ := engine.TestFlowInline(flow, nil, nil)
	if got.Status != "completed" {
		t.Fatalf("status %s err=%s", got.Status, got.Error)
	}
	if got.Variables["handled"] != "true" {
		t.Fatalf("handled = %v want true", got.Variables["handled"])
	}
}

func TestEngineProgressSink(t *testing.T) {
	rec := &progressRecorder{}
	engine := NewEngine(Config{Progress: rec, Store: newMemStore()})

	flow := newSimpleFlow(
		[]FlowNode{
			{ID: "trigger", Type: NodeTypeTrigger},
			{ID: "set", Type: NodeTypeSetVariable, Data: FlowNodeData{Config: map[string]interface{}{
				"name": "x", "value": "1",
			}}},
		},
		[]FlowEdge{{ID: "e1", Source: "trigger", Target: "set"}},
	)

	exec, err := engine.ExecuteFlow(flow, TriggerManual, nil, nil)
	if err != nil {
		t.Fatalf("ExecuteFlow: %v", err)
	}
	store := engine.cfg.Store.(*memStore)
	waitForExecution(t, store, exec.ID, 2*time.Second)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.events) < 2 {
		t.Fatalf("expected at least 2 progress events, got %v", rec.events)
	}
	last := rec.events[len(rec.events)-1]
	if last != ":completed" {
		t.Fatalf("last event = %s want ':completed'", last)
	}
}

func TestEngineMaxNodeExecsGuard(t *testing.T) {
	store := newMemStore()
	engine := NewEngine(Config{Store: store, MaxNodeExecs: 2})
	// Two-node cycle would normally re-visit, but engine's visited set kills
	// loops. Construct a long fan-out instead so we hit the limit.
	nodes := []FlowNode{{ID: "trigger", Type: NodeTypeTrigger}}
	edges := []FlowEdge{}
	for i := 0; i < 5; i++ {
		id := uuidLike(i)
		nodes = append(nodes, FlowNode{ID: id, Type: NodeTypeNote})
		if i == 0 {
			edges = append(edges, FlowEdge{ID: "et", Source: "trigger", Target: id})
		} else {
			edges = append(edges, FlowEdge{ID: "e" + id, Source: uuidLike(i - 1), Target: id})
		}
	}
	flow := newSimpleFlow(nodes, edges)

	exec, err := engine.ExecuteFlow(flow, TriggerManual, nil, nil)
	if err != nil {
		t.Fatalf("ExecuteFlow: %v", err)
	}
	final := waitForExecution(t, store, exec.ID, 2*time.Second)
	if final.Status != ExecutionFailed {
		t.Fatalf("expected failed (max nodes), got %s", final.Status)
	}
}

func uuidLike(i int) string {
	return "n" + string(rune('a'+i))
}
