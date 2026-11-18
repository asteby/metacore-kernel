package toolharness

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/agent"
	"github.com/asteby/metacore-kernel/agent/contextmgr"
	"github.com/asteby/metacore-kernel/agent/tools"
	"github.com/google/uuid"
)

type fakeTool struct {
	name string
	cat  string
	run  func(ctx context.Context) (interface{}, error)
}

func (f *fakeTool) Name() string                       { return f.name }
func (f *fakeTool) Description() string                { return f.name }
func (f *fakeTool) Category() string                   { return f.cat }
func (f *fakeTool) Parameters() map[string]interface{} { return nil }
func (f *fakeTool) Execute(ctx context.Context, _ map[string]interface{}, _ *contextmgr.ConversationContext) (interface{}, error) {
	return f.run(ctx)
}

// richTool declares every optional interface the harness must forward.
type richTool struct{ fakeTool }

func (richTool) ToolTimeout() time.Duration                  { return 3 * time.Second }
func (richTool) ParallelSafe() bool                          { return false }
func (richTool) ProgressLabel(map[string]interface{}) string { return "globe|propio" }
func (richTool) PromptHint() string                          { return "pista propia" }

func TestWrap_SkipsFinalAnswerAndIsIdempotent(t *testing.T) {
	fa := &fakeTool{name: "final_answer"}
	if Wrap(fa, Options{}) != tools.BaseTool(fa) {
		t.Fatal("final_answer no debe envolverse")
	}
	w := Wrap(&fakeTool{name: "x", cat: "data"}, Options{})
	if Wrap(w, Options{}) != w {
		t.Fatal("envolver dos veces debe devolver el mismo envoltorio")
	}
	if Wrap(nil, Options{}) != nil {
		t.Fatal("nil debe seguir nil")
	}
}

func TestWrap_ForwardsOptionalInterfaces(t *testing.T) {
	r := &richTool{fakeTool{name: "rich", cat: "data"}}
	w := Wrap(r, Options{Progress: func(string, map[string]interface{}) string { return "fallback" }})

	if got := w.(tools.TimeoutHinter).ToolTimeout(); got != 3*time.Second {
		t.Fatalf("ToolTimeout no se reenvió: %v", got)
	}
	if w.(tools.ParallelSafe).ParallelSafe() {
		t.Fatal("ParallelSafe=false de la herramienta debe ganar a la categoría data")
	}
	if got := w.(tools.ProgressLabeler).ProgressLabel(nil); got != "globe|propio" {
		t.Fatalf("la etiqueta propia debe ganar al fallback: %q", got)
	}
	if got := w.(tools.PromptHinter).PromptHint(); got != "pista propia" {
		t.Fatalf("PromptHint no se reenvió: %q", got)
	}
	if Unwrap(w) == w {
		t.Fatal("Unwrap debe quitar el envoltorio")
	}
}

func TestWrap_DefaultsWhenToolDeclaresNothing(t *testing.T) {
	w := Wrap(&fakeTool{name: "plain", cat: "data"}, Options{
		Progress: func(n string, _ map[string]interface{}) string { return "icon|" + n },
	})
	if !w.(tools.ParallelSafe).ParallelSafe() {
		t.Fatal("categoría data debe ser paralelizable por defecto")
	}
	if w.(tools.TimeoutHinter).ToolTimeout() != 0 {
		t.Fatal("sin pista el timeout debe ser 0 (valor del orquestador)")
	}
	if got := w.(tools.ProgressLabeler).ProgressLabel(nil); got != "icon|plain" {
		t.Fatalf("debe usar el fallback del host: %q", got)
	}
	if w.(tools.PromptHinter).PromptHint() != "" {
		t.Fatal("sin pista debe devolver vacío")
	}
	act := Wrap(&fakeTool{name: "send", cat: "action"}, Options{})
	if act.(tools.ParallelSafe).ParallelSafe() {
		t.Fatal("una herramienta de acción no debe correr en paralelo por defecto")
	}
}

