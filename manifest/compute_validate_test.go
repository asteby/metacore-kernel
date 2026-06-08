package manifest_test

import (
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
)

// salesManifest builds the legacy-shape manifest equivalent of the v3 sales
// example: a SalesOrder parent (org-scoped) with a one_to_many "items" relation
// carrying Tier-1 rollups, and a SalesOrderItem child carrying a Tier-2 formula.
// mutate runs against the returned value before validation.
func salesManifest(mutate func(m *manifest.Manifest)) manifest.Manifest {
	m := manifest.Manifest{
		Key:     "sales",
		Name:    "Sales",
		Version: "1.0.0",
		Kernel:  ">=2.0.0 <3.0.0",
		ModelDefinitions: []manifest.ModelDefinition{
			{
				TableName: "sales_orders",
				ModelKey:  "SalesOrder",
				OrgScoped: true,
				Columns: []manifest.ColumnDef{
					{Name: "total", Type: "decimal"},
					{Name: "line_count", Type: "int"},
				},
				Relations: []manifest.RelationDef{
					{
						Name:       "items",
						Kind:       "one_to_many",
						Through:    "SalesOrderItem",
						ForeignKey: "sales_order_id",
						Rollups: []manifest.Rollup{
							{Target: "total", Fn: "sum", From: "subtotal"},
							{Target: "line_count", Fn: "count"},
						},
					},
				},
			},
			{
				TableName: "sales_order_items",
				ModelKey:  "SalesOrderItem",
				Columns: []manifest.ColumnDef{
					{Name: "sales_order_id", Type: "uuid"},
					{Name: "quantity", Type: "int"},
					{Name: "unit_price", Type: "decimal"},
					{Name: "discount", Type: "decimal"},
					{Name: "subtotal", Type: "decimal"},
				},
				Formulas: []manifest.Formula{
					{Target: "subtotal", Expr: "quantity * unit_price - discount"},
				},
			},
		},
	}
	if mutate != nil {
		mutate(&m)
	}
	return m
}

func TestLegacyValidate_RollupsAndFormulas_OK(t *testing.T) {
	m := salesManifest(nil)
	if err := m.Validate("2.0.0"); err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
}

func TestLegacyValidate_Rollup_TargetNotOnParent(t *testing.T) {
	m := salesManifest(func(m *manifest.Manifest) {
		m.ModelDefinitions[0].Relations[0].Rollups[0].Target = "ghost"
	})
	err := m.Validate("2.0.0")
	if err == nil || !strings.Contains(err.Error(), "not a declared column on the parent") {
		t.Fatalf("expected parent-column error, got: %v", err)
	}
}

func TestLegacyValidate_Rollup_FromNotOnChild(t *testing.T) {
	m := salesManifest(func(m *manifest.Manifest) {
		m.ModelDefinitions[0].Relations[0].Rollups[0].From = "ghost"
	})
	err := m.Validate("2.0.0")
	if err == nil || !strings.Contains(err.Error(), "not a declared column on the child") {
		t.Fatalf("expected child-column error, got: %v", err)
	}
}

func TestLegacyValidate_Rollup_BadFn(t *testing.T) {
	m := salesManifest(func(m *manifest.Manifest) {
		m.ModelDefinitions[0].Relations[0].Rollups[0].Fn = "median"
	})
	err := m.Validate("2.0.0")
	if err == nil || !strings.Contains(err.Error(), "fn") {
		t.Fatalf("expected fn error, got: %v", err)
	}
}

func TestLegacyValidate_Rollup_ExprInjection(t *testing.T) {
	m := salesManifest(func(m *manifest.Manifest) {
		m.ModelDefinitions[0].Relations[0].Rollups[0].From = ""
		m.ModelDefinitions[0].Relations[0].Rollups[0].Expr = "subtotal); DROP TABLE sales_orders; --"
	})
	if err := m.Validate("2.0.0"); err == nil {
		t.Fatalf("expected injection rejection, got nil")
	}
}

func TestLegacyValidate_Rollup_BothFromAndExpr(t *testing.T) {
	m := salesManifest(func(m *manifest.Manifest) {
		m.ModelDefinitions[0].Relations[0].Rollups[0].Expr = "subtotal"
	})
	err := m.Validate("2.0.0")
	if err == nil || !strings.Contains(err.Error(), "both from and expr") {
		t.Fatalf("expected both-from-and-expr error, got: %v", err)
	}
}

func TestLegacyValidate_Formula_TargetNotOnModel(t *testing.T) {
	m := salesManifest(func(m *manifest.Manifest) {
		m.ModelDefinitions[1].Formulas[0].Target = "phantom"
	})
	err := m.Validate("2.0.0")
	if err == nil || !strings.Contains(err.Error(), "not a declared column on the model") {
		t.Fatalf("expected formula-target error, got: %v", err)
	}
}

func TestLegacyValidate_Formula_ExprSemicolon(t *testing.T) {
	m := salesManifest(func(m *manifest.Manifest) {
		m.ModelDefinitions[1].Formulas[0].Expr = "quantity; SELECT 1"
	})
	if err := m.Validate("2.0.0"); err == nil {
		t.Fatalf("expected expr-injection rejection, got nil")
	}
}

func TestLegacyValidate_Formula_ExprUnknownIdent(t *testing.T) {
	m := salesManifest(func(m *manifest.Manifest) {
		m.ModelDefinitions[1].Formulas[0].Expr = "quantity * mystery"
	})
	err := m.Validate("2.0.0")
	if err == nil || !strings.Contains(err.Error(), "unknown column") {
		t.Fatalf("expected unknown-column error, got: %v", err)
	}
}
