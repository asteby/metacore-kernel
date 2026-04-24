package flow

import (
	"context"
	"sync"

	"github.com/google/uuid"
)

// TriggerMatcher decides whether a flow should run for a given event. The
// matcher is stateless; hosts construct one per TriggerType they support.
// Return (triggerData, true) to execute, or (_, false) to skip. triggerData
// is whatever the app wants visible under `{{$trigger.data.*}}`.
type TriggerMatcher interface {
	Match(ctx context.Context, flow *Flow, event TriggerEvent) (triggerData map[string]interface{}, matched bool)
}

// TriggerMatcherFunc is an adapter that lets a plain func implement
// TriggerMatcher.
type TriggerMatcherFunc func(ctx context.Context, flow *Flow, event TriggerEvent) (map[string]interface{}, bool)

// Match satisfies TriggerMatcher.
func (f TriggerMatcherFunc) Match(ctx context.Context, flow *Flow, event TriggerEvent) (map[string]interface{}, bool) {
	return f(ctx, flow, event)
}

// TriggerEvent is the generic envelope hosts feed into TriggerService.
// Payload is the raw event body (e.g. inbound HTTP JSON, scheduled tick,
// incoming message); AppContext is merged into the resulting ExecutionContext.
type TriggerEvent struct {
	OrganizationID uuid.UUID
	Type           TriggerType
	Payload        map[string]interface{}
	AppContext     map[string]interface{}
}

// FlowLoader returns the candidate flows for a given org + trigger type. Hosts
// implement this against their own persistence layer (GORM, in-memory, etc.).
type FlowLoader interface {
	LoadFlows(ctx context.Context, orgID uuid.UUID, triggerType TriggerType) ([]*Flow, error)
}

// TriggerService is a small coordinator that glues together trigger events,
// per-type matchers, and the engine. It is optional — apps can bypass it and
// call Engine.ExecuteFlow directly when they don't need declarative matching.
//
// Typical wiring:
//
//	engine := flow.NewEngine(cfg)
//	svc    := flow.NewTriggerService(engine, loader)
//	svc.RegisterMatcher(flow.TriggerManual, flow.ManualMatcher{})
//	svc.RegisterMatcher("keyword", myApp.KeywordMatcher{})
//	// inbound loop:
//	svc.Dispatch(ctx, flow.TriggerEvent{ ... })
type TriggerService struct {
	engine   *Engine
	loader   FlowLoader
	mu       sync.RWMutex
	matchers map[TriggerType]TriggerMatcher
}

// NewTriggerService builds a trigger coordinator. Both engine and loader are
// required.
func NewTriggerService(engine *Engine, loader FlowLoader) *TriggerService {
	svc := &TriggerService{
		engine:   engine,
		loader:   loader,
		matchers: make(map[TriggerType]TriggerMatcher),
	}
	// Built-in matchers — manual / webhook / api fire unconditionally when
	// the flow's TriggerType matches. Apps override by re-registering.
	passthrough := TriggerMatcherFunc(func(_ context.Context, _ *Flow, e TriggerEvent) (map[string]interface{}, bool) {
		return e.Payload, true
	})
	svc.RegisterMatcher(TriggerManual, passthrough)
	svc.RegisterMatcher(TriggerWebhook, passthrough)
	svc.RegisterMatcher(TriggerAPI, passthrough)
	svc.RegisterMatcher(TriggerSchedule, passthrough)
	svc.RegisterMatcher(TriggerEventType, passthrough)
	return svc
}

// RegisterMatcher installs a matcher for the given trigger type. Calling
// RegisterMatcher again for the same type replaces the previous matcher.
func (t *TriggerService) RegisterMatcher(triggerType TriggerType, matcher TriggerMatcher) {
	t.mu.Lock()
	t.matchers[triggerType] = matcher
	t.mu.Unlock()
}

// Dispatch loads all active flows for the org + trigger type, runs the
// matcher for each, and executes the first flow that matches. Returns the
// FlowExecution when one was started, or nil when nothing matched.
//
// When multiple flows match, only the first (in loader-provided order) runs —
// this matches the single-dispatch semantics apps expect. Hosts that need
// fan-out should call DispatchAll.
func (t *TriggerService) Dispatch(ctx context.Context, event TriggerEvent) (*FlowExecution, error) {
	flows, err := t.loader.LoadFlows(ctx, event.OrganizationID, event.Type)
	if err != nil {
		return nil, err
	}
	t.mu.RLock()
	matcher := t.matchers[event.Type]
	t.mu.RUnlock()
	if matcher == nil {
		return nil, nil
	}
	for _, f := range flows {
		triggerData, ok := matcher.Match(ctx, f, event)
		if !ok {
			continue
		}
		return t.engine.ExecuteFlow(f, event.Type, triggerData, event.AppContext)
	}
	return nil, nil
}

// DispatchAll runs every matching flow (fan-out). Returns executions for
// every flow that started, plus any error from the loader.
func (t *TriggerService) DispatchAll(ctx context.Context, event TriggerEvent) ([]*FlowExecution, error) {
	flows, err := t.loader.LoadFlows(ctx, event.OrganizationID, event.Type)
	if err != nil {
		return nil, err
	}
	t.mu.RLock()
	matcher := t.matchers[event.Type]
	t.mu.RUnlock()
	if matcher == nil {
		return nil, nil
	}
	var executions []*FlowExecution
	for _, f := range flows {
		triggerData, ok := matcher.Match(ctx, f, event)
		if !ok {
			continue
		}
		exec, err := t.engine.ExecuteFlow(f, event.Type, triggerData, event.AppContext)
		if err != nil {
			return executions, err
		}
		executions = append(executions, exec)
	}
	return executions, nil
}
