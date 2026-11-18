// Package toolharness is the shared harness every host (ops, vantora, ...)
// wraps its agent tools with. One implementation, inherited by all hosts, so a
// fix lands once instead of being re-ported into each fork:
//
//   - per-call trace (tool, duration, result bytes, error) accumulated on the
//     turn (WithTurnTrace) so the chat endpoint can return it;
//   - per-call timeout and panic recovery;
//   - real progress to the user ("Leyendo afmedios.com…") through the
//     activity stream, with the tool's own label;
//   - a host Audit hook (the kernel knows nothing about the host's database);
//   - forwarding of the optional tool interfaces (ToolTimeout, ParallelSafe,
//     ProgressLabel, PromptHint): an embedded tools.BaseTool hides them, and a
//     wrapper that drops them silently changes how the tool runs.
//
// Parallelism, transient retries and the result cap belong to the kernel's
// orchestrator loop; the harness only makes them see the wrapped tool exactly
// as they would see the bare one.
package toolharness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/asteby/metacore-kernel/agent"
	"github.com/asteby/metacore-kernel/agent/contextmgr"
	"github.com/asteby/metacore-kernel/agent/tools"
)

// DefaultTimeout applies when neither the tool nor Options set one.
const DefaultTimeout = 30 * time.Second

// ToolCallTrace is the trace of one call.
type ToolCallTrace struct {
	Tool        string `json:"tool"`
	DurationMS  int64  `json:"duration_ms"`
	ResultBytes int    `json:"result_bytes"`
	Success     bool   `json:"success"`
	Error       string `json:"error,omitempty"`
	TimedOut    bool   `json:"timed_out,omitempty"`
}

// TurnTrace accumulates the traces of a turn (safe for concurrent use: the
// orchestrator may run independent calls in parallel).
type TurnTrace struct {
	mu    sync.Mutex
	Calls []ToolCallTrace
}

type turnTraceKey struct{}

// WithTurnTrace attaches a trace accumulator to the turn's context.
func WithTurnTrace(ctx context.Context) (context.Context, *TurnTrace) {
	t := &TurnTrace{}
	return context.WithValue(ctx, turnTraceKey{}, t), t
}

// TraceFrom returns the context's accumulator, or nil.
func TraceFrom(ctx context.Context) *TurnTrace {
	t, _ := ctx.Value(turnTraceKey{}).(*TurnTrace)
	return t
}

func (t *TurnTrace) add(c ToolCallTrace) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.Calls = append(t.Calls, c)
	t.mu.Unlock()
}

// Snapshot copies the traces.
func (t *TurnTrace) Snapshot() []ToolCallTrace {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]ToolCallTrace(nil), t.Calls...)
}

// Options are the host's knobs. Every field is optional.
type Options struct {
	// DefaultTimeout bounds a call whose tool declares none (0 = DefaultTimeout).
	DefaultTimeout time.Duration
	// Stream returns the live activity callback (it can change between turns,
	// hence a func). Nil = no progress events.
	Stream func() agent.StreamCallback
	// Progress supplies a fallback "icon|text" label for a tool that has no
	// ProgressLabel of its own. Empty = no progress event.
	Progress func(tool string, params map[string]interface{}) string
	// Audit receives every finished call (host-side persistence). It runs
	// synchronously: spawn a goroutine inside if it does I/O.
	Audit func(agentCtx *contextmgr.ConversationContext, params map[string]interface{}, tr ToolCallTrace)
}

// Wrapped is implemented by every harness wrapper so callers can see through it.
type Wrapped interface {
	tools.BaseTool
	Unwrap() tools.BaseTool
}

// Unwrap peels every harness layer off t.
func Unwrap(t tools.BaseTool) tools.BaseTool {
	for {
		w, ok := t.(Wrapped)
		if !ok {
			return t
		}
		t = w.Unwrap()
	}
}

type harnessTool struct {
	tools.BaseTool
	opts Options
}

// Wrap returns t wrapped with the harness. final_answer is never wrapped (it is
// not a step the user waits on) and an already wrapped tool is returned as is.
func Wrap(t tools.BaseTool, o Options) tools.BaseTool {
	if t == nil || t.Name() == "final_answer" {
		return t
	}
	if _, ok := t.(*harnessTool); ok {
		return t
	}
	return &harnessTool{BaseTool: t, opts: o}
}

