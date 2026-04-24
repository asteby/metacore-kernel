package flow

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ExecutionContext holds runtime state during a single flow execution. It is
// created by NewExecutionContext, mutated by the engine and by NodeExecutors
// through Set/Get, and finally frozen into FlowExecution.Variables when the
// run finishes.
//
// ExecutionContext is NOT safe for concurrent mutation. The engine guarantees
// single-writer semantics by executing nodes serially in BFS order; if a
// custom executor spawns goroutines, it must serialize access itself.
type ExecutionContext struct {
	ExecutionID    uuid.UUID
	FlowID         uuid.UUID
	OrganizationID uuid.UUID

	TriggerType TriggerType
	TriggerData map[string]interface{}

	// AppContext is an opaque map that hosts use to thread app-specific state
	// through the engine (e.g. contact ID, conversation ID, device ID in link).
	// The engine never reads or writes AppContext — only app-provided node
	// executors do. It is copied verbatim into FlowExecution.AppContext.
	AppContext map[string]interface{}

	Variables map[string]interface{}
	NodeLogs  []NodeExecution

	CurrentNodeID string
	Flow          *Flow
	StartedAt     time.Time
}

// NewExecutionContext seeds a fresh context from a Flow + trigger payload. It
// also populates a handful of system variables (`$now`, `$today`,
// `$trigger.data`, `$execution.id`, `$workflow.name`, `$workflow.id`) that
// templates can reference.
func NewExecutionContext(flow *Flow, triggerType TriggerType, triggerData map[string]interface{}) *ExecutionContext {
	ctx := &ExecutionContext{
		ExecutionID:    uuid.New(),
		FlowID:         flow.ID,
		OrganizationID: flow.OrganizationID,
		TriggerType:    triggerType,
		TriggerData:    triggerData,
		AppContext:     make(map[string]interface{}),
		Variables:      make(map[string]interface{}),
		NodeLogs:       make([]NodeExecution, 0),
		Flow:           flow,
		StartedAt:      time.Now(),
	}

	now := time.Now().UTC()
	ctx.Variables["$now"] = now.Format(time.RFC3339)
	ctx.Variables["$today"] = now.Format("2006-01-02")
	ctx.Variables["$trigger.data"] = triggerData
	ctx.Variables["$trigger.type"] = string(triggerType)
	ctx.Variables["$execution.id"] = ctx.ExecutionID.String()
	ctx.Variables["$workflow.name"] = flow.Name
	ctx.Variables["$workflow.id"] = flow.ID.String()

	for _, v := range flow.Variables {
		if v.Value != nil {
			ctx.Variables[v.Name] = v.Value
		}
	}
	return ctx
}

// SetAppContext merges entries into the AppContext map. Convenience for hosts
// that want to attach contact/conversation/device IDs to the run.
func (c *ExecutionContext) SetAppContext(entries map[string]interface{}) {
	if c.AppContext == nil {
		c.AppContext = make(map[string]interface{}, len(entries))
	}
	for k, v := range entries {
		c.AppContext[k] = v
	}
}

// Set stores a variable under key. Dot-notation is not auto-expanded on write
// — the caller writes the literal key.
func (c *ExecutionContext) Set(key string, value interface{}) {
	c.Variables[key] = value
}

// Get retrieves a variable, falling back to dot-notation traversal when the
// literal key misses. Dotted lookups try the longest prefix first so keys
// containing dots (e.g. "$trigger.data") still resolve before drilling into
// nested map values.
func (c *ExecutionContext) Get(key string) (interface{}, bool) {
	if v, ok := c.Variables[key]; ok {
		return v, true
	}
	// Try every "{prefix}.{rest}" split, longest prefix first.
	for i := strings.LastIndex(key, "."); i > 0; i = strings.LastIndex(key[:i], ".") {
		prefix := key[:i]
		rest := key[i+1:]
		parent, ok := c.Variables[prefix]
		if !ok {
			continue
		}
		if v, ok := traverse(parent, rest); ok {
			return v, true
		}
	}
	return nil, false
}

// traverse walks a dotted path against a nested map[string]interface{}. It
// returns false on any miss or unexpected type.
func traverse(root interface{}, path string) (interface{}, bool) {
	cur := root
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return nil, false
		}
		cur, ok = m[part]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// GetString returns Get(key) coerced to string, or "" if missing.
func (c *ExecutionContext) GetString(key string) string {
	v, ok := c.Get(key)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%v", v)
}

var templateRegex = regexp.MustCompile(`\{\{([^}]+)\}\}`)

// Interpolate substitutes `{{var}}` placeholders in the template with values
// from the context. Function suffixes are supported: `{{name.uppercase()}}`,
// `{{text.trim()}}`, etc. Unresolved placeholders are left intact so the
// caller can detect them.
func (c *ExecutionContext) Interpolate(template string) string {
	return templateRegex.ReplaceAllStringFunc(template, func(match string) string {
		varExpr := strings.TrimSpace(match[2 : len(match)-2])

		if idx := strings.Index(varExpr, "."); idx > 0 {
			lastPart := varExpr[strings.LastIndex(varExpr, ".")+1:]
			if strings.HasSuffix(lastPart, "()") {
				funcName := strings.TrimSuffix(lastPart, "()")
				varName := varExpr[:strings.LastIndex(varExpr, ".")]
				val := c.GetString(varName)
				return applyFunction(val, funcName)
			}
		}

		val, ok := c.Get(varExpr)
		if !ok {
			return match
		}
		return fmt.Sprintf("%v", val)
	})
}

