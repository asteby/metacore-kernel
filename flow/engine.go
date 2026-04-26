package flow

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Store persists executions and reports run statistics. Apps that want
// executions saved to their own database implement Store and pass it via
// Config.Store; kernel never writes to a database on its own.
//
// All methods must be safe for concurrent use. The engine calls them from
// the goroutine that owns the execution (single writer per execution).
type Store interface {
	// SaveExecution is called once when an execution starts. The execution
	// has Status=Running, StartedAt set, CompletedAt nil.
	SaveExecution(ctx context.Context, execution *FlowExecution) error

	// UpdateExecution is called on completion / failure / cancellation with
	// the final snapshot (Status, CompletedAt, Variables, NodeExecutions,
	// ErrorMessage, ErrorNode populated as appropriate).
	UpdateExecution(ctx context.Context, execution *FlowExecution) error

	// RecordCompletion updates flow-level counters after a successful run
	// (execution_count++, success_count++, avg_execution_time). Implementations
	// are free to no-op if they don't track stats.
	RecordCompletion(ctx context.Context, flowID uuid.UUID, durationMs int64) error

	// RecordFailure updates flow-level counters after a failed run
	// (execution_count++, failure_count++).
	RecordFailure(ctx context.Context, flowID uuid.UUID) error
}

// ProgressSink receives per-node progress events so hosts can forward them to
// WebSocket, SSE, metrics, etc. nodeID is "" when the event is an
// execution-level transition (completed / failed).
type ProgressSink interface {
	OnProgress(orgID, execID uuid.UUID, flowID uuid.UUID, nodeID, status string)
}

// Logger is the minimal logging contract. Hosts can inject obs.Logger,
// log.Default(), or a no-op. nopLogger is used when Config.Logger is nil.
type Logger interface {
	Printf(format string, args ...interface{})
}

type nopLogger struct{}

func (nopLogger) Printf(string, ...interface{}) {}

// Config wires optional dependencies into the engine. Every field is
// optional; a zero Config (Config{}) produces a usable engine that keeps
// executions in memory only.
type Config struct {
	// Store persists executions. Leave nil to skip persistence (useful for
	// TestFlowInline or ephemeral engines).
	Store Store

	// Progress receives per-node / per-execution transitions. Leave nil to
	// silence progress events.
	Progress ProgressSink

	// Logger receives informational log messages. Leave nil for a no-op.
	Logger Logger

	// MaxNodeExecs caps how many nodes a single execution can run to catch
	// infinite loops; defaults to 200 when zero.
	MaxNodeExecs int

	// ExecutionTimeout bounds a full async run; defaults to 5 minutes.
	ExecutionTimeout time.Duration

	// TestTimeout bounds a TestFlowInline run; defaults to 2 minutes.
	TestTimeout time.Duration
}

// Engine drives DAG execution against a Registry of NodeExecutors. Engine is
// safe for concurrent use; individual executions do not share mutable state.
type Engine struct {
	cfg         Config
	registry    *Registry
	mu          sync.Mutex
	activeExecs map[uuid.UUID]context.CancelFunc
}

// NewEngine constructs an engine with the kernel built-in node executors
// pre-registered (HTTP, Webhook, Condition, Switch, Delay, Wait, Loop,
// Filter, Merge, Split, SetVariable, TransformData, ErrorHandler, Note,
// Trigger). Apps layer their own nodes on top via Engine.RegisterNode.
func NewEngine(cfg Config) *Engine {
	if cfg.MaxNodeExecs <= 0 {
		cfg.MaxNodeExecs = 200
	}
	if cfg.ExecutionTimeout <= 0 {
		cfg.ExecutionTimeout = 5 * time.Minute
	}
	if cfg.TestTimeout <= 0 {
		cfg.TestTimeout = 2 * time.Minute
	}
	if cfg.Logger == nil {
		cfg.Logger = nopLogger{}
	}
	e := &Engine{
		cfg:         cfg,
		registry:    NewRegistry(),
		activeExecs: make(map[uuid.UUID]context.CancelFunc),
	}
	registerBuiltins(e.registry)
	return e
}

// Registry exposes the underlying Registry so apps may register / introspect
// nodes outside of RegisterNode (e.g. bulk-register at startup).
func (e *Engine) Registry() *Registry { return e.registry }

// RegisterNode installs an app-provided executor. This is the primary hook
// apps use to extend the engine (e.g. registering Message, AIChat,
// CreateTicket nodes from a messaging host).
func (e *Engine) RegisterNode(nodeType NodeType, executor NodeExecutor) {
	e.registry.Register(nodeType, executor)
}

