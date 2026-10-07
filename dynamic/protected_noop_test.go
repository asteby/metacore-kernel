package dynamic

import (
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
)

// Editar un producto sin fila de extensión (no es llanta): el formulario
// reenvía la llave de búsqueda vacía y eso no debe tumbar el guardado.
func TestProtectedNoop_UpdateEmptyWithoutExtensionRow(t *testing.T) {
	col := manifest.ColumnDef{Name: "size_key"}
	if !protectedNoop("", col, nil, true) {
		t.Fatal("vacío sin fila de extensión debe ser no-op")
	}
	if !protectedNoop(nil, col, map[string]any{}, true) {
		t.Fatal("nil con la columna ausente debe ser no-op")
	}
	if !protectedNoop("", col, map[string]any{"size_key": nil}, true) {
		t.Fatal("vacío contra NULL debe ser no-op")
	}
	// Cambiar el valor sigue rechazándose.
	if protectedNoop("205/55R16", col, map[string]any{}, true) {
		t.Fatal("un valor no vacío sin persistido no es no-op")
	}
	if protectedNoop("", col, map[string]any{"size_key": "205/55R16"}, true) {
		t.Fatal("vaciar un valor persistido no es no-op")
	}
	if !protectedNoop("205/55R16", col, map[string]any{"size_key": "205/55R16"}, true) {
		t.Fatal("reenviar el valor persistido es no-op")
	}
}