// InterpolateMap recursively interpolates all string leaves of m.
func (c *ExecutionContext) InterpolateMap(m map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		switch val := v.(type) {
		case string:
			out[k] = c.Interpolate(val)
		case map[string]interface{}:
			out[k] = c.InterpolateMap(val)
		default:
			out[k] = v
		}
	}
	return out
}

func applyFunction(val, name string) string {
	switch name {
	case "uppercase":
		return strings.ToUpper(val)
	case "lowercase":
		return strings.ToLower(val)
	case "trim":
		return strings.TrimSpace(val)
	case "length":
		return strconv.Itoa(len(val))
	default:
		return val
	}
}

// FindNode returns the node with id nodeID in the flow, or nil.
func (c *ExecutionContext) FindNode(nodeID string) *FlowNode {
	for i := range c.Flow.Nodes {
		if c.Flow.Nodes[i].ID == nodeID {
			return &c.Flow.Nodes[i]
		}
	}
	return nil
}

// FindTriggerNode returns the first node of type NodeTypeTrigger; flows
// without one cannot run (ExecuteFlow fails with "no trigger node found").
func (c *ExecutionContext) FindTriggerNode() *FlowNode {
	for i := range c.Flow.Nodes {
		if c.Flow.Nodes[i].Type == NodeTypeTrigger {
			return &c.Flow.Nodes[i]
		}
	}
	return nil
}

// FindOutgoingEdges returns every edge leaving nodeID (any source handle).
func (c *ExecutionContext) FindOutgoingEdges(nodeID string) []FlowEdge {
	var edges []FlowEdge
	for _, e := range c.Flow.Edges {
		if e.Source == nodeID {
			edges = append(edges, e)
		}
	}
	return edges
}

// FindOutgoingEdgesForHandle filters by source handle — used by branching
// nodes to follow only the chosen output (e.g. "true", "false", "case-2").
func (c *ExecutionContext) FindOutgoingEdgesForHandle(nodeID, handle string) []FlowEdge {
	var edges []FlowEdge
	for _, e := range c.Flow.Edges {
		if e.Source == nodeID && e.SourceHandle == handle {
			edges = append(edges, e)
		}
	}
	return edges
}

// EvaluateCondition returns true when cond is nil (unconditional edge) or
// when the comparison succeeds against the current Variables.
func (c *ExecutionContext) EvaluateCondition(cond *EdgeCondition) bool {
	if cond == nil {
		return true
	}
	val, _ := c.Get(cond.Field)
	return CompareValues(val, cond.Operator, cond.Value)
}

// CompareValues is exported because a handful of built-in executors (filter,
// condition) reuse the same comparison logic. It always coerces both sides to
// string first; numeric operators additionally parse as float.
func CompareValues(actual interface{}, operator string, expected interface{}) bool {
	actualStr := fmt.Sprintf("%v", actual)
	expectedStr := fmt.Sprintf("%v", expected)

	switch operator {
	case "eq", "==", "equals":
		return actualStr == expectedStr
	case "ne", "!=", "not_equals":
		return actualStr != expectedStr
	case "contains":
		return strings.Contains(strings.ToLower(actualStr), strings.ToLower(expectedStr))
	case "not_contains":
		return !strings.Contains(strings.ToLower(actualStr), strings.ToLower(expectedStr))
	case "starts_with":
		return strings.HasPrefix(strings.ToLower(actualStr), strings.ToLower(expectedStr))
	case "ends_with":
		return strings.HasSuffix(strings.ToLower(actualStr), strings.ToLower(expectedStr))
	case "gt", ">":
		a, _ := strconv.ParseFloat(actualStr, 64)
		b, _ := strconv.ParseFloat(expectedStr, 64)
		return a > b
	case "gte", ">=":
		a, _ := strconv.ParseFloat(actualStr, 64)
		b, _ := strconv.ParseFloat(expectedStr, 64)
		return a >= b
	case "lt", "<":
		a, _ := strconv.ParseFloat(actualStr, 64)
		b, _ := strconv.ParseFloat(expectedStr, 64)
		return a < b
	case "lte", "<=":
		a, _ := strconv.ParseFloat(actualStr, 64)
		b, _ := strconv.ParseFloat(expectedStr, 64)
		return a <= b
	case "is_empty":
		return actualStr == "" || actualStr == "<nil>"
	case "is_not_empty":
		return actualStr != "" && actualStr != "<nil>"
	case "matches":
		re, err := regexp.Compile(expectedStr)
		if err != nil {
			return false
		}
		return re.MatchString(actualStr)
	default:
		return actualStr == expectedStr
	}
}
