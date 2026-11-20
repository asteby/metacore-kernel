package dynamic

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
	"github.com/asteby/metacore-kernel/metadata"
	"github.com/asteby/metacore-kernel/modelbase"
)

func rowCondService(t *testing.T) *Service {
	t.Helper()
	db := setupTestDB(t)
	modelbase.Register("test_products", func() modelbase.ModelDefiner { return &TestProduct{} })
	return New(Config{
		AllowUnconfiguredPermissions: true,
		DB:                           db,
		Metadata:                     metadata.New(metadata.Config{CacheTTL: -1}),
		RowConditionResolver: func(_ context.Context, model string) (*v3.RowConditions, bool) {
			if model != "test_products" {
				return nil, false
			}
			return &v3.RowConditions{
				// A default row is never edited; expensive rows are not editable either.
				Update: &v3.RowGate{
					RowCondition: v3.RowCondition{Operator: "all", Conditions: []v3.RowCondition{
						{Field: "name", Operator: "neq", Value: "Default"},
						{Field: "price", Operator: "lte", Value: 100.0},
					}},
					ErrorKey: "products.locked",
				},
				// Only free rows can be deleted.
				Delete: &v3.RowGate{RowCondition: v3.RowCondition{Field: "price", Operator: "lte", Value: 0.0}},
			}, true
		},
	})
}

func TestRowConditions_UpdateAndDeleteEnforcedOnPersistedRow(t *testing.T) {
	svc := rowCondService(t)
	ctx := context.Background()
	u := newUser(uuid.New())

	cheap := createProduct(t, svc, u, "Widget", 10)
	dear := createProduct(t, svc, u, "Gold", 500)
	def := createProduct(t, svc, u, "Default", 1)
	free := createProduct(t, svc, u, "Free", 0)
	id := func(r map[string]any) uuid.UUID { return uuid.MustParse(r["id"].(string)) }

	if _, err := svc.Update(ctx, "test_products", u, id(cheap), map[string]any{"name": "Widget 2"}); err != nil {
		t.Fatalf("update of a row that satisfies the gate: %v", err)
	}
	for name, r := range map[string]map[string]any{"price > 100": dear, "default row": def} {
		_, err := svc.Update(ctx, "test_products", u, id(r), map[string]any{"name": "x"})
		var rce *RowConditionError
		if !errors.As(err, &rce) || rce.ErrorKey != "products.locked" || rce.Op != "update" {
			t.Fatalf("%s: want RowConditionError(update), got %v", name, err)
		}
		if !errors.Is(err, ErrRowConditionFailed) {
			t.Fatalf("%s: must match ErrRowConditionFailed", name)
		}
	}
	// The gate reads the PERSISTED row, not the incoming input: sending a
	// "cheap" price in the same request does not unlock an expensive row.
	if _, err := svc.Update(ctx, "test_products", u, id(dear), map[string]any{"price": 1}); !errors.Is(err, ErrRowConditionFailed) {
		t.Fatalf("input must not bypass the gate, got %v", err)
	}

	if err := svc.Delete(ctx, "test_products", u, id(cheap)); !errors.Is(err, ErrRowConditionFailed) {
		t.Fatalf("delete of a priced row must be refused, got %v", err)
	}
	if err := svc.Delete(ctx, "test_products", u, id(free)); err != nil {
		t.Fatalf("delete of a row that satisfies the gate: %v", err)
	}
	// A system caller is never gated; a missing row is the operation's own 404.
	if err := svc.Delete(ctx, "test_products", NewSystemCaller(u.orgID), id(dear)); err != nil {
		t.Fatalf("system caller: %v", err)
	}
	if err := svc.Delete(ctx, "test_products", u, uuid.New()); errors.Is(err, ErrRowConditionFailed) {
		t.Fatalf("a missing row must not be reported as a gate failure: %v", err)
	}
}

// The action's data condition is enforced server-side, even though the SDK also
// hides the button.
func TestActionCondition_EnforcedByServer(t *testing.T) {
	fx := setupActionFixture(t)
	fx.registerAction("test_products", &manifest.ActionDef{
		Key:     "discount",
		Trigger: &manifest.ActionTrigger{Type: "wasm", Export: "discount"},
		Condition: &manifest.ConditionDef{
			Operator: "all",
			Conditions: []v3.RowCondition{
				{Field: "price", Operator: "gt", Value: 50.0},
				{Field: "name", Operator: "neq", Value: "Widget"},
			},
		},
	})
	fx.wasm.fn = func(context.Context, ActionRequest) (ActionResponse, error) {
		return ActionResponse{Success: true}, nil
	}

	// Fixture product is "Widget" at 9.99: the group does not hold.
	status, env := invokeAction(t, fx.app, "test_products", fx.productID, "discount", `{}`)
	if status != fiber.StatusConflict {
		t.Fatalf("status = %d, want 409 (%v)", status, env)
	}
	if env["code"] != "action_condition_not_met" {
		t.Fatalf("code = %v", env["code"])
	}
	if fx.wasm.calls != 0 {
		t.Fatal("the handler must not run when the condition fails")
	}

	// Make the row satisfy it: now it dispatches.
	if _, err := fx.svc.Update(context.Background(), "test_products", fx.user, fx.productID, map[string]any{"name": "Gadget", "price": 80}); err != nil {
		t.Fatal(err)
	}
	status, env = invokeAction(t, fx.app, "test_products", fx.productID, "discount", `{}`)
	if status != fiber.StatusOK || env["success"] != true {
		b, _ := json.Marshal(env)
		t.Fatalf("status = %d %s", status, b)
	}
	if fx.wasm.calls != 1 {
		t.Fatalf("calls = %d", fx.wasm.calls)
	}
}
