package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"sync"
	"time"

	"github.com/asteby/metacore-kernel/agent/contextmgr"
	"github.com/asteby/metacore-kernel/agent/tools"
)

// Defaults of the tool execution policy. A zero field in OrchestratorConfig
// takes the default; a negative one disables the limit (retries, result cap,
// turn budget).
const (
	DefaultToolTimeout        = 30 * time.Second
	DefaultToolRetries        = 1
	DefaultMaxParallelTools   = 4
	DefaultMaxToolResultBytes = 24_000
	DefaultMaxTurnDuration    = 150 * time.Second
	toolRetryBaseDelay        = 400 * time.Millisecond
)

// ToolTrace is the record of one tool call: what ran, for how long, how big
// its result was and how it ended. The orchestrator collects them per turn
// (AgentResponse.Metadata["tool_trace"]) and hands each one to
// OrchestratorConfig.OnToolTrace so the host can audit it.
type ToolTrace struct {
	CallID      string    `json:"call_id"`
	Tool        string    `json:"tool"`
	Iteration   int       `json:"iteration"`
	StartedAt   time.Time `json:"started_at"`
	DurationMS  int64     `json:"duration_ms"`
	Attempts    int       `json:"attempts"`
	Parallel    bool      `json:"parallel"`
	Success     bool      `json:"success"`
	Error       string    `json:"error,omitempty"`
	TimedOut    bool      `json:"timed_out,omitempty"`
	ResultBytes int       `json:"result_bytes"`
	Truncated   bool      `json:"truncated,omitempty"`
}

// TurnStats summarizes one turn of the loop (AgentResponse.Metadata["turn_stats"]).
type TurnStats struct {
	Iterations   int   `json:"iterations"`
	LLMCalls     int   `json:"llm_calls"`
	ToolCalls    int   `json:"tool_calls"`
	DurationMS   int64 `json:"duration_ms"`
	BudgetForced bool  `json:"budget_forced,omitempty"`
}

// errToolTimeout is returned when a call exceeds its per-call deadline.
var errToolTimeout = errors.New("la herramienta excedió su tiempo máximo")

func (o *AgentOrchestrator) toolTimeout(t tools.BaseTool) time.Duration {
	if h, ok := t.(tools.TimeoutHinter); ok {
		if d := h.ToolTimeout(); d > 0 {
			return d
		}
	}
	if o.policy.ToolTimeout > 0 {
		return o.policy.ToolTimeout
	}
	return DefaultToolTimeout
}

func (o *AgentOrchestrator) toolRetries() int {
	switch {
	case o.policy.ToolRetries < 0:
		return 0
	case o.policy.ToolRetries == 0:
		return DefaultToolRetries
	}
	return o.policy.ToolRetries
}

func (o *AgentOrchestrator) maxParallel() int {
	switch {
	case o.policy.MaxParallelTools < 0:
		return 1
	case o.policy.MaxParallelTools == 0:
		return DefaultMaxParallelTools
	}
	return o.policy.MaxParallelTools
}

func (o *AgentOrchestrator) maxResultBytes() int {
	switch {
	case o.policy.MaxToolResultBytes < 0:
		return 0
	case o.policy.MaxToolResultBytes == 0:
		return DefaultMaxToolResultBytes
	}
	return o.policy.MaxToolResultBytes
}

func (o *AgentOrchestrator) maxTurnDuration() time.Duration {
	switch {
	case o.policy.MaxTurnDuration < 0:
		return 0
	case o.policy.MaxTurnDuration == 0:
		return DefaultMaxTurnDuration
	}
	return o.policy.MaxTurnDuration
}

// runToolCalls executes the calls of one model turn. Calls to parallel-safe
// tools (read-only lookups) run concurrently, bounded by MaxParallelTools;
// the rest run one by one, in order, after them. Results keep the order of
// calls whatever the execution order, as the providers expect.
func (o *AgentOrchestrator) runToolCalls(ctx context.Context, calls []tools.ToolCall, convCtx *contextmgr.ConversationContext, registry *tools.ToolRegistry, iteration int) ([]tools.ToolResult, []ToolTrace) {
	results := make([]tools.ToolResult, len(calls))
	traces := make([]ToolTrace, len(calls))
	var parallel, serial []int
	for i, c := range calls {
		if t, ok := registry.Get(c.ToolName); ok && tools.IsParallelSafe(t) && len(calls) > 1 {
			parallel = append(parallel, i)
		} else {
			serial = append(serial, i)
		}
	}
	if len(parallel) == 1 {
		serial = append(parallel, serial...)
		parallel = nil
	}
	if len(parallel) > 0 {
		sem := make(chan struct{}, o.maxParallel())
		var wg sync.WaitGroup
		for _, i := range parallel {
			wg.Add(1)
			sem <- struct{}{}
			go func(i int) {
				defer wg.Done()
				defer func() { <-sem }()
				results[i], traces[i] = o.runOne(ctx, calls[i], convCtx, registry)
				traces[i].Parallel = true
			}(i)
		}
		wg.Wait()
	}
	for _, i := range serial {
		results[i], traces[i] = o.runOne(ctx, calls[i], convCtx, registry)
	}
	for i := range traces {
		traces[i].Iteration = iteration
	}
	return results, traces
}