// ExecuteFlow starts an asynchronous run. The returned FlowExecution is a
// snapshot at submission time; callers observe completion through Store +
// ProgressSink. A context.DeadlineExceeded caused by ExecutionTimeout is
// translated into Status=Failed.
func (e *Engine) ExecuteFlow(flow *Flow, triggerType TriggerType, triggerData map[string]interface{}, appContext map[string]interface{}) (*FlowExecution, error) {
	if flow == nil {
		return nil, fmt.Errorf("flow: nil flow")
	}
	if flow.Status != FlowStatusActive && flow.Status != FlowStatusDraft {
		return nil, fmt.Errorf("flow %s is not active (status: %s)", flow.Name, flow.Status)
	}

	execCtx := NewExecutionContext(flow, triggerType, triggerData)
	if appContext != nil {
		execCtx.SetAppContext(appContext)
	}

	now := time.Now()
	execution := &FlowExecution{
		ID:             execCtx.ExecutionID,
		FlowID:         flow.ID,
		OrganizationID: flow.OrganizationID,
		Status:         ExecutionRunning,
		StartedAt:      &now,
		TriggerType:    triggerType,
		TriggerData:    triggerData,
		AppContext:     execCtx.AppContext,
		Variables:      make(map[string]interface{}),
	}

	if e.cfg.Store != nil {
		if err := e.cfg.Store.SaveExecution(context.Background(), execution); err != nil {
			return nil, fmt.Errorf("flow: save execution: %w", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), e.cfg.ExecutionTimeout)
	e.mu.Lock()
	e.activeExecs[execCtx.ExecutionID] = cancel
	e.mu.Unlock()

	go func() {
		defer cancel()
		defer func() {
			e.mu.Lock()
			delete(e.activeExecs, execCtx.ExecutionID)
			e.mu.Unlock()
		}()
		e.run(ctx, execCtx, execution)
	}()

	return execution, nil
}

// TestResult is the synchronous response from TestFlowInline. It mirrors a
// completed FlowExecution minus the persistence side-effects.
type TestResult struct {
	Status      string                 `json:"status"`
	Error       string                 `json:"error,omitempty"`
	ErrorNode   string                 `json:"error_node,omitempty"`
	NodeResults []NodeExecution        `json:"node_results"`
	Variables   map[string]interface{} `json:"variables,omitempty"`
	Duration    int64                  `json:"duration_ms"`
}

// TestFlowInline runs a flow synchronously without touching the Store. Used
// by editor "Test" buttons and by unit tests. The timeout is Config.TestTimeout.
func (e *Engine) TestFlowInline(flow *Flow, triggerData map[string]interface{}, appContext map[string]interface{}) (*TestResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), e.cfg.TestTimeout)
	defer cancel()

	execCtx := NewExecutionContext(flow, flow.TriggerType, triggerData)
	if appContext != nil {
		execCtx.SetAppContext(appContext)
	}

	triggerNode := execCtx.FindTriggerNode()
	if triggerNode == nil {
		return &TestResult{Status: "failed", Error: "no trigger node found"}, nil
	}

	if _, err := e.executeNodeWithRetry(ctx, execCtx, triggerNode); err != nil {
		return &TestResult{Status: "failed", Error: err.Error(), NodeResults: execCtx.NodeLogs}, nil
	}

	queue := e.resolveNextNodes(execCtx, triggerNode.ID, "")
	visited := map[string]bool{triggerNode.ID: true}
	count := 0

	for len(queue) > 0 && count < e.cfg.MaxNodeExecs {
		select {
		case <-ctx.Done():
			return &TestResult{Status: "timeout", NodeResults: execCtx.NodeLogs, Variables: execCtx.Variables}, nil
		default:
		}

		currentID := queue[0]
		queue = queue[1:]
		if visited[currentID] {
			continue
		}
		visited[currentID] = true
		count++

		node := execCtx.FindNode(currentID)
		if node == nil {
			continue
		}

		result, err := e.executeNodeWithRetry(ctx, execCtx, node)
		if err != nil {
			if node.Data.ContinueOnError {
				for _, edge := range execCtx.FindOutgoingEdgesForHandle(node.ID, "error") {
					queue = append(queue, edge.Target)
				}
				continue
			}
			return &TestResult{
				Status:      "failed",
				Error:       err.Error(),
				ErrorNode:   node.ID,
				NodeResults: execCtx.NodeLogs,
				Variables:   execCtx.Variables,
			}, nil
		}

		if result != nil && result.Stop {
			break
		}

		queue = append(queue, e.collectNextIDs(execCtx, node.ID, result)...)
	}

	return &TestResult{
		Status:      "completed",
		NodeResults: execCtx.NodeLogs,
		Variables:   execCtx.Variables,
		Duration:    time.Since(execCtx.StartedAt).Milliseconds(),
	}, nil
}

