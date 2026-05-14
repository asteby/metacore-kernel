package flow

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
)

// inMemSource is a FlowSource backed by a map. Tests preload it with the
// flows they want exposed and look up by ID.
type inMemSource struct {
	flows map[uuid.UUID]*Flow
}

func (s *inMemSource) GetFlow(_ context.Context, _ uuid.UUID, id uuid.UUID) (*Flow, error) {
	if f, ok := s.flows[id]; ok {
		return f, nil
	}
	return nil, nil
}

func newTestHandlerApp(engine *Engine, source FlowSource) *fiber.App {
	app := fiber.New()
	h := NewHandler(HandlerConfig{
		Engine:     engine,
		FlowSource: source,
	})
	h.Mount(app.Group("/flows"))
	return app
}

func TestHandler_TestEndpointInline(t *testing.T) {
	engine := NewEngine(Config{})

	flow := newSimpleFlow(
		[]FlowNode{
			{ID: "trigger", Type: NodeTypeTrigger},
			{ID: "set", Type: NodeTypeSetVariable, Data: FlowNodeData{Config: map[string]interface{}{
				"name": "out", "value": "hi {{$trigger.data.who}}",
			}}},
		},
		[]FlowEdge{{ID: "e1", Source: "trigger", Target: "set"}},
	)

	source := &inMemSource{flows: map[uuid.UUID]*Flow{flow.ID: flow}}
	app := newTestHandlerApp(engine, source)

	body, _ := json.Marshal(map[string]interface{}{
		"trigger_data": map[string]interface{}{"who": "ada"},
	})
	req := httptest.NewRequest("POST", "/flows/"+flow.ID.String()+"/test", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d body=%s", resp.StatusCode, string(raw))
	}

	var envelope struct {
		Success bool       `json:"success"`
		Data    TestResult `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !envelope.Success {
		t.Fatalf("success=false")
	}
	if envelope.Data.Status != "completed" {
		t.Fatalf("status=%s err=%s", envelope.Data.Status, envelope.Data.Error)
	}
	if envelope.Data.Variables["out"] != "hi ada" {
		t.Fatalf("out=%v want 'hi ada'", envelope.Data.Variables["out"])
	}
}

func TestHandler_RunEndpointAsync(t *testing.T) {
	store := newMemStore()
	engine := NewEngine(Config{Store: store})

	flow := newSimpleFlow(
		[]FlowNode{{ID: "trigger", Type: NodeTypeTrigger}},
		nil,
	)
	source := &inMemSource{flows: map[uuid.UUID]*Flow{flow.ID: flow}}
	app := newTestHandlerApp(engine, source)

	req := httptest.NewRequest("POST", "/flows/"+flow.ID.String()+"/run", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d body=%s", resp.StatusCode, string(raw))
	}

	var envelope struct {
		Success bool `json:"success"`
		Data    struct {
			ExecutionID string `json:"execution_id"`
			Status      string `json:"status"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !envelope.Success {
		t.Fatalf("success=false")
	}
	if envelope.Data.ExecutionID == "" {
		t.Fatalf("execution_id empty")
	}
	if envelope.Data.Status != string(ExecutionRunning) {
		t.Fatalf("status=%s want running", envelope.Data.Status)
	}
}

func TestHandler_RunFlowNotFound(t *testing.T) {
	engine := NewEngine(Config{})
	source := &inMemSource{flows: map[uuid.UUID]*Flow{}}
	app := newTestHandlerApp(engine, source)

	req := httptest.NewRequest("POST", "/flows/"+uuid.New().String()+"/run", nil)
	resp, _ := app.Test(req)
	if resp.StatusCode != 404 {
		t.Fatalf("status=%d want 404", resp.StatusCode)
	}
}

func TestHandler_RunInvalidUUID(t *testing.T) {
	engine := NewEngine(Config{})
	app := newTestHandlerApp(engine, &inMemSource{flows: map[uuid.UUID]*Flow{}})

	req := httptest.NewRequest("POST", "/flows/not-a-uuid/run", nil)
	resp, _ := app.Test(req)
	if resp.StatusCode != 400 {
		t.Fatalf("status=%d want 400", resp.StatusCode)
	}
}

func TestNewHandler_NilDeps(t *testing.T) {
	if h := NewHandler(HandlerConfig{}); h != nil {
		t.Fatalf("expected nil handler when deps missing")
	}
	if h := NewHandler(HandlerConfig{Engine: NewEngine(Config{})}); h != nil {
		t.Fatalf("expected nil handler without FlowSource")
	}
}