// Unwrap returns the wrapped tool.
func (h *harnessTool) Unwrap() tools.BaseTool { return h.BaseTool }

// ToolTimeout forwards the tool's own hint (0 = the orchestrator's value).
func (h *harnessTool) ToolTimeout() time.Duration {
	if th, ok := h.BaseTool.(tools.TimeoutHinter); ok {
		return th.ToolTimeout()
	}
	return 0
}

// PromptHint forwards the tool's usage guidance to the orchestrator.
func (h *harnessTool) PromptHint() string {
	if ph, ok := h.BaseTool.(tools.PromptHinter); ok {
		return ph.PromptHint()
	}
	return ""
}

// ParallelSafe forwards the declaration, or applies the category default
// (read-only categories run in parallel).
func (h *harnessTool) ParallelSafe() bool {
	if ps, ok := h.BaseTool.(tools.ParallelSafe); ok {
		return ps.ParallelSafe()
	}
	switch tools.ToolCategory(h.Category()) {
	case tools.CategoryData, tools.CategoryKnowledge, tools.CategoryContext:
		return true
	}
	return false
}

// ProgressLabel forwards the tool's own label, then the host fallback.
func (h *harnessTool) ProgressLabel(params map[string]interface{}) string {
	if pl, ok := h.BaseTool.(tools.ProgressLabeler); ok {
		if l := pl.ProgressLabel(params); l != "" {
			return l
		}
	}
	if h.opts.Progress != nil {
		return h.opts.Progress(h.Name(), params)
	}
	return ""
}

// Execute runs the tool under the harness.
func (h *harnessTool) Execute(ctx context.Context, params map[string]interface{}, agentCtx *contextmgr.ConversationContext) (interface{}, error) {
	start := time.Now()
	if label := h.ProgressLabel(params); label != "" && agentCtx != nil && h.opts.Stream != nil {
		if cb := h.opts.Stream(); cb != nil {
			cb(agentCtx.ConversationID, agent.AIActivityEvent{
				SessionID: agentCtx.ConversationID.String(), Type: "tool_call", Tool: h.Name(), Message: label, Success: true,
			})
		}
	}
	timeout := h.ToolTimeout()
	if timeout <= 0 {
		timeout = h.opts.DefaultTimeout
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	out, err, timedOut := runWithTimeout(ctx, timeout, func(c context.Context) (interface{}, error) {
		return h.BaseTool.Execute(c, params, agentCtx)
	})
	tr := ToolCallTrace{Tool: h.Name(), DurationMS: time.Since(start).Milliseconds(), Success: err == nil, TimedOut: timedOut}
	if err != nil {
		tr.Error = agent.ClipRunes(err.Error(), 300)
	} else if b, merr := json.Marshal(out); merr == nil {
		tr.ResultBytes = len(b)
	}
	TraceFrom(ctx).add(tr)
	if h.opts.Audit != nil {
		h.opts.Audit(agentCtx, params, tr)
	}
	return out, err
}

// runWithTimeout does not wait for a tool that ignores its context past the
// timeout, and turns a panic into an error.
func runWithTimeout(ctx context.Context, d time.Duration, fn func(context.Context) (interface{}, error)) (interface{}, error, bool) {
	cctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	type res struct {
		v   interface{}
		err error
	}
	ch := make(chan res, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("💥 herramienta con panic: %v", r)
				ch <- res{err: errors.New("la herramienta falló internamente")}
			}
		}()
		v, err := fn(cctx)
		ch <- res{v, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil && errors.Is(r.err, context.DeadlineExceeded) && ctx.Err() == nil {
			return nil, fmt.Errorf("la herramienta excedió su tiempo máximo (%v)", d), true
		}
		return r.v, r.err, false
	case <-cctx.Done():
		if ctx.Err() != nil {
			return nil, ctx.Err(), false
		}
		return nil, fmt.Errorf("la herramienta excedió su tiempo máximo (%v)", d), true
	}
}

// SanitizeParams bounds every value for an audit record (no long texts).
func SanitizeParams(params map[string]interface{}, maxRunes int) map[string]interface{} {
	out := make(map[string]interface{}, len(params))
	for k, v := range params {
		switch x := v.(type) {
		case string:
			out[k] = agent.ClipRunes(x, maxRunes)
		case float64, int, bool, nil:
			out[k] = x
		default:
			b, _ := json.Marshal(x)
			out[k] = agent.ClipRunes(string(b), maxRunes)
		}
	}
	return out
}
