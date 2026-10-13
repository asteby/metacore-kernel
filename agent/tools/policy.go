package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"
	"unicode/utf8"
)

// Optional interfaces a tool can implement to tune how the orchestrator runs
// it. A tool that implements none keeps the defaults: the orchestrator's
// timeout, no retries for plain errors, and parallel execution only for
// read-only categories (data, knowledge, context).

// TimeoutHinter overrides the orchestrator's per-call timeout for one tool
// (e.g. a web reader that needs 25s, a calculator that needs 1s).
type TimeoutHinter interface {
	ToolTimeout() time.Duration
}

// ParallelSafe declares whether calls to the tool may run concurrently with
// other calls of the same model turn. Read-only lookups are safe; anything
// that mutates state, sends messages or depends on a previous call is not.
type ParallelSafe interface {
	ParallelSafe() bool
}

// ProgressLabeler returns the "icon|text" line streamed to the user while the
// call runs ("globe|Leyendo afmedios.com…"). Empty means "use the default".
type ProgressLabeler interface {
	ProgressLabel(params map[string]interface{}) string
}

// ErrTransient marks an error worth retrying (timeouts of an upstream, 429,
// 5xx, connection resets). Wrap it with Transient; plain errors are final.
var ErrTransient = errors.New("error transitorio")

type transientError struct{ err error }

func (e transientError) Error() string { return e.err.Error() }
func (e transientError) Unwrap() []error {
	return []error{e.err, ErrTransient}
}

// Transient wraps err so the orchestrator retries the call with backoff.
func Transient(err error) error {
	if err == nil {
		return nil
	}
	return transientError{err: err}
}

// IsTransient reports whether err should be retried: errors wrapped with
// Transient, errors exposing Temporary() true, and network timeouts. Context
// cancellation and the orchestrator's own per-call deadline are never retried.
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrTransient) {
		return true
	}
	var tmp interface{ Temporary() bool }
	if errors.As(err, &tmp) && tmp.Temporary() {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// IsParallelSafe applies the ParallelSafe override or the category default.
func IsParallelSafe(t BaseTool) bool {
	if p, ok := t.(ParallelSafe); ok {
		return p.ParallelSafe()
	}
	switch ToolCategory(t.Category()) {
	case CategoryData, CategoryKnowledge, CategoryContext:
		return true
	}
	return false
}

// FormatForLLMLimit renders the result as compact JSON (indentation costs
// ~30% more tokens for nothing) and caps it at maxBytes. A capped result ends
// with an explicit notice so the model knows data is missing and asks for less
// (filters, pagination) instead of treating the cut as the whole answer.
// maxBytes <= 0 disables the cap.
func (r ToolResult) FormatForLLMLimit(maxBytes int) (text string, truncated bool) {
	if !r.Success {
		text = fmt.Sprintf("Error executing %s: %s", r.ToolName, r.Error)
	} else if b, err := json.Marshal(r.Result); err != nil {
		text = fmt.Sprintf("Result of %s: %v", r.ToolName, r.Result)
	} else {
		text = fmt.Sprintf("Result of %s:\n%s", r.ToolName, b)
	}
	if maxBytes <= 0 || len(text) <= maxBytes {
		return text, false
	}
	omitted := len(text) - maxBytes
	cut := text[:maxBytes]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + fmt.Sprintf("\n[RESULTADO TRUNCADO: se omitieron %d bytes. Lo anterior está incompleto; "+
		"no lo presentes como total. Pide menos filas, usa filtros o la siguiente página.]", omitted), true
}
