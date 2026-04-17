package manifest_test

import (
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
)

func TestValidate_OK(t *testing.T) {
	m := manifest.Manifest{
		Key:     "tickets",
		Name:    "Tickets",
		Version: "1.0.0",
		Kernel:  ">=2.0.0 <3.0.0",
		ModelDefinitions: []manifest.ModelDefinition{{
			TableName: "tickets",
			ModelKey:  "tickets",
			Columns:   []manifest.ColumnDef{{Name: "title", Type: "string"}},
		}},
		Capabilities: []manifest.Capability{
			{Kind: "db:read", Target: "users"},
		},
	}
	if err := m.Validate("2.0.0"); err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
}

func TestValidate_KernelRange(t *testing.T) {
	m := manifest.Manifest{
		Key:     "aa",
		Name:    "A",
		Version: "1.0.0",
		Kernel:  ">=3.0.0",
	}
	err := m.Validate("2.0.0")
	if err == nil || !strings.Contains(err.Error(), "does not satisfy") {
		t.Fatalf("expected kernel mismatch, got %v", err)
	}
}

func TestValidate_BadKey(t *testing.T) {
	m := manifest.Manifest{Key: "Bad-Key!", Name: "x", Version: "1.0.0"}
	if err := m.Validate("2.0.0"); err == nil {
		t.Fatal("expected invalid key")
	}
}

func TestValidate_BackendWasmRequiresEntry(t *testing.T) {
	m := manifest.Manifest{
		Key: "aa", Name: "A", Version: "1.0.0",
		Backend: &manifest.BackendSpec{Runtime: "wasm"},
	}
	if err := m.Validate("2.0.0"); err == nil || !strings.Contains(err.Error(), "entry") {
		t.Fatalf("expected entry-required error, got %v", err)
	}
}

func TestValidate_BackendWasmHookNotExported(t *testing.T) {
	m := manifest.Manifest{
		Key: "aa", Name: "A", Version: "1.0.0",
		Hooks: map[string]string{"fiscal_documents::stamp_fiscal": "foo"},
		Backend: &manifest.BackendSpec{
			Runtime: "wasm",
			Entry:   "backend/b.wasm",
			Exports: []string{"cancel_fiscal"},
		},
	}
	if err := m.Validate("2.0.0"); err == nil || !strings.Contains(err.Error(), "stamp_fiscal") {
		t.Fatalf("expected export-mismatch error, got %v", err)
	}
}

func TestValidate_BackendUnknownRuntime(t *testing.T) {
	m := manifest.Manifest{
		Key: "aa", Name: "A", Version: "1.0.0",
		Backend: &manifest.BackendSpec{Runtime: "magic"},
	}
	if err := m.Validate("2.0.0"); err == nil {
		t.Fatal("expected unknown runtime error")
	}
}

func TestValidate_CapabilityKind(t *testing.T) {
	m := manifest.Manifest{
		Key:          "aa",
		Name:         "A",
		Version:      "1.0.0",
		Capabilities: []manifest.Capability{{Kind: "weird", Target: "x"}},
	}
	if err := m.Validate("2.0.0"); err == nil {
		t.Fatal("expected capability kind error")
	}
}
