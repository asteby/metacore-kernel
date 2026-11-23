package dynamic

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Update con `_children` (inline_children_update.go): cada relación nombrada se
// sincroniza con las filas enviadas — con `id` actualiza, sin `id` crea, lo que
// falta se borra — y todo es atómico con el padre. Arnés de inline_children_test.go
// (IcRule/IcTier/IcNote/IcSecret y, de otro addon, IcProduct/IcPrice/IcGhost).

func icPriceByMarket(t *testing.T, db *gorm.DB, productID string) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, r := range icRows(t, db, "ic_prices", "product_id", productID) {
		out[fmt.Sprint(r["market"])] = r
	}
	return out
}

func icProductName(t *testing.T, db *gorm.DB, id string) string {
	t.Helper()
	rows := icRows(t, db, "ic_products", "id", id)
	if len(rows) != 1 {
		t.Fatalf("product rows = %d", len(rows))
	}
	return fmt.Sprint(rows[0]["name"])
}

func TestUpdate_InlineSyncsCreatesUpdatesAndDeletes(t *testing.T) {
	svc, db := newInlineService(t, true)
	user := newUser(uuid.New())
	id := uuid.NewString()
	if _, err := svc.Create(context.Background(), "IcProduct", user, map[string]any{
		"id": id, "name": "Creatina",
		"_children": map[string]any{"prices": []any{
			map[string]any{"market": "mx", "price": 1399.0},
			map[string]any{"market": "us", "price": 69.99},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	before := icPriceByMarket(t, db, id)
	mxID := fmt.Sprint(before["mx"]["id"])

	uid, _ := uuid.Parse(id)
	// mx cambia de precio, us desaparece de la lista (se borra), eu es nuevo y el
	// padre cambia de nombre en la misma llamada.
	if _, err := svc.Update(context.Background(), "IcProduct", user, uid, map[string]any{
		"name": "Creatina 1kg",
		"_children": map[string]any{"prices": []any{
			map[string]any{"id": mxID, "market": "mx", "price": 1499.0},
			map[string]any{"market": "eu", "price": 64.0},
		}},
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got := icProductName(t, db, id); got != "Creatina 1kg" {
		t.Fatalf("name = %q", got)
	}
	now := icPriceByMarket(t, db, id)
	if len(now) != 2 || now["mx"] == nil || now["eu"] == nil || now["us"] != nil {
		t.Fatalf("prices = %v (want mx updated, eu new, us removed)", now)
	}
	if fmt.Sprint(now["mx"]["id"]) != mxID || fmt.Sprint(now["mx"]["price"]) != "1499" {
		t.Fatalf("mx = %v", now["mx"])
	}
	if fmt.Sprint(now["eu"]["product_id"]) != id || fmt.Sprint(now["eu"]["organization_id"]) != user.orgID.String() {
		t.Fatalf("eu lost the runtime-owned fields: %v", now["eu"])
	}
}

// Una relación que no se nombra no se toca; una lista vacía borra todos los hijos.
func TestUpdate_InlineUntouchedWhenNotNamedAndEmptyListRemovesAll(t *testing.T) {
	svc, db := newInlineService(t, true)
	user := newUser(uuid.New())
	id := uuid.NewString()
	if _, err := svc.Create(context.Background(), "IcRule", user, map[string]any{
		"id": id, "name": "R",
		"_children": map[string]any{
			"tiers": []any{map[string]any{"min_amount": 1.0, "percent": 1.0}},
			"notes": []any{map[string]any{"body": "a"}, map[string]any{"body": "b"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	uid, _ := uuid.Parse(id)
	// Solo notes, vacía: tiers (min 1) NO se exige ni se toca.
	if _, err := svc.Update(context.Background(), "IcRule", user, uid, map[string]any{
		"name": "R2", "_children": map[string]any{"notes": []any{}},
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if n := len(icRows(t, db, "ic_tiers", "rule_id", id)); n != 1 {
		t.Fatalf("tiers = %d, want 1 (not named: untouched)", n)
	}
	if n := len(icRows(t, db, "ic_notes", "owner_id", id)); n != 0 {
		t.Fatalf("notes = %d, want 0 (empty list removes all)", n)
	}
}

// Lo que se puede rechazar se rechaza antes de escribir el padre.
func TestUpdate_InlineRefusedBeforeWritingTheParent(t *testing.T) {
	svc, db := newInlineService(t, true)
	user := newUser(uuid.New())
	id := uuid.NewString()
	if _, err := svc.Create(context.Background(), "IcRule", user, map[string]any{
		"id": id, "name": "R",
		"_children": map[string]any{"tiers": []any{map[string]any{"min_amount": 1.0, "percent": 1.0}}},
	}); err != nil {
		t.Fatal(err)
	}
	uid, _ := uuid.Parse(id)

	// id desconocido
	_, err := svc.Update(context.Background(), "IcRule", user, uid, map[string]any{
		"name": "X", "_children": map[string]any{"tiers": []any{map[string]any{"id": uuid.NewString(), "percent": 2.0}}},
	})
	if f := icValidation(t, err).Fields["_children.tiers.0.id"]; len(f) != 1 || f[0].Code != "not_found" {
		t.Fatalf("unknown id: %v", err)
	}
	// min: vaciar tiers (min 1)
	_, err = svc.Update(context.Background(), "IcRule", user, uid, map[string]any{
		"name": "X", "_children": map[string]any{"tiers": []any{}},
	})
	if f := icValidation(t, err).Fields["_children.tiers"]; len(f) != 1 || f[0].Code != CodeInlineMin {
		t.Fatalf("min: %v", err)
	}
	// relación no declarada
	_, err = svc.Update(context.Background(), "IcRule", user, uid, map[string]any{
		"name": "X", "_children": map[string]any{"audits": []any{}},
	})
	if f := icValidation(t, err).Fields["_children.audits"]; len(f) != 1 || f[0].Code != CodeInlineUnknownRelation {
		t.Fatalf("undeclared: %v", err)
	}
	// permiso: crear en un hijo vedado
	_, err = svc.Update(context.Background(), "IcRule", user, uid, map[string]any{
		"name": "X", "_children": map[string]any{"secrets": []any{map[string]any{"body": "x"}}},
	})
	var ade *AccessDeniedError
	if !errors.As(err, &ade) || ade.Model != "IcSecret" {
		t.Fatalf("forbidden child: %v", err)
	}
	rows := icRows(t, db, "ic_rules", "id", id)
	if len(rows) != 1 || fmt.Sprint(rows[0]["name"]) != "R" {
		t.Fatalf("the parent was written although the children were refused: %v", rows)
	}
	if n := len(icRows(t, db, "ic_tiers", "rule_id", id)); n != 1 {
		t.Fatalf("tiers = %d, want the original 1", n)
	}
}

// El addon dueño del hijo no está instalado: error claro y el padre intacto.
func TestUpdate_InlineChildOfMissingAddonIsRefused(t *testing.T) {
	svc, db := newInlineService(t, true)
	user := newUser(uuid.New())
	id := uuid.NewString()
	if _, err := svc.Create(context.Background(), "IcProduct", user, map[string]any{"id": id, "name": "P"}); err != nil {
		t.Fatal(err)
	}
	uid, _ := uuid.Parse(id)
	_, err := svc.Update(context.Background(), "IcProduct", user, uid, map[string]any{
		"name": "P2", "_children": map[string]any{"ghosts": []any{map[string]any{"x": 1.0}}},
	})
	f := icValidation(t, err).Fields["_children.ghosts"]
	if len(f) != 1 || f[0].Code != CodeInlineChildUnavailable || f[0].Params["addon"] != "ghostaddon" {
		t.Fatalf("errors = %v", err)
	}
	if got := icProductName(t, db, id); got != "P" {
		t.Fatalf("name = %q, the parent must not be written", got)
	}
}

// Si un hijo falla al escribir, todo vuelve a como estaba: hijos creados,
// actualizados y borrados, y el padre.
func TestUpdate_InlineFailureRestoresChildrenAndParent(t *testing.T) {
	hooks := NewHookRegistry()
	hooks.RegisterBeforeCreate("IcPrice", func(_ context.Context, _ HookContext, in map[string]any) error {
		if in["market"] == "boom" {
			return NewValidationError().AddMessage("market", "Mercado no válido").Err()
		}
		return nil
	})
	svc, db := newInlineService(t, true, hooks)
	user := newUser(uuid.New())
	id := uuid.NewString()
	if _, err := svc.Create(context.Background(), "IcProduct", user, map[string]any{
		"id": id, "name": "Creatina",
		"_children": map[string]any{"prices": []any{
			map[string]any{"market": "mx", "price": 1399.0},
			map[string]any{"market": "us", "price": 69.99},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	before := icPriceByMarket(t, db, id)
	uid, _ := uuid.Parse(id)

	// mx se actualiza, us se borra, eu se crea y el último create falla.
	_, err := svc.Update(context.Background(), "IcProduct", user, uid, map[string]any{
		"name": "Otro nombre",
		"_children": map[string]any{"prices": []any{
			map[string]any{"id": fmt.Sprint(before["mx"]["id"]), "market": "mx", "price": 1.0},
			map[string]any{"market": "eu", "price": 64.0},
			map[string]any{"market": "boom", "price": 5.0},
		}},
	})
	ve := icValidation(t, err)
	if f := ve.Fields["_children.prices.2.market"]; len(f) != 1 {
		t.Fatalf("errors = %+v", ve.Fields)
	}
	if got := icProductName(t, db, id); got != "Creatina" {
		t.Fatalf("parent name = %q, want it restored", got)
	}
	after := icPriceByMarket(t, db, id)
	if len(after) != 2 || after["eu"] != nil || after["us"] == nil || after["mx"] == nil {
		t.Fatalf("prices = %v, want the original mx and us only", after)
	}
	if fmt.Sprint(after["mx"]["price"]) != "1399" {
		t.Fatalf("mx price = %v, want it restored to 1399", after["mx"]["price"])
	}
}