func TestExecute_TraceAuditAndProgress(t *testing.T) {
	var events []agent.AIActivityEvent
	var audited []ToolCallTrace
	conv := uuid.New()
	w := Wrap(&fakeTool{name: "ok", cat: "data", run: func(context.Context) (interface{}, error) {
		return map[string]int{"a": 1}, nil
	}}, Options{
		Stream: func() agent.StreamCallback {
			return func(_ uuid.UUID, e agent.AIActivityEvent) { events = append(events, e) }
		},
		Progress: func(string, map[string]interface{}) string { return "search|Buscando…" },
		Audit: func(_ *contextmgr.ConversationContext, _ map[string]interface{}, tr ToolCallTrace) {
			audited = append(audited, tr)
		},
	})

	ctx, trace := WithTurnTrace(context.Background())
	if _, err := w.Execute(ctx, nil, &contextmgr.ConversationContext{ConversationID: conv}); err != nil {
		t.Fatal(err)
	}
	calls := trace.Snapshot()
	if len(calls) != 1 || calls[0].Tool != "ok" || !calls[0].Success || calls[0].ResultBytes == 0 {
		t.Fatalf("traza inesperada: %+v", calls)
	}
	if len(audited) != 1 || audited[0].Tool != "ok" {
		t.Fatalf("auditoría no recibió la llamada: %+v", audited)
	}
	if len(events) != 1 || events[0].Type != "tool_call" || events[0].Message != "search|Buscando…" {
		t.Fatalf("progreso inesperado: %+v", events)
	}

	// Sin acumulador en el contexto no debe fallar.
	if _, err := w.Execute(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestExecute_PanicBecomesError(t *testing.T) {
	w := Wrap(&fakeTool{name: "boom", cat: "data", run: func(context.Context) (interface{}, error) { panic("x") }}, Options{})
	ctx, trace := WithTurnTrace(context.Background())
	_, err := w.Execute(ctx, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "falló internamente") {
		t.Fatalf("el panic debe volverse error, got %v", err)
	}
	if c := trace.Snapshot(); len(c) != 1 || c[0].Success {
		t.Fatalf("la traza debe marcar el fallo: %+v", c)
	}
}

func TestExecute_TimeoutDoesNotWaitForStuckTool(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	w := Wrap(&fakeTool{name: "slow", cat: "data", run: func(context.Context) (interface{}, error) {
		<-release // ignora el contexto
		return nil, nil
	}}, Options{DefaultTimeout: 50 * time.Millisecond})

	ctx, trace := WithTurnTrace(context.Background())
	start := time.Now()
	_, err := w.Execute(ctx, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "excedió su tiempo máximo") {
		t.Fatalf("debe devolver timeout, got %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("no debe esperar a la herramienta atascada")
	}
	if c := trace.Snapshot(); len(c) != 1 || !c[0].TimedOut {
		t.Fatalf("la traza debe marcar timed_out: %+v", c)
	}
}

func TestExecute_CallerCancellationIsNotATimeout(t *testing.T) {
	w := Wrap(&fakeTool{name: "c", cat: "data", run: func(ctx context.Context) (interface{}, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}, Options{DefaultTimeout: 5 * time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := w.Execute(ctx, nil, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("la cancelación del turno debe propagarse, got %v", err)
	}
}

func TestSanitizeParams_BoundsLongValues(t *testing.T) {
	got := SanitizeParams(map[string]interface{}{
		"q": strings.Repeat("a", 500), "n": 3.0, "list": []string{strings.Repeat("b", 500)},
	}, 20)
	if n := len([]rune(got["q"].(string))); n > 21 {
		t.Fatalf("el texto largo no se acotó: %d", n)
	}
	if got["n"] != 3.0 {
		t.Fatal("los escalares deben pasar tal cual")
	}
	if n := len([]rune(got["list"].(string))); n > 21 {
		t.Fatalf("la estructura no se acotó: %d", n)
	}
}
