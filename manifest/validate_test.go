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

// withColumn returns a minimal valid manifest containing a single column —
// helper to keep the column-extension tests focused on the field under test.
func withColumn(col manifest.ColumnDef) manifest.Manifest {
	return manifest.Manifest{
		Key:     "ext",
		Name:    "Ext",
		Version: "1.0.0",
		ModelDefinitions: []manifest.ModelDefinition{{
			TableName: "items",
			ModelKey:  "items",
			Columns:   []manifest.ColumnDef{col},
		}},
	}
}

func ptrF(f float64) *float64 { return &f }

func TestValidate_ColumnExtensions_ZeroValueIsBackwardsCompat(t *testing.T) {
	// A column without any of the new fields populated must validate
	// exactly like before — this is the contract the task hinges on.
	m := withColumn(manifest.ColumnDef{Name: "title", Type: "string"})
	if err := m.Validate("2.0.0"); err != nil {
		t.Fatalf("zero-value extension fields should still validate, got %v", err)
	}
}

func TestValidate_ColumnVisibility(t *testing.T) {
	for _, v := range []string{"all", "table", "modal", "list"} {
		m := withColumn(manifest.ColumnDef{Name: "title", Type: "string", Visibility: v})
		if err := m.Validate("2.0.0"); err != nil {
			t.Errorf("visibility=%q should be accepted, got %v", v, err)
		}
	}
}

func TestValidate_ColumnVisibilityRejected(t *testing.T) {
	m := withColumn(manifest.ColumnDef{Name: "title", Type: "string", Visibility: "everywhere"})
	err := m.Validate("2.0.0")
	if err == nil || !strings.Contains(err.Error(), "visibility") {
		t.Fatalf("expected visibility error, got %v", err)
	}
}

func TestValidate_ColumnWidgetAccepted(t *testing.T) {
	m := withColumn(manifest.ColumnDef{Name: "title", Type: "string", Widget: "textarea"})
	if err := m.Validate("2.0.0"); err != nil {
		t.Fatalf("widget=textarea should be accepted, got %v", err)
	}
}

func TestValidate_ColumnWidgetRejected(t *testing.T) {
	m := withColumn(manifest.ColumnDef{Name: "title", Type: "string", Widget: "neural-blob"})
	err := m.Validate("2.0.0")
	if err == nil || !strings.Contains(err.Error(), "widget") {
		t.Fatalf("expected widget error, got %v", err)
	}
}

func TestValidate_ColumnSearchable(t *testing.T) {
	// Searchable is a plain bool — there is no invalid state, just
	// confirm it survives a round trip through Validate without
	// upsetting the existing checks.
	m := withColumn(manifest.ColumnDef{Name: "title", Type: "string", Searchable: true})
	if err := m.Validate("2.0.0"); err != nil {
		t.Fatalf("searchable=true should validate, got %v", err)
	}
}

func TestValidate_ValidationRegexCompiles(t *testing.T) {
	m := withColumn(manifest.ColumnDef{
		Name:       "tax_id",
		Type:       "string",
		Validation: &manifest.ValidationRule{Regex: `^[A-Z0-9]{12,13}$`},
	})
	if err := m.Validate("2.0.0"); err != nil {
		t.Fatalf("valid regex should pass, got %v", err)
	}
}

func TestValidate_ValidationRegexBroken(t *testing.T) {
	m := withColumn(manifest.ColumnDef{
		Name:       "tax_id",
		Type:       "string",
		Validation: &manifest.ValidationRule{Regex: `(unclosed`},
	})
	err := m.Validate("2.0.0")
	if err == nil || !strings.Contains(err.Error(), "regex") {
		t.Fatalf("expected regex compile error, got %v", err)
	}
}

func TestValidate_ValidationMinMaxOK(t *testing.T) {
	m := withColumn(manifest.ColumnDef{
		Name:       "age",
		Type:       "int",
		Validation: &manifest.ValidationRule{Min: ptrF(0), Max: ptrF(150)},
	})
	if err := m.Validate("2.0.0"); err != nil {
		t.Fatalf("min<=max should pass, got %v", err)
	}
}

func TestValidate_ValidationMinGreaterThanMax(t *testing.T) {
	m := withColumn(manifest.ColumnDef{
		Name:       "age",
		Type:       "int",
		Validation: &manifest.ValidationRule{Min: ptrF(10), Max: ptrF(5)},
	})
	err := m.Validate("2.0.0")
	if err == nil || !strings.Contains(err.Error(), "min") {
		t.Fatalf("expected min>max error, got %v", err)
	}
}

func TestValidate_ValidationMinOnlyIsOK(t *testing.T) {
	// Only Min set — there is no Max to compare against, must pass.
	m := withColumn(manifest.ColumnDef{
		Name:       "age",
		Type:       "int",
		Validation: &manifest.ValidationRule{Min: ptrF(18)},
	})
	if err := m.Validate("2.0.0"); err != nil {
		t.Fatalf("min-only should pass, got %v", err)
	}
}

func TestValidate_ValidationCustomShape(t *testing.T) {
	good := withColumn(manifest.ColumnDef{
		Name:       "rfc",
		Type:       "string",
		Validation: &manifest.ValidationRule{Custom: "rfc.tax_id"},
	})
	if err := good.Validate("2.0.0"); err != nil {
		t.Fatalf("dotted custom should pass, got %v", err)
	}

	bad := withColumn(manifest.ColumnDef{
		Name:       "rfc",
		Type:       "string",
		Validation: &manifest.ValidationRule{Custom: "Has Space!"},
	})
	if err := bad.Validate("2.0.0"); err == nil || !strings.Contains(err.Error(), "custom") {
		t.Fatalf("expected custom shape error, got %v", err)
	}
}

func TestValidate_AllColumnExtensionsTogether(t *testing.T) {
	m := withColumn(manifest.ColumnDef{
		Name:       "email",
		Type:       "string",
		Visibility: "modal",
		Searchable: true,
		Widget:     "email",
		Validation: &manifest.ValidationRule{
			Regex:  `^[^@]+@[^@]+\.[^@]+$`,
			Min:    ptrF(3),
			Max:    ptrF(254),
			Custom: "email.deliverable",
		},
	})
	if err := m.Validate("2.0.0"); err != nil {
		t.Fatalf("rich column should validate, got %v", err)
	}
}