// CancelExecution aborts a running execution. Returns an error if the
// execution is not currently tracked (e.g. already completed).
func (e *Engine) CancelExecution(executionID uuid.UUID) error {
	e.mu.Lock()
	cancel, ok := e.activeExecs[executionID]
	e.mu.Unlock()
	if !ok {
		return fmt.Errorf("execution %s is not active", executionID)
	}
	cancel()

	if e.cfg.Store != nil {
		now := time.Now()
		exec := &FlowExecution{
			ID:          executionID,
			Status:      ExecutionCancelled,
			CompletedAt: &now,
		}
		if err := e.cfg.Store.UpdateExecution(context.Background(), exec); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) run(ctx context.Context, execCtx *ExecutionContext, execution *FlowExecution) {
	e.cfg.Logger.Printf("flow: starting execution %s for flow %q", execCtx.ExecutionID, execCtx.Flow.Name)

	triggerNode := execCtx.FindTriggerNode()
	if triggerNode == nil {
		e.failExecution(execution, "", "no trigger node found in flow")
		return
	}

	if _, err := e.executeNodeWithRetry(ctx, execCtx, triggerNode); err != nil {
		e.failExecution(execution, triggerNode.ID, err.Error())
		return
	}

	nextIDs := e.resolveNextNodes(execCtx, triggerNode.ID, "")
	if len(nextIDs) == 0 {
		e.completeExecution(execCtx, execution)
		return
	}

	queue := nextIDs
	visited := map[string]bool{triggerNode.ID: true}
	count := 0

	for len(queue) > 0 && count < e.cfg.MaxNodeExecs {
		select {
		case <-ctx.Done():
			e.failExecution(execution, execCtx.CurrentNodeID, "execution cancelled or timed out")
			return
		default:
		}

		currentID := queue[0]
		queue = queue[1:]
		if visited[currentID] {
			continue
		}
		visited[currentID] = true
		count++

		node := execCtx.FindNode(currentID)
		if node == nil {
			e.cfg.Logger.Printf("flow: node %s not found, skipping", currentID)
			continue
		}

		result, err := e.executeNodeWithRetry(ctx, execCtx, node)
		if err != nil {
			if node.Data.ContinueOnError {
				e.cfg.Logger.Printf("flow: node %s failed but continueOnError=true: %v", node.ID, err)
				for _, edge := range execCtx.FindOutgoingEdgesForHandle(node.ID, "error") {
					queue = append(queue, edge.Target)
				}
				continue
			}
			e.failExecution(execution, node.ID, err.Error())
			return
		}

		if result != nil && result.Stop {
			e.cfg.Logger.Printf("flow: node %s requested stop", node.ID)
			break
		}

		queue = append(queue, e.collectNextIDs(execCtx, node.ID, result)...)
		e.emitProgress(execCtx, node.ID, "completed")
	}

	if count >= e.cfg.MaxNodeExecs {
		e.failExecution(execution, execCtx.CurrentNodeID, fmt.Sprintf("execution exceeded max node limit (%d)", e.cfg.MaxNodeExecs))
		return
	}
	e.completeExecution(execCtx, execution)
}

func (e *Engine) collectNextIDs(execCtx *ExecutionContext, nodeID string, result *NodeResult) []string {
	switch {
	case result != nil && len(result.OutputHandles) > 0:
		var ids []string
		for _, h := range result.OutputHandles {
			ids = append(ids, e.resolveNextNodes(execCtx, nodeID, h)...)
		}
		return ids
	case result != nil && result.OutputHandle != "":
		return e.resolveNextNodes(execCtx, nodeID, result.OutputHandle)
	default:
		return e.resolveNextNodes(execCtx, nodeID, "")
	}
}

func (e *Engine) executeNodeWithRetry(ctx context.Context, execCtx *ExecutionContext, node *FlowNode) (*NodeResult, error) {
	execCtx.CurrentNodeID = node.ID

	executor, ok := e.registry.Get(node.Type)
	if !ok {
		return nil, fmt.Errorf("no executor for node type: %s", node.Type)
	}

	maxRetries := node.Data.RetryCount
	if maxRetries < 0 {
		maxRetries = 0
	}

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			delay := time.Duration(node.Data.RetryDelay) * time.Second
			if delay == 0 {
				delay = 2 * time.Second
			}
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			e.cfg.Logger.Printf("flow: retrying node %s (attempt %d/%d)", node.ID, attempt+1, maxRetries+1)
		}

		startedAt := time.Now()
		result, err := executor.Execute(ctx, node, execCtx)
		completedAt := time.Now()
		duration := completedAt.Sub(startedAt).Milliseconds()

		log := NodeExecution{
			NodeID:      node.ID,
			NodeType:    node.Type,
			StartedAt:   startedAt,
			CompletedAt: &completedAt,
			Duration:    duration,
		}

		if err != nil {
			lastErr = err
			log.Status = ExecutionFailed
			log.Error = err.Error()
			execCtx.NodeLogs = append(execCtx.NodeLogs, log)
			if attempt < maxRetries {
				continue
			}
			return nil, lastErr
		}

		log.Status = ExecutionCompleted
		if result != nil {
			log.Output = result.Output
		}
		execCtx.NodeLogs = append(execCtx.NodeLogs, log)

		if result != nil && result.Output != nil {
			for k, v := range result.Output {
				execCtx.Set(fmt.Sprintf("%s.%s", node.ID, k), v)
			}
			for _, ov := range node.Data.OutputVariables {
				if val, ok := result.Output[ov.Name]; ok {
					execCtx.Set(ov.Name, val)
				}
			}
		}

		e.cfg.Logger.Printf("flow: node %s (%s) completed in %dms", node.ID, node.Type, duration)
		return result, nil
	}
	return nil, lastErr
}

