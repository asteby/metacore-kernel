package manifest_test

import (
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
)

// stockManifest builds a minimal Stock model whose `available` column is a
// Postgres STORED generated column: available = quantity - reserved. mutate runs
// against the returned value before validation.
func stockManifest(mutate func(m *manifest.Manifest)) manifest.Manifest {
	m := manifest.Manifest{
		Key:     "stock",
		Name:    "Stock",
		Version: "1.0.0",
		Kernel:  ">=2.0.0 <3.0.0",
		ModelDefinitions: []manifest.ModelDefinition{
			{
				TableName: "stock_levels",
				ModelKey:  "Stock",
				OrgScoped: true,
				Columns: []manifest.ColumnDef{
					{Name: "quantity", Type: "bigint"},
					{Name: "reserved", Type: "bigint"},
					{Name: "available", Type: "bigint", Generated: "quantity - reserved"},
				},
			},
		},
	}
	if mutate != nil {
		mutate(&m)
	}
	return m
}

func TestLegacyValidate_Generated_OK(t *testing.T) {
	m := stockManifest(nil)
	if err := m.Validate("2.0.0"); err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
}

func TestLegacyValidate_Generated_SelfReference(t *testing.T) {
	m := stockManifest(func(m *manifest.Manifest) {
		m.ModelDefinitions[0].Columns[2].Generated = "available - reserved"
	})
	err := m.Validate("2.0.0")
	if err == nil || !strings.Contains(err.Error(), "reference itself") {
		t.Fatalf("expected self-reference error, got: %v", err)
	}
}

func TestLegacyValidate_Generated_UnknownIdent(t *testing.T) {
	m := stockManifest(func(m *manifest.Manifest) {
		m.ModelDefinitions[0].Columns[2].Generated = "quantity - ghost"
	})
	err := m.Validate("2.0.0")
	if err == nil || !strings.Contains(err.Error(), "generated expr") {
		t.Fatalf("expected unknown-ident error, got: %v", err)
	}
}

func TestLegacyValidate_Generated_WithDefault(t *testing.T) {
	m := stockManifest(func(m *manifest.Manifest) {
		m.ModelDefinitions[0].Columns[2].Default = 0
	})
	err := m.Validate("2.0.0")
	if err == nil || !strings.Contains(err.Error(), "cannot also declare a default") {
		t.Fatalf("expected default-conflict error, got: %v", err)
	}
}

func TestLegacyValidate_Generated_WithRequired(t *testing.T) {
	m := stockManifest(func(m *manifest.Manifest) {
		m.ModelDefinitions[0].Columns[2].Required = true
	})
	err := m.Validate("2.0.0")
	if err == nil || !strings.Contains(err.Error(), "cannot be required") {
		t.Fatalf("expected required-conflict error, got: %v", err)
	}
}

func TestLegacyValidate_Generated_Injection(t *testing.T) {
	m := stockManifest(func(m *manifest.Manifest) {
		m.ModelDefinitions[0].Columns[2].Generated = "quantity); DROP TABLE stock_levels; --"
	})
	if err := m.Validate("2.0.0"); err == nil {
		t.Fatalf("expected injection rejection, got nil")
	}
}
