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

// withActions returns a minimal valid manifest that hangs the supplied
// ActionDef under model key "tickets". Mirrors the validate_test helpers
// for column/relation extensions so each trigger test stays focused on the
// field under check.
func withActions(actions ...manifest.ActionDef) manifest.Manifest {
	return manifest.Manifest{
		Key:     "act",
		Name:    "Act",
		Version: "1.0.0",
		Actions: map[string][]manifest.ActionDef{
			"tickets": actions,
		},
	}
}

// withWasmActions is identical to withActions but also wires a Backend
// declaration so wasm triggers can resolve their Export. The exports
// argument is variadic so tests pick whichever symbols they expect to be
// present (or absent) in the backend declaration.
func withWasmActions(exports []string, actions ...manifest.ActionDef) manifest.Manifest {
	m := withActions(actions...)
	m.Backend = &manifest.BackendSpec{
		Runtime: "wasm",
		Entry:   "backend/b.wasm",
		Exports: exports,
	}
	return m
}

func TestValidate_ActionTrigger_NilIsBackwardsCompat(t *testing.T) {
	// An ActionDef without Trigger must validate just like before — the
	// addon ecosystem has months of manifests with no trigger field set.
	m := withActions(manifest.ActionDef{Key: "escalate", Name: "Escalate", Label: "Escalate"})
	if err := m.Validate("2.0.0"); err != nil {
		t.Fatalf("nil trigger should still validate, got %v", err)
	}
}

func TestValidate_ActionTrigger_WasmOK(t *testing.T) {
	m := withWasmActions([]string{"escalateTicket"}, manifest.ActionDef{
		Key:     "escalate",
		Name:    "Escalate",
		Label:   "Escalate",
		Trigger: &manifest.ActionTrigger{Type: "wasm", Export: "escalateTicket", RunInTx: true},
	})
	if err := m.Validate("2.0.0"); err != nil {
		t.Fatalf("valid wasm trigger should pass, got %v", err)
	}
}

func TestValidate_ActionTrigger_WasmRequiresExport(t *testing.T) {
	m := withWasmActions([]string{"escalateTicket"}, manifest.ActionDef{
		Key:     "escalate",
		Name:    "Escalate",
		Label:   "Escalate",
		Trigger: &manifest.ActionTrigger{Type: "wasm"},
	})
	err := m.Validate("2.0.0")
	if err == nil || !strings.Contains(err.Error(), "export") {
		t.Fatalf("expected export-required error, got %v", err)
	}
}

func TestValidate_ActionTrigger_WasmExportNotInBackend(t *testing.T) {
	// The export must appear in Backend.Exports so the wasm host can
	// resolve it at dispatch — same contract enforced for hooks.
	m := withWasmActions([]string{"otherSymbol"}, manifest.ActionDef{
		Key:     "escalate",
		Name:    "Escalate",
		Label:   "Escalate",
		Trigger: &manifest.ActionTrigger{Type: "wasm", Export: "escalateTicket"},
	})
	err := m.Validate("2.0.0")
	if err == nil || !strings.Contains(err.Error(), "backend.exports") {
		t.Fatalf("expected export-mismatch error, got %v", err)
	}
}

func TestValidate_ActionTrigger_WasmExportInvalidSymbol(t *testing.T) {
	m := withWasmActions([]string{"with space"}, manifest.ActionDef{
		Key:     "escalate",
		Name:    "Escalate",
		Label:   "Escalate",
		Trigger: &manifest.ActionTrigger{Type: "wasm", Export: "with space"},
	})
	err := m.Validate("2.0.0")
	if err == nil || !strings.Contains(err.Error(), "invalid symbol") {
		t.Fatalf("expected invalid-symbol error, got %v", err)
	}
}

func TestValidate_ActionTrigger_WasmWithoutBackendExports(t *testing.T) {
	// No Backend declared at all — wasm trigger has nothing to point at.
	m := withActions(manifest.ActionDef{
		Key:     "escalate",
		Name:    "Escalate",
		Label:   "Escalate",
		Trigger: &manifest.ActionTrigger{Type: "wasm", Export: "escalateTicket"},
	})
	err := m.Validate("2.0.0")
	if err == nil || !strings.Contains(err.Error(), "backend.exports") {
		t.Fatalf("expected export-mismatch error when Backend is nil, got %v", err)
	}
}

func TestValidate_ActionTrigger_WebhookOK(t *testing.T) {
	m := withActions(manifest.ActionDef{
		Key:     "escalate",
		Name:    "Escalate",
		Label:   "Escalate",
		Trigger: &manifest.ActionTrigger{Type: "webhook"},
	})
	if err := m.Validate("2.0.0"); err != nil {
		t.Fatalf("webhook trigger without export should pass, got %v", err)
	}
}

