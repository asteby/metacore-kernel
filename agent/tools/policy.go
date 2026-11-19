package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
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

// PromptHinter lets a tool teach the model how to use it. The orchestrator
// appends each registered tool's hint to the turn's system prompt, so the
// guidance travels with the tool: register the tool and the rule follows, in
// any host or preset, and a tool that is not registered adds nothing. Keep it
// to a few lines; empty means "no hint". A host wrapper around a tool must
// forward PromptHint (an embedded BaseTool hides optional interfaces).
type PromptHinter interface {
	PromptHint() string
}

// OutcomeClaimer lets a tool declare the phrases with which a reply claims to
// have produced what only this tool produces ("aquí está el gráfico", "ya
// creé el evento"). When the reply makes the claim and the tool did not run
// successfully in the turn, the orchestrator sends the model back to call it
// (once) or, failing that, tells the user the result was not produced. It is
// the generic form of "never announce what you did not do": any tool in any
// host opts in by implementing it, and an unregistered tool checks nothing.
//
// Patterns are keyed by language (primary BCP-47 subtag: "es", "en", "pt"...)
// so the mechanism is not tied to one language: the orchestrator accepts a
// claim in ANY declared language, and a host adds one without touching the
// tool (agent.RegisterOutcomeClaims). Keep patterns narrow: they must match an
// assertion that the outcome exists now, never an offer or a refusal.
type OutcomeClaimer interface {
	OutcomeClaims() map[string][]*regexp.Regexp
}

// Unwrapper is implemented by tool wrappers (harness, permission guards) so
// the kernel can reach the wrapped tool's optional interfaces.
type Unwrapper interface {
	Unwrap() BaseTool
}

// As returns t, or any tool it wraps, as the optional interface T. A wrapper
// that embeds BaseTool hides the optional interfaces of what it wraps; going
// through Unwrap lets the kernel see them without every host forwarding each
// one by hand. The outermost implementation wins.
func As[T any](t BaseTool) (T, bool) {
	for t != nil {
		if v, ok := t.(T); ok {
			return v, true
		}
		u, ok := t.(Unwrapper)
		if !ok {
			break
		}
		t = u.Unwrap()
	}
	var zero T
	return zero, false
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

// LLMTexter is a tool result that renders its own text for the model (for
// example a compact Markdown table instead of the JSON of its rows). The
// structured value still reaches embeds and hooks; an empty text falls back
// to JSON.
type LLMTexter interface {
	LLMText() string
}

func llmText(v interface{}) (string, bool) {
	t, ok := v.(LLMTexter)
	if !ok || t == nil {
		return "", false
	}
	s := t.LLMText()
	return s, s != ""
}

// FormatForLLMLimit renders the result as compact JSON (indentation costs
// ~30% more tokens for nothing) and caps it at maxBytes. A capped result ends
// with an explicit notice so the model knows data is missing and asks for less
// (filters, pagination) instead of treating the cut as the whole answer.
// maxBytes <= 0 disables the cap.
func (r ToolResult) FormatForLLMLimit(maxBytes int) (text string, truncated bool) {
	if !r.Success {
		text = fmt.Sprintf("Error executing %s: %s", r.ToolName, r.Error)
	} else if s, ok := llmText(r.Result); ok {
		text = fmt.Sprintf("Result of %s:\n%s", r.ToolName, s)
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
		"no lo presentes como total. Para ver el resto, repite la consulta más acotada: agrega filtros, "+
		"pide menos columnas, baja el limit o pide la siguiente página; para totales o conteos usa una agregación.]", omitted), true
}

// OutcomeSatisfier lets a tool declare that its success also backs the
// outcome claims of other tools: show_ui draws what show_chart used to, so
// "aquí está el gráfico" after a successful show_ui is not an unbacked claim
// about show_chart. The relation is declared by the tool that supersedes, not
// hardcoded in the guard.
type OutcomeSatisfier interface {
	SatisfiesOutcomes() []string
}
