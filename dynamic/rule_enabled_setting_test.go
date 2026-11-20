package dynamic

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
	"github.com/asteby/metacore-kernel/metadata"
	"github.com/asteby/metacore-kernel/modelbase"
)

const enabledSettingManifest = `{
  "apiVersion": "asteby.com/v3", "kind": "Addon",
  "metadata": {"key": "pricing", "name": "Pricing", "version": "1.0.0"},
  "compatibility": {"requires": [{"key": "kernel", "version": ">=3.0.0 <4.0.0"}]},
  "tenancy": {"isolation": "shared", "rls_column": "organization_id"},
  "capabilities": [{"kind": "db:write", "target": "addon_pricing.*"}],
  "settings": [{"key": "enforce_cap", "type": "boolean"}],
  "models": [{"key": "Product", "table": "test_products", "columns": [
    {"name": "id", "type": "uuid"},
    {"name": "organization_id", "type": "uuid", "not_null": true},
    {"name": "name", "type": "text"},
    {"name": "price", "type": "numeric"}
  ]}],
  "contributions": {"rules": [{
    "key": "price_cap", "model": "Product", "when": "price > 100", "enabled_setting": "enforce_cap",
    "then": {"kind": "request_approval", "message": "pricing.over_cap",
             "approval": {"roles": ["manager"]}}
  }]}
}`

func enabledSettingSvc(t *testing.T, resolve SettingResolver) (*Service, *ModelConstraints) {
	t.Helper()
	m, err := v3.Parse([]byte(enabledSettingManifest))
	if err != nil {
		t.Fatal(err)
	}
	var md manifest.ModelDefinition
	for _, d := range manifest.FromV3(m).ModelDefinitions {
		if d.ModelKey == "Product" {
			md = d
		}
	}
	g := md.RuleGuards
	if len(g) != 1 || g[0].EnabledSetting != "enforce_cap" || g[0].AddonKey != "pricing" {
		t.Fatalf("projection: %+v", g)
	}
	mc, _ := BuildModelConstraints(md)
	db := setupTestDB(t)
	modelbase.Register("test_products", func() modelbase.ModelDefiner { return &TestProduct{} })
	return New(Config{
		AllowUnconfiguredPermissions: true, DB: db,
		Metadata: metadata.New(metadata.Config{CacheTTL: -1}),
		ConstraintResolver: func(_ context.Context, model string) (*ModelConstraints, bool) {
			return mc, model == "test_products"
		},
		ActorRolesResolver: func(context.Context, modelbase.AuthUser) []string { return []string{"manager"} },
		SettingResolver:    resolve,
	}), mc
}

func TestEnabledSetting_OffByDefaultAndOnPerOrg(t *testing.T) {
	orgOn, orgOff := uuid.New(), uuid.New()
	var calls int32
	svc, _ := enabledSettingSvc(t, func(_ context.Context, org uuid.UUID, addon, key string) (bool, error) {
		atomic.AddInt32(&calls, 1)
		if addon != "pricing" || key != "enforce_cap" {
			t.Fatalf("resolver args %s/%s", addon, key)
		}
		return org == orgOn, nil
	})
	ctx := context.Background()
	over := map[string]any{"name": "caro", "price": 150.0}

	// Org encendida: parquea (K5).
	_, err := svc.Create(ctx, "test_products", newUser(orgOn), over)
	var ar *ApprovalRequiredError
	if !errors.As(err, &ar) {
		t.Fatalf("on: %v, want approval_required", err)
	}
	// Otra org (apagada): no se ve afectada.
	if _, err := svc.Create(ctx, "test_products", newUser(orgOff), over); err != nil {
		t.Fatalf("off org must not be gated: %v", err)
	}
	// Aprobar y repetir en la org encendida: el replay salta solo esa regla.
	if _, err := svc.ApproveRequest(ctx, newUser(orgOn), ar.Request.ID, ""); err != nil {
		t.Fatalf("approve/replay: %v", err)
	}
	// Bajo el tope en la org encendida pasa.
	if _, err := svc.Create(ctx, "test_products", newUser(orgOn), map[string]any{"name": "ok", "price": 5.0}); err != nil {
		t.Fatal(err)
	}
}

func TestEnabledSetting_ResolverErrorOrMissingIsOff(t *testing.T) {
	org := uuid.New()
	over := map[string]any{"name": "caro", "price": 150.0}
	ctx := context.Background()

	svc, _ := enabledSettingSvc(t, func(context.Context, uuid.UUID, string, string) (bool, error) {
		return true, errors.New("boom")
	})
	if _, err := svc.Create(ctx, "test_products", newUser(org), over); err != nil {
		t.Fatalf("resolver error must mean OFF: %v", err)
	}
	svc, _ = enabledSettingSvc(t, nil)
	if _, err := svc.Create(ctx, "test_products", newUser(org), over); err != nil {
		t.Fatalf("no resolver must mean OFF: %v", err)
	}
}