func TestValidate_ActionTrigger_WebhookRejectsExport(t *testing.T) {
	m := withActions(manifest.ActionDef{
		Key:     "escalate",
		Name:    "Escalate",
		Label:   "Escalate",
		Trigger: &manifest.ActionTrigger{Type: "webhook", Export: "shouldNotBeHere"},
	})
	err := m.Validate("2.0.0")
	if err == nil || !strings.Contains(err.Error(), "export") {
		t.Fatalf("expected export-not-allowed error, got %v", err)
	}
}

func TestValidate_ActionTrigger_WebhookRejectsRunInTx(t *testing.T) {
	// A webhook hop escapes the request transaction, so honouring RunInTx
	// would silently lie. Reject at authoring time.
	m := withActions(manifest.ActionDef{
		Key:     "escalate",
		Name:    "Escalate",
		Label:   "Escalate",
		Trigger: &manifest.ActionTrigger{Type: "webhook", RunInTx: true},
	})
	err := m.Validate("2.0.0")
	if err == nil || !strings.Contains(err.Error(), "run_in_tx") {
		t.Fatalf("expected run_in_tx-not-allowed error, got %v", err)
	}
}

func TestValidate_ActionTrigger_NoopOK(t *testing.T) {
	m := withActions(manifest.ActionDef{
		Key:     "track",
		Name:    "Track",
		Label:   "Track",
		Trigger: &manifest.ActionTrigger{Type: "noop"},
	})
	if err := m.Validate("2.0.0"); err != nil {
		t.Fatalf("noop trigger should pass, got %v", err)
	}
}

func TestValidate_ActionTrigger_NoopRejectsExport(t *testing.T) {
	m := withActions(manifest.ActionDef{
		Key:     "track",
		Name:    "Track",
		Label:   "Track",
		Trigger: &manifest.ActionTrigger{Type: "noop", Export: "irrelevant"},
	})
	err := m.Validate("2.0.0")
	if err == nil || !strings.Contains(err.Error(), "export") {
		t.Fatalf("expected export-not-allowed error, got %v", err)
	}
}

func TestValidate_ActionTrigger_NoopRejectsRunInTx(t *testing.T) {
	m := withActions(manifest.ActionDef{
		Key:     "track",
		Name:    "Track",
		Label:   "Track",
		Trigger: &manifest.ActionTrigger{Type: "noop", RunInTx: true},
	})
	err := m.Validate("2.0.0")
	if err == nil || !strings.Contains(err.Error(), "run_in_tx") {
		t.Fatalf("expected run_in_tx-not-allowed error, got %v", err)
	}
}

func TestValidate_ActionTrigger_UnknownType(t *testing.T) {
	m := withActions(manifest.ActionDef{
		Key:     "track",
		Name:    "Track",
		Label:   "Track",
		Trigger: &manifest.ActionTrigger{Type: "queue"},
	})
	err := m.Validate("2.0.0")
	if err == nil || !strings.Contains(err.Error(), "trigger.type") {
		t.Fatalf("expected unknown-type error, got %v", err)
	}
}

func TestValidate_ActionTrigger_OnExtension(t *testing.T) {
	// Triggers on actions added via ModelExtension follow the same rules.
	m := manifest.Manifest{
		Key:     "act",
		Name:    "Act",
		Version: "1.0.0",
		Backend: &manifest.BackendSpec{
			Runtime: "wasm",
			Entry:   "backend/b.wasm",
			Exports: []string{"escalateTicket"},
		},
		Extensions: []manifest.ModelExtension{{
			Model: "tickets",
			Actions: []manifest.ActionDef{{
				Key:     "escalate",
				Name:    "Escalate",
				Label:   "Escalate",
				Trigger: &manifest.ActionTrigger{Type: "wasm", Export: "escalateTicket"},
			}},
		}},
	}
	if err := m.Validate("2.0.0"); err != nil {
		t.Fatalf("extension trigger should validate, got %v", err)
	}
}

func TestValidate_ActionTrigger_ErrorPathIncludesIndex(t *testing.T) {
	// The path stitched onto an actions-map error should surface both the
	// model key and the slice index so operators can grep it.
	m := manifest.Manifest{
		Key:     "act",
		Name:    "Act",
		Version: "1.0.0",
		Actions: map[string][]manifest.ActionDef{
			"tickets": {
				{Key: "ok", Name: "Ok", Label: "Ok"},
				{
					Key:     "broken",
					Name:    "Broken",
					Label:   "Broken",
					Trigger: &manifest.ActionTrigger{Type: "queue"},
				},
			},
		},
	}
	err := m.Validate("2.0.0")
	if err == nil ||
		!strings.Contains(err.Error(), `actions["tickets"][1]`) ||
		!strings.Contains(err.Error(), "trigger.type") {
		t.Fatalf("expected fully-qualified action path in error, got %v", err)
	}
}
