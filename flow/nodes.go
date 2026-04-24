package flow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// registerBuiltins is invoked by NewEngine. It installs the domain-free node
// executors the kernel ships with. Apps can replace any of these by calling
// Engine.RegisterNode with the same NodeType.
func registerBuiltins(r *Registry) {
	r.Register(NodeTypeTrigger, &TriggerNode{})
	r.Register(NodeTypeHTTPRequest, &HTTPRequestNode{})
	r.Register(NodeTypeWebhook, &WebhookNode{})
	r.Register(NodeTypeCondition, &ConditionNode{})
	r.Register(NodeTypeSwitch, &SwitchNode{})
	r.Register(NodeTypeDelay, &DelayNode{})
	r.Register(NodeTypeWait, &DelayNode{}) // alias
	r.Register(NodeTypeLoop, &LoopNode{})
	r.Register(NodeTypeFilter, &FilterNode{})
	r.Register(NodeTypeSetVariable, &SetVariableNode{})
	r.Register(NodeTypeTransformData, &TransformDataNode{})
	r.Register(NodeTypeSplit, &SplitNode{})
	r.Register(NodeTypeMerge, &MergeNode{})
	r.Register(NodeTypeErrorHandler, &ErrorHandlerNode{})
	r.Register(NodeTypeNote, &NoteNode{})
}

// ─── helpers ───────────────────────────────────────────────────────────────

func configMap(node *FlowNode) map[string]interface{} {
	if node.Data.Config != nil {
		return node.Data.Config
	}
	return node.Data.TriggerConfig
}

// ConfigString is exported so app-side node executors can reuse the same
// coercion rules the built-ins use.
func ConfigString(config map[string]interface{}, key string) string {
	if config == nil {
		return ""
	}
	if v, ok := config[key]; ok {
		if v == nil {
			return ""
		}
		return fmt.Sprintf("%v", v)
	}
	return ""
}

// ConfigFloat is the float64 counterpart of ConfigString. Strings are parsed
// best-effort; unparsable values return 0.
func ConfigFloat(config map[string]interface{}, key string) float64 {
	if config == nil {
		return 0
	}
	v, ok := config[key]
	if !ok {
		return 0
	}
	switch val := v.(type) {
	case float64:
		return val
	case float32:
		return float64(val)
	case int:
		return float64(val)
	case int64:
		return float64(val)
	case string:
		var f float64
		_, _ = fmt.Sscanf(val, "%f", &f)
		return f
	default:
		return 0
	}
}

// ─── Trigger ───────────────────────────────────────────────────────────────

// TriggerNode is a pass-through entry point. The engine always executes it
// first; its output is just TriggerData so downstream nodes can reference
// {{$trigger.data.foo}}.
type TriggerNode struct{}

func (TriggerNode) Name() string { return "trigger" }
func (TriggerNode) Execute(_ context.Context, _ *FlowNode, ec *ExecutionContext) (*NodeResult, error) {
	return &NodeResult{Output: ec.TriggerData}, nil
}

// ─── HTTPRequest ───────────────────────────────────────────────────────────

// HTTPRequestNode issues an outbound HTTP request with template-expanded URL,
// headers, and body. Response body is truncated to 1 MiB to bound memory.
type HTTPRequestNode struct{}