func (e *Engine) resolveNextNodes(execCtx *ExecutionContext, nodeID, handle string) []string {
	var edges []FlowEdge
	if handle != "" {
		edges = execCtx.FindOutgoingEdgesForHandle(nodeID, handle)
	} else {
		edges = execCtx.FindOutgoingEdges(nodeID)
	}

	var ids []string
	for _, edge := range edges {
		if !execCtx.EvaluateCondition(edge.Condition) {
			continue
		}
		ids = append(ids, edge.Target)
	}
	return ids
}

func (e *Engine) completeExecution(execCtx *ExecutionContext, execution *FlowExecution) {
	now := time.Now()
	duration := now.Sub(execCtx.StartedAt).Milliseconds()

	execution.Status = ExecutionCompleted
	execution.CompletedAt = &now
	execution.Duration = duration
	execution.Variables = execCtx.Variables
	execution.NodeExecutions = execCtx.NodeLogs

	if e.cfg.Store != nil {
		if err := e.cfg.Store.UpdateExecution(context.Background(), execution); err != nil {
			e.cfg.Logger.Printf("flow: update execution %s: %v", execution.ID, err)
		}
		if err := e.cfg.Store.RecordCompletion(context.Background(), execution.FlowID, duration); err != nil {
			e.cfg.Logger.Printf("flow: record completion %s: %v", execution.FlowID, err)
		}
	}

	e.cfg.Logger.Printf("flow: execution %s completed in %dms (%d nodes)", execCtx.ExecutionID, duration, len(execCtx.NodeLogs))
	e.emitProgress(execCtx, "", "completed")
}

func (e *Engine) failExecution(execution *FlowExecution, nodeID, errMsg string) {
	now := time.Now()
	execution.Status = ExecutionFailed
	execution.CompletedAt = &now
	execution.ErrorMessage = errMsg
	execution.ErrorNode = nodeID

	if e.cfg.Store != nil {
		if err := e.cfg.Store.UpdateExecution(context.Background(), execution); err != nil {
			e.cfg.Logger.Printf("flow: update execution %s: %v", execution.ID, err)
		}
		if err := e.cfg.Store.RecordFailure(context.Background(), execution.FlowID); err != nil {
			e.cfg.Logger.Printf("flow: record failure %s: %v", execution.FlowID, err)
		}
	}

	e.cfg.Logger.Printf("flow: execution %s failed at node %s: %s", execution.ID, nodeID, errMsg)
	if e.cfg.Progress != nil {
		e.cfg.Progress.OnProgress(execution.OrganizationID, execution.ID, execution.FlowID, nodeID, "failed")
	}
}

func (e *Engine) emitProgress(execCtx *ExecutionContext, nodeID, status string) {
	if e.cfg.Progress == nil {
		return
	}
	e.cfg.Progress.OnProgress(execCtx.OrganizationID, execCtx.ExecutionID, execCtx.FlowID, nodeID, status)
}
