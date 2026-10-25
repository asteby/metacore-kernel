package wasm

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/security"
	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func consumeEnv(t *testing.T, raw []byte) (ok bool, code string, data map[string]any) {
	t.Helper()
	var e struct {
		Success bool           `json:"success"`
		Data    map[string]any `json:"data"`
		Error   *dbqError      `json:"error"`
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatalf("envelope: %v -- %s", err, raw)
	}
	if e.Error != nil {
		code = e.Error.Code
	}
	return e.Success, code, e.Data
}

func TestExecuteApprovalConsume_Unavailable(t *testing.T) {
	inv := &invocation{addonKey: "customers", orgID: uuid.New()}
	ok, code, _ := consumeEnv(t, executeApprovalConsume(context.Background(), inv, []byte(`{"approval_id":"`+uuid.NewString()+`","policy":"p"}`)))
	if ok || code != "approvals_unavailable" {
		t.Fatalf("ok=%v code=%q", ok, code)
	}
}

func TestExecuteApprovalConsume_Validates(t *testing.T) {
	inv := &invocation{addonKey: "customers", orgID: uuid.New(),
		approvalConsumer: func(context.Context, dynamic.ConsumeInput) error { t.Fatal("no debe llamarse"); return nil }}
	ctx := dynamic.WithActorID(context.Background(), uuid.NewString())
	for name, body := range map[string]string{
		"vacio":      ``,
		"sin json":   `{`,
		"sin id":     `{"policy":"p"}`,
		"id no uuid": `{"approval_id":"x","policy":"p"}`,
		"sin policy": `{"approval_id":"` + uuid.NewString() + `"}`,
	} {
		if ok, code, _ := consumeEnv(t, executeApprovalConsume(ctx, inv, []byte(body))); ok || code != "invalid_request" {
			t.Errorf("%s: ok=%v code=%q", name, ok, code)
		}
	}
	// Without an acting user nothing is redeemable.
	body := `{"approval_id":"` + uuid.NewString() + `","policy":"p"}`
	if ok, code, _ := consumeEnv(t, executeApprovalConsume(context.Background(), inv, []byte(body))); ok || code != "invalid_request" {
		t.Errorf("sin usuario: ok=%v code=%q", ok, code)
	}
}

func TestExecuteApprovalConsume_PassesHostScopeNotGuest(t *testing.T) {
	org, actor, grant := uuid.New(), uuid.New(), uuid.New()
	var got dynamic.ConsumeInput
	inv := &invocation{addonKey: "customers", orgID: org,
		approvalConsumer: func(_ context.Context, in dynamic.ConsumeInput) error { got = in; return nil }}
	ctx := dynamic.WithActorID(context.Background(), actor.String())
	// organization_id / requested_by in the guest body are ignored.
	body := fmt.Sprintf(`{"approval_id":%q,"policy":"price_below_min","model":"sales_items","record_id":"r1","organization_id":%q,"requested_by":%q,
		"context_eq":{"product_id":"p1"},"context_gte":{"unit_shortfall":5}}`, grant, uuid.NewString(), uuid.NewString())
	ok, _, data := consumeEnv(t, executeApprovalConsume(ctx, inv, []byte(body)))
	if !ok || data["consumed"] != true {
		t.Fatalf("ok=%v data=%v", ok, data)
	}
	if got.OrgID != org || got.RequestedBy != actor || got.ApprovalID != grant || got.Policy != "price_below_min" ||
		got.ModelKey != "sales_items" || got.RecordID != "r1" || got.AddonKey != "customers" || got.ContextEq["product_id"] != "p1" || got.ContextGte["unit_shortfall"] != 5 {
		t.Fatalf("input = %+v", got)
	}
}