func (HTTPRequestNode) Name() string { return "http_request" }
func (HTTPRequestNode) Execute(ctx context.Context, node *FlowNode, ec *ExecutionContext) (*NodeResult, error) {
	cfg := configMap(node)

	method := strings.ToUpper(ConfigString(cfg, "method"))
	if method == "" {
		method = "GET"
	}
	url := ec.Interpolate(ConfigString(cfg, "url"))
	if url == "" {
		return nil, fmt.Errorf("http_request: url is required")
	}

	var body io.Reader
	if bodyStr := ConfigString(cfg, "body"); bodyStr != "" {
		body = bytes.NewBufferString(ec.Interpolate(bodyStr))
	}

	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, fmt.Errorf("http_request: %w", err)
	}

	if h, ok := cfg["headers"].(map[string]interface{}); ok {
		for k, v := range h {
			req.Header.Set(k, ec.Interpolate(fmt.Sprintf("%v", v)))
		}
	}
	if req.Header.Get("Content-Type") == "" && body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http_request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	output := map[string]interface{}{
		"status":      resp.StatusCode,
		"status_text": resp.Status,
		"body":        string(respBody),
	}

	var parsed interface{}
	if json.Unmarshal(respBody, &parsed) == nil {
		output["json"] = parsed
		ec.Set("$response", parsed)
	}
	ec.Set("$http_status", resp.StatusCode)
	ec.Set("$http_body", string(respBody))

	return &NodeResult{Output: output}, nil
}

// WebhookNode is an outbound webhook — HTTP POST with JSON by default.
type WebhookNode struct{}

func (WebhookNode) Name() string { return "webhook" }
func (w WebhookNode) Execute(ctx context.Context, node *FlowNode, ec *ExecutionContext) (*NodeResult, error) {
	if node.Data.Config == nil {
		node.Data.Config = make(map[string]interface{})
	}
	if _, ok := node.Data.Config["method"]; !ok {
		node.Data.Config["method"] = "POST"
	}
	return HTTPRequestNode{}.Execute(ctx, node, ec)
}

// ─── Condition ─────────────────────────────────────────────────────────────

// ConditionNode evaluates (field operator value) and routes to the "true" or
// "false" source handle. When no `field` is set but `expression` is, the
// expression is treated as a template and checked is_not_empty.
type ConditionNode struct{}

func (ConditionNode) Name() string { return "condition" }
func (ConditionNode) Execute(_ context.Context, node *FlowNode, ec *ExecutionContext) (*NodeResult, error) {
	cfg := configMap(node)
	field := ConfigString(cfg, "field")
	operator := ConfigString(cfg, "operator")
	var value interface{}
	if cfg != nil {
		value = cfg["value"]
	}

	if field == "" {
		if expr := ConfigString(cfg, "expression"); expr != "" {
			field = expr
			operator = "is_not_empty"
		}
	}

	actual, _ := ec.Get(field)
	if actual == nil {
		if interpolated := ec.Interpolate(field); interpolated != field {
			actual = interpolated
		}
	}

	matched := CompareValues(actual, operator, value)
	handle := "false"
	if matched {
		handle = "true"
	}

	return &NodeResult{
		Output: map[string]interface{}{
			"result": matched,
			"field":  field,
			"value":  actual,
		},
		OutputHandle: handle,
	}, nil
}

// ─── Switch ────────────────────────────────────────────────────────────────

// SwitchNode scans a list of cases and emits the matching handle (case-i) or
// "default" when none matches.
type SwitchNode struct{}

func (SwitchNode) Name() string { return "switch" }
func (SwitchNode) Execute(_ context.Context, node *FlowNode, ec *ExecutionContext) (*NodeResult, error) {
	cfg := configMap(node)
	field := ConfigString(cfg, "field")
	actual := ec.GetString(field)

	cases, _ := cfg["cases"].([]interface{})
	handle := "default"
	for i, c := range cases {
		cm, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if strings.EqualFold(actual, fmt.Sprintf("%v", cm["value"])) {
			handle = fmt.Sprintf("case-%d", i)
			break
		}
	}
	return &NodeResult{
		Output: map[string]interface{}{
			"matched": handle,
			"value":   actual,
		},
		OutputHandle: handle,
	}, nil
}

// ─── Delay / Wait ──────────────────────────────────────────────────────────

// DelayNode sleeps for `seconds` (or `delay`). Capped at 300 seconds to
// prevent runaway flows; for longer waits use an app-supplied scheduler.
type DelayNode struct{}

