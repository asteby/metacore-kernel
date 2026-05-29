package dynamic

import (
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
)

func TestDeriveTableColumns(t *testing.T) {
	def := manifest.ModelDefinition{
		Columns: []manifest.ColumnDef{
			{Name: "name", Type: "string"},
			{Name: "amount", Type: "decimal"},
			{Name: "is_active", Type: "boolean"},
			{Name: "start_date", Type: "date"},
			{Name: "customer_id", Type: "uuid"},
		},
	}

	cols := DeriveTableColumns(def)
	if len(cols) != 5 {
		t.Fatalf("expected 5 columns, got %d", len(cols))
	}

	tests := []struct {
		key      string
		label    string
		typ      string
		sortable bool
	}{
		{"name", "Name", "text", true},
		{"amount", "Amount", "number", true},
		{"is_active", "Is Active", "boolean", true},
		{"start_date", "Start Date", "datetime", true},
		{"customer_id", "Customer", "text", true}, // " Id" stripped, uuid → text
	}
	for i, want := range tests {
		got := cols[i]
		if got.Key != want.key {
			t.Errorf("col[%d].Key = %q, want %q", i, got.Key, want.key)
		}
		if got.Label != want.label {
			t.Errorf("col[%d].Label = %q, want %q", i, got.Label, want.label)
		}
		if got.Type != want.typ {
			t.Errorf("col[%d].Type = %q, want %q", i, got.Type, want.typ)
		}
		if got.Sortable != want.sortable {
			t.Errorf("col[%d].Sortable = %v, want %v", i, got.Sortable, want.sortable)
		}
	}
}

func TestDeriveTableColumnsEmpty(t *testing.T) {
	cols := DeriveTableColumns(manifest.ModelDefinition{})
	if cols == nil {
		t.Fatal("expected non-nil slice for empty def")
	}
	if len(cols) != 0 {
		t.Fatalf("expected 0 columns, got %d", len(cols))
	}
}

func TestDeriveFormFieldsSkipsManaged(t *testing.T) {
	def := manifest.ModelDefinition{
		Columns: []manifest.ColumnDef{
			{Name: "id", Type: "uuid"},
			{Name: "name", Type: "string", Required: true},
			{Name: "notes", Type: "text"},
			{Name: "created_at", Type: "timestamp"},
			{Name: "updated_at", Type: "timestamp"},
			{Name: "organization_id", Type: "uuid"},
			{Name: "deleted_at", Type: "timestamp"},
			{Name: "qty", Type: "integer"},
		},
	}

	fields := DeriveFormFields(def)
	if len(fields) != 3 {
		t.Fatalf("expected 3 fields (managed skipped), got %d: %+v", len(fields), fields)
	}

	tests := []struct {
		key      string
		label    string
		typ      string
		required bool
	}{
		{"name", "Name", "text", true},
		{"notes", "Notes", "textarea", false},
		{"qty", "Qty", "number", false},
	}
	for i, want := range tests {
		got := fields[i]
		if got.Key != want.key {
			t.Errorf("field[%d].Key = %q, want %q", i, got.Key, want.key)
		}
		if got.Label != want.label {
			t.Errorf("field[%d].Label = %q, want %q", i, got.Label, want.label)
		}
		if got.Type != want.typ {
			t.Errorf("field[%d].Type = %q, want %q", i, got.Type, want.typ)
		}
		if got.Required != want.required {
			t.Errorf("field[%d].Required = %v, want %v", i, got.Required, want.required)
		}
	}
}

func TestColumnUIType(t *testing.T) {
	cases := map[string]string{
		"integer":     "number",
		"int":         "number",
		"bigint":      "number",
		"float":       "number",
		"numeric":     "number",
		"decimal":     "number",
		"number":      "number",
		"boolean":     "boolean",
		"bool":        "boolean",
		"date":        "datetime",
		"datetime":    "datetime",
		"timestamp":   "datetime",
		"timestamptz": "datetime",
		"string":      "text",
		"text":        "text",
		"jsonb":       "text",
		"uuid":        "text",
		"":            "text",
	}
	for in, want := range cases {
		if got := ColumnUIType(in); got != want {
			t.Errorf("ColumnUIType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFormFieldType(t *testing.T) {
	cases := map[string]string{
		"integer":     "number",
		"decimal":     "number",
		"boolean":     "boolean",
		"bool":        "boolean",
		"date":        "date",
		"timestamp":   "date",
		"timestamptz": "date",
		"text":        "textarea",
		"jsonb":       "textarea",
		"json":        "textarea",
		"string":      "text",
		"uuid":        "text",
		"":            "text",
	}
	for in, want := range cases {
		if got := FormFieldType(in); got != want {
			t.Errorf("FormFieldType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDefaultsCRUDEnabled(t *testing.T) {
	if DefaultsCRUDEnabled(manifest.ModelDefinition{}) {
		t.Error("expected CRUD disabled for a def with no columns")
	}
	withCols := manifest.ModelDefinition{Columns: []manifest.ColumnDef{{Name: "name", Type: "string"}}}
	if !DefaultsCRUDEnabled(withCols) {
		t.Error("expected CRUD enabled for a def with columns")
	}
}
