package flow

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// staticLoader returns the same list every time, ignoring filters.
type staticLoader struct{ flows []*Flow }

func (s staticLoader) LoadFlows(_ context.Context, _ uuid.UUID, _ TriggerType) ([]*Flow, error) {
	return s.flows, nil
}

func TestTriggerServiceManualPassthrough(t *testing.T) {
	store := newMemStore()
	engine := NewEngine(Config{Store: store})

	flow := newSimpleFlow(
		[]FlowNode{
			{ID: "trigger", Type: NodeTypeTrigger},
			{ID: "set", Type: NodeTypeSetVariable, Data: FlowNodeData{Config: map[string]interface{}{
				"name": "out", "value": "{{$trigger.data.k}}",
			}}},
		},
		[]FlowEdge{{ID: "e1", Source: "trigger", Target: "set"}},
	)
	flow.TriggerType = TriggerManual

	loader := staticLoader{flows: []*Flow{flow}}
	svc := NewTriggerService(engine, loader)

	exec, err := svc.Dispatch(context.Background(), TriggerEvent{
		OrganizationID: flow.OrganizationID,
		Type:           TriggerManual,
		Payload:        map[string]interface{}{"k": "v"},
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if exec == nil {
		t.Fatalf("Dispatch returned nil execution")
	}
	final := waitForExecution(t, store, exec.ID, 2*time.Second)
	if final.Status != ExecutionCompleted {
		t.Fatalf("status = %s err=%s", final.Status, final.ErrorMessage)
	}
	if final.Variables["out"] != "v" {
		t.Fatalf("out = %v want v", final.Variables["out"])
	}
}

func TestTriggerServiceCustomMatcher(t *testing.T) {
	engine := NewEngine(Config{Store: newMemStore()})

	keywordType := TriggerType("keyword")
	flow := newSimpleFlow(
		[]FlowNode{
			{ID: "trigger", Type: NodeTypeTrigger},
		},
		nil,
	)
	flow.TriggerType = keywordType
	flow.TriggerConfig = map[string]interface{}{"keyword": "hello"}

	loader := staticLoader{flows: []*Flow{flow}}
	svc := NewTriggerService(engine, loader)

	svc.RegisterMatcher(keywordType, TriggerMatcherFunc(func(_ context.Context, f *Flow, e TriggerEvent) (map[string]interface{}, bool) {
		kw, _ := f.TriggerConfig["keyword"].(string)
		msg, _ := e.Payload["message"].(string)
		if kw == "" || msg == "" {
			return nil, false
		}
		if msg == kw {
			return map[string]interface{}{"matched_keyword": kw}, true
		}
		return nil, false
	}))

	// Non-matching event → no execution.
	exec, err := svc.Dispatch(context.Background(), TriggerEvent{
		OrganizationID: flow.OrganizationID,
		Type:           keywordType,
		Payload:        map[string]interface{}{"message": "bye"},
	})
	if err != nil {
		t.Fatalf("Dispatch (non-match): %v", err)
	}
	if exec != nil {
		t.Fatalf("expected nil exec for non-match, got %v", exec)
	}

	// Matching event → execution.
	exec, err = svc.Dispatch(context.Background(), TriggerEvent{
		OrganizationID: flow.OrganizationID,
		Type:           keywordType,
		Payload:        map[string]interface{}{"message": "hello"},
	})
	if err != nil {
		t.Fatalf("Dispatch (match): %v", err)
	}
	if exec == nil {
		t.Fatalf("expected non-nil exec for match")
	}
}