func (DelayNode) Name() string { return "delay" }
func (DelayNode) Execute(ctx context.Context, node *FlowNode, _ *ExecutionContext) (*NodeResult, error) {
	cfg := configMap(node)
	seconds := ConfigFloat(cfg, "seconds")
	if seconds == 0 {
		seconds = ConfigFloat(cfg, "delay")
	}
	if seconds == 0 {
		seconds = 1
	}
	if seconds > 300 {
		seconds = 300
	}
	select {
	case <-time.After(time.Duration(seconds * float64(time.Second))):
	case <-ctx.Done():
		return &NodeResult{Stop: true}, ctx.Err()
	}
	return &NodeResult{Output: map[string]interface{}{"delayed_seconds": seconds}}, nil
}

// ─── SetVariable ───────────────────────────────────────────────────────────

// SetVariableNode writes a named variable into the context. String values
// are template-expanded.
type SetVariableNode struct{}

func (SetVariableNode) Name() string { return "set_variable" }
func (SetVariableNode) Execute(_ context.Context, node *FlowNode, ec *ExecutionContext) (*NodeResult, error) {
	cfg := configMap(node)
	name := ConfigString(cfg, "name")
	if name == "" {
		name = ConfigString(cfg, "variable")
	}
	var value interface{}
	if cfg != nil {
		value = cfg["value"]
	}
	if s, ok := value.(string); ok {
		value = ec.Interpolate(s)
	}
	if name != "" {
		ec.Set(name, value)
	}
	return &NodeResult{Output: map[string]interface{}{"name": name, "value": value}}, nil
}

// ─── TransformData ─────────────────────────────────────────────────────────

// TransformDataNode applies a list of {source,target,transform} rules. Each
// transform is one of: uppercase, lowercase, trim.
type TransformDataNode struct{}

func (TransformDataNode) Name() string { return "transform_data" }
func (TransformDataNode) Execute(_ context.Context, node *FlowNode, ec *ExecutionContext) (*NodeResult, error) {
	cfg := configMap(node)
	if cfg == nil {
		return &NodeResult{}, nil
	}
	transforms, _ := cfg["transforms"].([]interface{})
	output := make(map[string]interface{})
	for _, t := range transforms {
		tr, ok := t.(map[string]interface{})
		if !ok {
			continue
		}
		source := ConfigString(tr, "source")
		target := ConfigString(tr, "target")
		transform := ConfigString(tr, "transform")
		val := ec.GetString(source)
		switch transform {
		case "uppercase":
			val = strings.ToUpper(val)
		case "lowercase":
			val = strings.ToLower(val)
		case "trim":
			val = strings.TrimSpace(val)
		}
		if target != "" {
			ec.Set(target, val)
			output[target] = val
		}
	}
	return &NodeResult{Output: output}, nil
}

// ─── Loop ──────────────────────────────────────────────────────────────────

// LoopNode exposes iteration metadata to downstream nodes. It does NOT
// re-execute subgraphs by itself; use it to seed `$loop.items` / `$loop.count`
// for condition-driven traversals.
type LoopNode struct{}

func (LoopNode) Name() string { return "loop" }
func (LoopNode) Execute(_ context.Context, node *FlowNode, ec *ExecutionContext) (*NodeResult, error) {
	cfg := configMap(node)
	if cfg == nil {
		return &NodeResult{}, nil
	}
	source := ConfigString(cfg, "source")
	itemVar := ConfigString(cfg, "itemVariable")
	if itemVar == "" {
		itemVar = "$item"
	}
	indexVar := ConfigString(cfg, "indexVariable")
	if indexVar == "" {
		indexVar = "$index"
	}

	val, _ := ec.Get(source)
	items, ok := val.([]interface{})
	if !ok {
		return &NodeResult{Output: map[string]interface{}{"count": 0, "finished": true}}, nil
	}

	ec.Set("$loop.count", len(items))
	ec.Set("$loop.items", items)

	for i, item := range items {
		ec.Set(itemVar, item)
		ec.Set(indexVar, i)
	}
	if len(items) > 0 {
		ec.Set(itemVar, items[len(items)-1])
		ec.Set(indexVar, len(items)-1)
	}

	return &NodeResult{Output: map[string]interface{}{"count": len(items), "finished": true}}, nil
}