// runOne runs a single call with its timeout, panic recovery and retries
// (with exponential backoff and jitter) for transient errors only.
func (o *AgentOrchestrator) runOne(ctx context.Context, call tools.ToolCall, convCtx *contextmgr.ConversationContext, registry *tools.ToolRegistry) (tools.ToolResult, ToolTrace) {
	tr := ToolTrace{CallID: call.ID, Tool: call.ToolName, StartedAt: time.Now()}
	tool, ok := registry.Get(call.ToolName)
	if !ok {
		res := tools.ToolResult{ToolCallID: call.ID, ToolName: call.ToolName, Error: fmt.Sprintf("tool not found: %s", call.ToolName)}
		tr.Error, tr.Attempts = res.Error, 1
		return res, tr
	}
	timeout := o.toolTimeout(tool)
	retries := o.toolRetries()
	var (
		out interface{}
		err error
	)
	for attempt := 1; ; attempt++ {
		tr.Attempts = attempt
		out, err = executeWithDeadline(ctx, tool, call, convCtx, timeout)
		if err == nil || attempt > retries || !tools.IsTransient(err) || ctx.Err() != nil {
			break
		}
		delay := toolRetryBaseDelay<<(attempt-1) + time.Duration(rand.Int63n(int64(toolRetryBaseDelay)))
		log.Printf("🔁 Tool %s: error transitorio (%v), reintento %d en %v", call.ToolName, err, attempt, delay)
		select {
		case <-ctx.Done():
		case <-time.After(delay):
		}
	}
	tr.DurationMS = time.Since(tr.StartedAt).Milliseconds()
	res := tools.ToolResult{ToolCallID: call.ID, ToolName: call.ToolName}
	if err != nil {
		res.Error = err.Error()
		tr.Error = res.Error
		tr.TimedOut = errors.Is(err, errToolTimeout)
		log.Printf("❌ Tool %s failed after %d attempt(s) in %dms: %v", call.ToolName, tr.Attempts, tr.DurationMS, err)
		return res, tr
	}
	res.Success, res.Result = true, out
	tr.Success = true
	return res, tr
}

// executeWithDeadline runs the tool under a per-call deadline. A tool that
// ignores ctx is abandoned when the deadline passes (its goroutine finishes on
// its own); the loop never waits past the timeout. Panics become errors.
func executeWithDeadline(ctx context.Context, tool tools.BaseTool, call tools.ToolCall, convCtx *contextmgr.ConversationContext, timeout time.Duration) (interface{}, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	type outcome struct {
		v   interface{}
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("💥 Tool %s panicked: %v", call.ToolName, r)
				done <- outcome{err: fmt.Errorf("la herramienta %s falló internamente", call.ToolName)}
			}
		}()
		v, err := tool.Execute(cctx, call.Parameters, convCtx)
		done <- outcome{v, err}
	}()
	select {
	case r := <-done:
		if r.err != nil && errors.Is(r.err, context.DeadlineExceeded) && ctx.Err() == nil {
			return nil, fmt.Errorf("%w (%v)", errToolTimeout, timeout)
		}
		return r.v, r.err
	case <-cctx.Done():
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w (%v)", errToolTimeout, timeout)
	}
}

// progressLabel is the line streamed while a call runs: the tool's own label
// when it has one, else the generic description.
func (o *AgentOrchestrator) progressLabel(registry *tools.ToolRegistry, tc tools.ToolCall) string {
	if t, ok := registry.Get(tc.ToolName); ok {
		if pl, ok := t.(tools.ProgressLabeler); ok {
			if l := pl.ProgressLabel(tc.Parameters); l != "" {
				return l
			}
		}
	}
	return o.describeToolCall(tc.ToolName, tc.Parameters)
}
