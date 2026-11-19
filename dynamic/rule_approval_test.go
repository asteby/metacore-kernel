package dynamic

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
	"github.com/asteby/metacore-kernel/metadata"
	"github.com/asteby/metacore-kernel/modelbase"
)

const ruleApprovalManifest = `{
  "apiVersion": "asteby.com/v3", "kind": "Addon",
  "metadata": {"key": "pricing", "name": "Pricing", "version": "1.0.0"},
  "compatibility": {"requires": [{"key": "kernel", "version": ">=3.0.0 <4.0.0"}]},
  "tenancy": {"isolation": "shared", "rls_column": "organization_id"},
  "capabilities": [{"kind": "db:write", "target": "addon_pricing.*"}],
  "models": [{"key": "Product", "table": "test_products", "columns": [
    {"name": "id", "type": "uuid"},
    {"name": "organization_id", "type": "uuid", "not_null": true},
    {"name": "name", "type": "text"},
    {"name": "price", "type": "numeric"}
  ]}],
  "contributions": {"rules": [{
    "key": "price_cap", "model": "Product", "when": "price > 100",
    "then": {"kind": "request_approval", "message": "pricing.over_cap",
             "approval": {"roles": ["manager"]}}
  }]}
}`

// P5: una regla declarativa pide aprobacion ANTES de escribir; el supervisor
// aprueba y la misma escritura se repite saltando solo esa regla.
func TestRuleApproval_ParksAndReplays(t *testing.T) {
	m, err := v3.Parse([]byte(ruleApprovalManifest))
	if err != nil {
		t.Fatal(err)
	}
	var md manifest.ModelDefinition
	for _, d := range manifest.FromV3(m).ModelDefinitions {
		if d.ModelKey == "Product" {
			md = d
		}
	}
	if len(md.RuleGuards) != 1 || !md.RuleGuards[0].Invert || md.RuleGuards[0].ErrorKey != "rule:price_cap" {
		t.Fatalf("rule not projected: %+v", md.RuleGuards)
	}
	mc, ok := BuildModelConstraints(md)
	if !ok {
		t.Fatal("no constraints built")
	}

	db := setupTestDB(t)
	modelbase.Register("test_products", func() modelbase.ModelDefiner { return &TestProduct{} })
	roles := map[uuid.UUID][]string{}
	svc := New(Config{
		AllowUnconfiguredPermissions: true, DB: db,
		Metadata: metadata.New(metadata.Config{CacheTTL: -1}),
		ConstraintResolver: func(_ context.Context, model string) (*ModelConstraints, bool) {
			return mc, model == "test_products"
		},
		ActorRolesResolver: func(_ context.Context, u modelbase.AuthUser) []string { return roles[u.GetID()] },
	})
	org := uuid.New()
	clerk, boss := newUser(org), newUser(org)
	roles[boss.GetID()] = []string{"manager"}
	ctx := context.Background()

	// Bajo el tope: pasa directo.
	if _, err := svc.Create(ctx, "test_products", clerk, map[string]any{"name": "ok", "price": 80.0}); err != nil {
		t.Fatalf("under the cap: %v", err)
	}
	// Sobre el tope: aprobacion requerida y NADA escrito.
	_, err = svc.Create(ctx, "test_products", clerk, map[string]any{"name": "caro", "price": 150.0})
	var ar *ApprovalRequiredError
	if !errors.As(err, &ar) {
		t.Fatalf("over the cap: %v, want approval_required", err)
	}
	var n int64
	db.Raw("SELECT COUNT(*) FROM test_products WHERE name = 'caro'").Scan(&n)
	if n != 0 {
		t.Fatalf("a parked write must not persist (%d rows)", n)
	}
	// Un empleado sin rol no aprueba; el gerente si, y la fila aparece.
	if _, err := svc.ApproveRequest(ctx, clerk, ar.Request.ID, ""); err == nil {
		t.Fatal("a non-approver approved the request")
	}
	if _, err := svc.ApproveRequest(ctx, boss, ar.Request.ID, ""); err != nil {
		t.Fatalf("approve: %v", err)
	}
	db.Raw("SELECT COUNT(*) FROM test_products WHERE name = 'caro'").Scan(&n)
	if n != 1 {
		t.Fatalf("approved write must be applied once, got %d rows", n)
	}
}

