package flow

import (
	"context"
	"sort"
	"sync"
)

// NodeExecutor is the runtime contract every node type implements. Apps plug
// their domain nodes (Message, AIChat, CreateTicket, …) into the engine by
// implementing this interface and calling Engine.RegisterNode.
//
// Execute receives the FlowNode being evaluated and the live ExecutionContext.
// The returned NodeResult drives the engine's traversal decision:
//   - Output      → variables to merge into the context
//   - OutputHandle  → name of the single handle to follow (branching nodes)
//   - OutputHandles → multiple handles to follow (split nodes)
//   - Stop        → halt execution without marking the run as failed
//
// Name is informational; the engine looks nodes up by NodeType through the
// Registry, not by Name.
type NodeExecutor interface {
	Execute(ctx context.Context, node *FlowNode, execCtx *ExecutionContext) (*NodeResult, error)
	Name() string
}

// NodeResult is what executors return after running.
type NodeResult struct {
	// Output variables produced by this node. Each key is stored under
	// "{nodeID}.{key}" and, when the node declares Data.OutputVariables, also
	// under the declared variable name at top-level.
	Output map[string]interface{}

	// OutputHandle selects a single outgoing edge by its SourceHandle (used by
	// Condition for "true"/"false", by Switch for "case-0".."case-n").
	OutputHandle string

	// OutputHandles selects multiple outgoing edges (used by Split). When set,
	// OutputHandle is ignored.
	OutputHandles []string

	// Stop halts the engine without marking the execution as failed; useful
	// for error_handler(stop) and similar short-circuiting nodes.
	Stop bool
}

// Registry is a thread-safe map of NodeType → NodeExecutor. Engines own a
// Registry; apps call Engine.RegisterNode / Engine.Registry() to populate it.
type Registry struct {
	mu        sync.RWMutex
	executors map[NodeType]NodeExecutor
}

// NewRegistry returns an empty registry. Engines pre-populate it with the
// kernel built-ins in NewEngine.
func NewRegistry() *Registry {
	return &Registry{executors: make(map[NodeType]NodeExecutor)}
}

// Register installs executor for nodeType. It is an intentional overwrite:
// apps may replace kernel built-ins (for example, override NodeTypeDelay with
// a distributed-scheduler version) by calling Register again.
func (r *Registry) Register(nodeType NodeType, executor NodeExecutor) {
	if executor == nil {
		return
	}
	r.mu.Lock()
	r.executors[nodeType] = executor
	r.mu.Unlock()
}

// Unregister drops the executor for nodeType. Missing entries are a no-op.
func (r *Registry) Unregister(nodeType NodeType) {
	r.mu.Lock()
	delete(r.executors, nodeType)
	r.mu.Unlock()
}

// Get returns the executor registered for nodeType, or false.
func (r *Registry) Get(nodeType NodeType) (NodeExecutor, bool) {
	r.mu.RLock()
	e, ok := r.executors[nodeType]
	r.mu.RUnlock()
	return e, ok
}

// Types lists every registered node type, sorted alphabetically. Useful for
// debug endpoints or admin UIs.
func (r *Registry) Types() []NodeType {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]NodeType, 0, len(r.executors))
	for t := range r.executors {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Len returns the number of registered node types.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.executors)
}