func TestExecuteApprovalConsume_MapsRefusals(t *testing.T) {
	ctx := dynamic.WithActorID(context.Background(), uuid.NewString())
	body := []byte(`{"approval_id":"` + uuid.NewString() + `","policy":"p","model":"sales_items"}`)
	refused := &invocation{orgID: uuid.New(), approvalConsumer: func(context.Context, dynamic.ConsumeInput) error {
		return fmt.Errorf("%w: already used", dynamic.ErrApprovalConsume)
	}}
	if _, code, _ := consumeEnv(t, executeApprovalConsume(ctx, refused, body)); code != "approval_invalid" {
		t.Fatalf("code = %q", code)
	}
	broken := &invocation{orgID: uuid.New(), approvalConsumer: func(context.Context, dynamic.ConsumeInput) error {
		return fmt.Errorf("db down")
	}}
	if _, code, _ := consumeEnv(t, executeApprovalConsume(ctx, broken, body)); code != "db_error" {
		t.Fatalf("code = %q", code)
	}
}

func TestExecuteApprovalConsume_RequiresModel(t *testing.T) {
	inv := &invocation{addonKey: "customers", orgID: uuid.New(),
		approvalConsumer: func(context.Context, dynamic.ConsumeInput) error { t.Fatal("no debe llamarse"); return nil }}
	ctx := dynamic.WithActorID(context.Background(), uuid.NewString())
	body := []byte(`{"approval_id":"` + uuid.NewString() + `","policy":"p"}`)
	if ok, code, _ := consumeEnv(t, executeApprovalConsume(ctx, inv, body)); ok || code != "invalid_request" {
		t.Fatalf("ok=%v code=%q", ok, code)
	}
}

func TestExecuteApprovalConsume_CapabilityGate(t *testing.T) {
	enf := security.NewEnforcer(func(string) *security.Capabilities {
		return security.Compile("customers", []manifest.Capability{{Kind: "db:write", Target: "sales_items"}})
	})
	enf.SetMode(security.ModeEnforce)
	called := false
	inv := &invocation{addonKey: "customers", orgID: uuid.New(), enforcer: enf,
		approvalConsumer: func(context.Context, dynamic.ConsumeInput) error { called = true; return nil }}
	ctx := dynamic.WithActorID(context.Background(), uuid.NewString())
	mk := func(model string) []byte {
		return []byte(`{"approval_id":"` + uuid.NewString() + `","policy":"p","model":"` + model + `","context_eq":{"a":"b"}}`)
	}
	if ok, code, _ := consumeEnv(t, executeApprovalConsume(ctx, inv, mk("ledger"))); ok || code != "forbidden" || called {
		t.Fatalf("sin db:write: ok=%v code=%q called=%v", ok, code, called)
	}
	if ok, _, _ := consumeEnv(t, executeApprovalConsume(ctx, inv, mk("sales_items"))); !ok || !called {
		t.Fatalf("con db:write debe canjear")
	}
}

// Under InvokeInTx the redemption rides the invocation's transaction.
func TestExecuteApprovalConsume_UsesInvocationTx(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:consume_tx_"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	tx := db.Begin() // a real transaction: isTx() must say so
	defer tx.Rollback()
	var got *gorm.DB
	f := func(_ context.Context, in dynamic.ConsumeInput) error { got = in.DB; return nil }
	ctx := dynamic.WithActorID(context.Background(), uuid.NewString())
	body := []byte(`{"approval_id":"` + uuid.NewString() + `","policy":"p","model":"m","context_eq":{"a":"b"}}`)
	executeApprovalConsume(ctx, &invocation{addonKey: "a", orgID: uuid.New(), db: db, tx: tx, approvalConsumer: f}, body)
	if got != tx {
		t.Fatalf("debe usar el tx de la invocación")
	}
	executeApprovalConsume(ctx, &invocation{addonKey: "a", orgID: uuid.New(), db: db, approvalConsumer: f}, body)
	if got != db {
		t.Fatalf("sin tx usa la conexión")
	}
	// A pool handle in inv.tx (Native actions) is not a transaction.
	executeApprovalConsume(ctx, &invocation{addonKey: "a", orgID: uuid.New(), db: db, tx: &gorm.DB{}, approvalConsumer: f}, body)
	if got != db {
		t.Fatalf("un handle que no es tx cae a la conexión")
	}
}