func TestEnabledSetting_UpdateAndRowGuard(t *testing.T) {
	org := uuid.New()
	on := true
	svc, mc := enabledSettingSvc(t, func(context.Context, uuid.UUID, string, string) (bool, error) { return on, nil })
	ctx := context.Background()
	u := newUser(org)
	on = false
	rec, err := svc.Create(ctx, "test_products", u, map[string]any{"name": "a", "price": 10.0})
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.MustParse(rec["id"].(string))
	on = true
	_, err = svc.Update(ctx, "test_products", u, id, map[string]any{"price": 500.0})
	var ar *ApprovalRequiredError
	if !errors.As(err, &ar) {
		t.Fatalf("update on: %v, want approval_required", err)
	}
	on = false
	if _, err := svc.Update(ctx, "test_products", u, id, map[string]any{"price": 500.0}); err != nil {
		t.Fatalf("update off: %v", err)
	}

	// Camino wasm (data_mutate): EvalRowConstraints sin resolver = apagado;
	// EvalRowConstraintsFor con resolver encendido dispara.
	row := map[string]any{"price": 500.0}
	if err := EvalRowConstraints(mc, row); err != nil {
		t.Fatalf("unresolved guard must be OFF: %v", err)
	}
	yes := func(context.Context, uuid.UUID, string, string) (bool, error) { return true, nil }
	var ce *ConstraintError
	if err := EvalRowConstraintsFor(ctx, org, mc, row, yes); !errors.As(err, &ce) || !ce.RequestsApproval() {
		t.Fatalf("resolved guard must fire: %v", err)
	}
	if err := EvalRowConstraintsFor(ctx, uuid.Nil, mc, row, yes); err != nil {
		t.Fatalf("nil org must be OFF: %v", err)
	}
	// El mc original no se muta.
	if mc.Constraints[0].EnabledSetting != "enforce_cap" {
		t.Fatal("ResolveEnabledGuards mutated its input")
	}
}

func TestEnabledSetting_ManifestValidation(t *testing.T) {
	cases := map[string]string{
		"missing":  strings.Replace(enabledSettingManifest, `"enabled_setting": "enforce_cap"`, `"enabled_setting": "nope"`, 1),
		"notbool":  strings.Replace(enabledSettingManifest, `{"key": "enforce_cap", "type": "boolean"}`, `{"key": "enforce_cap", "type": "string"}`, 1),
		"branch":   strings.Replace(enabledSettingManifest, `{"key": "enforce_cap", "type": "boolean"}`, `{"key": "enforce_cap", "type": "boolean", "scope": "branch"}`, 1),
		"defaulttrue": strings.Replace(enabledSettingManifest, `{"key": "enforce_cap", "type": "boolean"}`, `{"key": "enforce_cap", "type": "boolean", "default": true}`, 1),
		"notguard": strings.Replace(strings.Replace(enabledSettingManifest, `"kind": "request_approval"`, `"kind": "flag"`, 1), `"approval": {"roles": ["manager"]}`, `"severity": "info"`, 1),
	}
	for name, src := range cases {
		_, err := v3.Parse([]byte(src))
		if err == nil || !strings.Contains(err.Error(), "enabled_setting") {
			t.Errorf("%s: want enabled_setting validation error, got %v", name, err)
		}
	}
	if _, err := v3.Parse([]byte(enabledSettingManifest)); err != nil {
		t.Fatalf("valid manifest rejected: %v", err)
	}
}

// Dos guards que comparten el setting: el resolver corre UNA vez por escritura.
func TestEnabledSetting_SharedSettingResolvedOnce(t *testing.T) {
	g := func(key string) manifest.ConstraintDef {
		return manifest.ConstraintDef{Expr: "price > 100", ErrorKey: key, Invert: true, OnViolation: "request_approval",
			Approval: &manifest.ApprovalDef{Roles: []string{"manager"}}, EnabledSetting: "enforce_cap", AddonKey: "pricing"}
	}
	mc := &ModelConstraints{Constraints: []manifest.ConstraintDef{g("rule:a"), g("rule:b")}}
	var calls int
	out := ResolveEnabledGuards(context.Background(), uuid.New(), mc, func(context.Context, uuid.UUID, string, string) (bool, error) {
		calls++
		return true, nil
	})
	if calls != 1 || len(out.Constraints) != 2 {
		t.Fatalf("calls=%d guards=%d, want 1 call and 2 guards", calls, len(out.Constraints))
	}
}

// Aprobar con el setting APAGADO entre la solicitud y la decision: el replay
// aplica la escritura ya aprobada (el guard simplemente no esta; no hay nada
// que saltar). Comportamiento documentado en el CHANGELOG.
func TestEnabledSetting_ApproveAfterSettingTurnedOff(t *testing.T) {
	org := uuid.New()
	on := true
	svc, _ := enabledSettingSvc(t, func(context.Context, uuid.UUID, string, string) (bool, error) { return on, nil })
	ctx := context.Background()
	u := newUser(org)
	_, err := svc.Create(ctx, "test_products", u, map[string]any{"name": "tardio", "price": 150.0})
	var ar *ApprovalRequiredError
	if !errors.As(err, &ar) {
		t.Fatalf("create: %v", err)
	}
	on = false
	if _, err := svc.ApproveRequest(ctx, u, ar.Request.ID, ""); err != nil {
		t.Fatalf("approve with setting off: %v", err)
	}
	var n int64
	svc.db.Raw("SELECT COUNT(*) FROM test_products WHERE name = 'tardio'").Scan(&n)
	if n != 1 {
		t.Fatalf("approved write must apply exactly once, got %d", n)
	}
}