// ─── Filter ────────────────────────────────────────────────────────────────

// FilterNode filters []interface{} items by (field operator value). Items
// that are not map[string]interface{} are dropped.
type FilterNode struct{}

func (FilterNode) Name() string { return "filter" }
func (FilterNode) Execute(_ context.Context, node *FlowNode, ec *ExecutionContext) (*NodeResult, error) {
	cfg := configMap(node)
	if cfg == nil {
		return &NodeResult{}, nil
	}
	source := ConfigString(cfg, "source")
	field := ConfigString(cfg, "field")
	operator := ConfigString(cfg, "operator")
	var value interface{}
	if cfg != nil {
		value = cfg["value"]
	}

	val, _ := ec.Get(source)
	items, ok := val.([]interface{})
	if !ok {
		return &NodeResult{Output: map[string]interface{}{"filtered": []interface{}{}, "count": 0}}, nil
	}

	var filtered []interface{}
	for _, item := range items {
		if m, ok := item.(map[string]interface{}); ok {
			if CompareValues(m[field], operator, value) {
				filtered = append(filtered, item)
			}
		}
	}
	ec.Set("$filtered", filtered)
	return &NodeResult{Output: map[string]interface{}{"filtered": filtered, "count": len(filtered)}}, nil
}

// ─── Split ─────────────────────────────────────────────────────────────────

// SplitNode follows all outgoing edges in parallel (in BFS semantics).
type SplitNode struct{}

func (SplitNode) Name() string { return "split" }
func (SplitNode) Execute(_ context.Context, _ *FlowNode, _ *ExecutionContext) (*NodeResult, error) {
	return &NodeResult{
		Output:        map[string]interface{}{"split": true},
		OutputHandles: []string{"output-0", "output-1"},
	}, nil
}

// ─── Merge ─────────────────────────────────────────────────────────────────

// MergeNode is a no-op marker used by editors to join parallel paths; the
// engine already handles graph convergence via the visited set.
type MergeNode struct{}

func (MergeNode) Name() string { return "merge" }
func (MergeNode) Execute(_ context.Context, _ *FlowNode, _ *ExecutionContext) (*NodeResult, error) {
	return &NodeResult{Output: map[string]interface{}{"merged": true}}, nil
}

// ─── ErrorHandler ──────────────────────────────────────────────────────────

// ErrorHandlerNode supports three actions: stop (halt execution), retry
// (no-op result so the engine will traverse onwards), continue (default).
type ErrorHandlerNode struct{}

func (ErrorHandlerNode) Name() string { return "error_handler" }
func (ErrorHandlerNode) Execute(_ context.Context, node *FlowNode, _ *ExecutionContext) (*NodeResult, error) {
	cfg := configMap(node)
	if cfg == nil {
		return &NodeResult{}, nil
	}
	switch ConfigString(cfg, "action") {
	case "stop":
		return &NodeResult{Stop: true, Output: map[string]interface{}{"action": "stop"}}, nil
	case "retry":
		return &NodeResult{Output: map[string]interface{}{"action": "retry"}}, nil
	default:
		return &NodeResult{Output: map[string]interface{}{"action": "continue"}}, nil
	}
}

// ─── Note ──────────────────────────────────────────────────────────────────

// NoteNode is a visual annotation — it does nothing at runtime.
type NoteNode struct{}

func (NoteNode) Name() string                                                        { return "note" }
func (NoteNode) Execute(_ context.Context, _ *FlowNode, _ *ExecutionContext) (*NodeResult, error) {
	return &NodeResult{}, nil
}