func ruleFixture(t *testing.T) (*Service, map[string]*ModelConstraints, manifest.ModelDefinition, *fakeUser, *fakeUser) {
	t.Helper()
	m, err := v3.Parse([]byte(ruleApprovalManifest))
	if err != nil {
		t.Fatal(err)
	}
	var md manifest.ModelDefinition
	for _, d := range manifest.FromV3(m).ModelDefinitions {
		if d.ModelKey == "Product" {
			md = d
		}
	}
	mc, _ := BuildModelConstraints(md)
	db := setupTestDB(t)
	modelbase.Register("test_products", func() modelbase.ModelDefiner { return &TestProduct{} })
	roles := map[uuid.UUID][]string{}
	svc := New(Config{
		AllowUnconfiguredPermissions: true, DB: db,
		Metadata: metadata.New(metadata.Config{CacheTTL: -1}),
		ConstraintResolver: func(_ context.Context, model string) (*ModelConstraints, bool) {
			return mc, model == "test_products"
		},
		ActorRolesResolver: func(_ context.Context, u modelbase.AuthUser) []string { return roles[u.GetID()] },
	})
	org := uuid.New()
	clerk, boss := newUser(org), newUser(org)
	roles[boss.GetID()] = []string{"manager"}
	return svc, nil, md, clerk, boss
}

// Un PUT que solo cambia `name` NO vuelve a pedir aprobacion tras aprobar un
// precio > tope; si cambia el precio (columna que la regla lee) si.
func TestRuleApproval_UnrelatedUpdateDoesNotReask(t *testing.T) {
	svc, _, _, clerk, boss := ruleFixture(t)
	ctx := context.Background()
	_, err := svc.Create(ctx, "test_products", clerk, map[string]any{"name": "caro", "price": 150.0})
	var ar *ApprovalRequiredError
	if !errors.As(err, &ar) {
		t.Fatalf("create: %v", err)
	}
	if _, err := svc.ApproveRequest(ctx, boss, ar.Request.ID, ""); err != nil {
		t.Fatal(err)
	}
	var idS string
	svc.db.Raw("SELECT id FROM test_products WHERE name = 'caro'").Scan(&idS)
	id := uuid.MustParse(idS)
	if _, err := svc.Update(ctx, "test_products", clerk, id, map[string]any{"name": "caro 2"}); err != nil {
		t.Fatalf("renaming an approved record must not re-ask: %v", err)
	}
	// Reenviar el mismo precio (formulario completo) tampoco cuenta como cambio.
	if _, err := svc.Update(ctx, "test_products", clerk, id, map[string]any{"name": "caro 3", "price": 150.0}); err != nil {
		t.Fatalf("resending the same price must not re-ask: %v", err)
	}
	// Cambiar el precio (aun sobre el tope) si vuelve a pedirla.
	if _, err := svc.Update(ctx, "test_products", clerk, id, map[string]any{"price": 200.0}); !errors.As(err, &ar) {
		t.Fatalf("changing the watched column: %v, want approval_required", err)
	}
	// Una fila bajo el tope que sube por encima, tambien.
	_, err = svc.Create(ctx, "test_products", clerk, map[string]any{"name": "barato", "price": 50.0})
	if err != nil {
		t.Fatal(err)
	}
	var id2S string
	svc.db.Raw("SELECT id FROM test_products WHERE name = 'barato'").Scan(&id2S)
	id2 := uuid.MustParse(id2S)
	if _, err := svc.Update(ctx, "test_products", clerk, id2, map[string]any{"price": 120.0}); !errors.As(err, &ar) {
		t.Fatalf("crossing the cap: %v, want approval_required", err)
	}
}

// Un host que arma ModelConstraints a mano pierde las reglas: el kernel lo avisa.
func TestRuleApproval_UnwiredRuleGuardsAreReported(t *testing.T) {
	svc, _, md, _, _ := ruleFixture(t)
	if got := svc.UnwiredRuleGuards(context.Background(), []manifest.ModelDefinition{md}); len(got) != 0 {
		t.Fatalf("wired via BuildModelConstraints, got %v", got)
	}
	handRolled := New(Config{
		AllowUnconfiguredPermissions: true, DB: svc.db,
		Metadata: metadata.New(metadata.Config{CacheTTL: -1}),
		ConstraintResolver: func(context.Context, string) (*ModelConstraints, bool) {
			return &ModelConstraints{Constraints: nil, Locking: ""}, false
		},
	})
	got := handRolled.UnwiredRuleGuards(context.Background(), []manifest.ModelDefinition{md})
	if len(got) != 1 || got[0] != "Product: rule:price_cap" {
		t.Fatalf("unwired = %v", got)
	}
}
