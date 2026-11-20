package manifest

import (
	"testing"

	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// A compiled handler projects onto the same trigger as a wasm export, so every
// tier that dispatches wasm exports also reaches a native Go export.
func TestHandlerToTrigger_CompiledSharesTheWasmSeam(t *testing.T) {
	w := handlerToTrigger(v3.Handler{Type: "wasm", Function: "OnConfirm"})
	c := handlerToTrigger(v3.Handler{Type: "compiled", Function: "OnConfirm"})
	if w == nil || c == nil || w.Type != c.Type || w.Export != c.Export {
		t.Fatalf("compiled must project like wasm: wasm=%+v compiled=%+v", w, c)
	}
	if c.Type != "wasm" || c.Export != "OnConfirm" {
		t.Fatalf("unexpected trigger %+v", c)
	}
}
