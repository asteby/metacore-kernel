package wasm

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/security"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

func ctxInv(org uuid.UUID, caps ...string) *invocation {
	var cs []manifest.Capability
	for _, k := range caps {
		cs = append(cs, manifest.Capability{Kind: k, Target: "*"})
	}
	rate, incl := 16.0, true
	return &invocation{
		addonKey: "inventory",
		orgID:    org,
		caps:     security.Compile("inventory", cs),
		ctxProvider: func(_ context.Context, _, _ uuid.UUID, _ HostContextScopes) (*HostContext, error) {
			return &HostContext{
				UserEmail: "ana@example.com",
				Roles:     []string{"warehouse", "admin"},
				Org:       OrgConfig{CurrencyCode: "MXN", TaxRate: &rate, TaxIncluded: &incl, Locale: "es-MX", Timezone: "America/Mexico_City"},
			}, nil
		},
	}
}

func ctxDecode(t *testing.T, raw []byte) (bool, map[string]any, string) {
	t.Helper()
	var env struct {
		Success bool           `json:"success"`
		Data    map[string]any `json:"data"`
		Error   struct{ Code string }
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("bad envelope %s: %v", raw, err)
	}
	return env.Success, env.Data, env.Error.Code
}

func TestCtxGet_UserAndOrgConfig(t *testing.T) {
	org, user := uuid.New(), uuid.New()
	ctx := dynamic.WithActorID(context.Background(), user.String())
	ok, data, _ := ctxDecode(t, executeCtxGet(ctx, ctxInv(org, "ctx:user", "ctx:org_config"), nil))
	if !ok {
		t.Fatal("expected success")
	}
	if data["org_id"] != org.String() || data["user_id"] != user.String() || data["user_email"] != "ana@example.com" {
		t.Fatalf("user slice wrong: %v", data)
	}
	orgCfg := data["org"].(map[string]any)
	if orgCfg["currency_code"] != "MXN" || orgCfg["tax_rate"] != 16.0 || orgCfg["tax_included"] != true {
		t.Fatalf("org slice wrong: %v", orgCfg)
	}
	if _, leaked := data["roles"]; leaked {
		t.Fatal("roles must not appear without ctx:roles")
	}
}

func TestCtxGet_UserScopeCarriesActiveBranch(t *testing.T) {
	org, user, branch := uuid.New(), uuid.New(), uuid.New()
	ctx := dynamic.WithBranchID(dynamic.WithActorID(context.Background(), user.String()), branch.String())
	ok, data, _ := ctxDecode(t, executeCtxGet(ctx, ctxInv(org, "ctx:user"), nil))
	if !ok || data["branch_id"] != branch.String() {
		t.Fatalf("expected branch_id %s, got %v", branch, data)
	}

	// No branch in the invocation: the key is present and null, not invented.
	ok, data, _ = ctxDecode(t, executeCtxGet(dynamic.WithActorID(context.Background(), user.String()), ctxInv(org, "ctx:user"), nil))
	if v, present := data["branch_id"]; !ok || !present || v != nil {
		t.Fatalf("expected branch_id null, got %v", data)
	}
}

func TestCtxGet_NoCapabilityYieldsOnlyOrgID(t *testing.T) {
	org := uuid.New()
	ok, data, _ := ctxDecode(t, executeCtxGet(context.Background(), ctxInv(org), nil))
	if !ok || len(data) != 1 || data["org_id"] != org.String() {
		t.Fatalf("undeclared addon must see only org_id, got %v", data)
	}
}

func TestCtxGet_ExplicitUngrantedScopeIsForbidden(t *testing.T) {
	ok, _, code := ctxDecode(t, executeCtxGet(context.Background(), ctxInv(uuid.New(), "ctx:org_config"), []byte(`{"scopes":["user"]}`)))
	if ok || code != "forbidden" {
		t.Fatalf("want forbidden, got ok=%v code=%s", ok, code)
	}
	ok, _, code = ctxDecode(t, executeCtxGet(context.Background(), ctxInv(uuid.New(), "ctx:user"), []byte(`{"scopes":["secrets"]}`)))
	if ok || code != "invalid_request" {
		t.Fatalf("unknown scope: got ok=%v code=%s", ok, code)
	}
}

func TestCtxGet_SystemInvocationHasNullUserAndSortedRoles(t *testing.T) {
	_, data, _ := ctxDecode(t, executeCtxGet(context.Background(), ctxInv(uuid.New(), "ctx:user", "ctx:roles"), nil))
	if v, present := data["user_id"]; !present || v != nil {
		t.Fatalf("no actor must give user_id null, got %v", v)
	}
	roles := data["roles"].([]any)
	if len(roles) != 2 || roles[0] != "admin" {
		t.Fatalf("roles not sorted: %v", roles)
	}
}

func TestCtxGet_NoProviderAndNoOrg(t *testing.T) {
	inv := ctxInv(uuid.New(), "ctx:user")
	inv.ctxProvider = nil
	if _, _, code := ctxDecode(t, executeCtxGet(context.Background(), inv, nil)); code != "context_unavailable" {
		t.Fatalf("code = %s", code)
	}
	if _, _, code := ctxDecode(t, executeCtxGet(context.Background(), ctxInv(uuid.Nil, "ctx:user"), nil)); code != "no_active_org" {
		t.Fatalf("code = %s", code)
	}
}

// data_mutate create must run the embedder's sequence stamper on the create's
// own transaction and INSERT the stamped folio; an explicit column is the
// stamper's business (kernel-side test in dynamic/), a non-create is untouched.
func TestDataMutate_CreateStampsDeclaredSequences(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()
	org := uuid.New()
	rowID := uuid.NewString()
	bus, _, _ := captureBus(t, "inventory.WorkOrder.created")

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT \* FROM "work_orders" LIMIT 0`).WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id", "folio"}))
	mock.ExpectQuery(`INSERT INTO "work_orders" \("created_at", "folio", "id", "organization_id", "updated_at"\)`).
		WithArgs(sqlmock.AnyArg(), "OT-000001", rowID, org, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "folio"}).AddRow(rowID, "OT-000001"))
	mock.ExpectCommit()

	calls := 0
	inv := testInvocation(gdb, bus, org, nil, nil)
	inv.sequenceStamp = func(_ context.Context, tx *gorm.DB, o uuid.UUID, model string, row map[string]any) error {
		calls++
		if tx == nil || o != org || model != "WorkOrder" {
			t.Errorf("stamper got tx=%v org=%v model=%q", tx, o, model)
		}
		row["folio"] = "OT-000001"
		return nil
	}
	out := executeDataMutate(context.Background(), inv, []byte(`{"op":"create","table":"work_orders","model":"WorkOrder","id":"`+rowID+`","data":{}}`))
	if env := unmarshalMutate(t, out); !env.Success {
		t.Fatalf("expected success, got %s", out)
	}
	if calls != 1 {
		t.Fatalf("stamper calls = %d, want 1", calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// ctxGetWasm: imports metacore_host.ctx_get and exports ctx_test(ptr,len)->i64
// that forwards its (ptr,len) request straight to the import, returning the
// host's packed envelope. Same skeleton as eventEmitWasm.
func ctxGetWasm() []byte {
	buf := []byte{0x00, 0x61, 0x73, 0x6D, 0x01, 0x00, 0x00, 0x00}
	buf = append(buf, section(0x01, []byte{
		0x02,
		0x60, 0x01, 0x7F, 0x01, 0x7F, // (i32) -> i32
		0x60, 0x02, 0x7F, 0x7F, 0x01, 0x7E, // (i32,i32) -> i64
	})...)
	var imports []byte
	imports = append(imports, 0x01)
	imports = append(imports, encodeName("metacore_host")...)
	imports = append(imports, encodeName("ctx_get")...)
	imports = append(imports, 0x00, 0x01)
	buf = append(buf, section(0x02, imports)...)
	buf = append(buf, section(0x03, []byte{0x02, 0x00, 0x01})...)
	buf = append(buf, section(0x05, []byte{0x01, 0x00, 0x01})...)
	globals := []byte{0x01, 0x7F, 0x01, 0x41}
	globals = append(globals, encodeSLEB128(1024)...)
	globals = append(globals, 0x0B)
	buf = append(buf, section(0x06, globals)...)
	var exports []byte
	exports = append(exports, 0x03)
	exports = append(exports, encodeName("memory")...)
	exports = append(exports, 0x02, 0x00)
	exports = append(exports, encodeName("alloc")...)
	exports = append(exports, 0x00, 0x01)
	exports = append(exports, encodeName("ctx_test")...)
	exports = append(exports, 0x00, 0x02)
	buf = append(buf, section(0x07, exports)...)
	allocBody := withSize([]byte{0x01, 0x01, 0x7F, 0x23, 0x00, 0x22, 0x01, 0x20, 0x00, 0x6A, 0x24, 0x00, 0x20, 0x01, 0x0B})
	ctxBody := withSize([]byte{0x00, 0x20, 0x00, 0x20, 0x01, 0x10, 0x00, 0x0B})
	code := []byte{0x02}
	code = append(code, allocBody...)
	code = append(code, ctxBody...)
	buf = append(buf, section(0x0A, code)...)
	return buf
}

// End to end through a real wasm guest: manifest capabilities compiled into the
// Host's per-addon policy, actor id on the context, provider port wired.
func TestHost_InvokeCtxGet_RealGuest(t *testing.T) {
	ctx := context.Background()
	h, err := NewHost(ctx, security.Compile("", nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close(ctx)
	h.WithAddonCaps(func(k string) *security.Capabilities {
		return security.Compile(k, []manifest.Capability{
			{Kind: "ctx:user", Target: "*"}, {Kind: "ctx:org_config", Target: "*"},
		})
	})
	h.WithContextProvider(ctxInv(uuid.New()).ctxProvider)
	spec := &manifest.BackendSpec{Runtime: "wasm", Entry: "b.wasm", Exports: []string{"ctx_test"}, MemoryLimitMB: 4, TimeoutMs: 2000}
	if err := h.Load(ctx, "inventory", ctxGetWasm(), spec); err != nil {
		t.Fatal(err)
	}
	org, user := uuid.New(), uuid.New()
	callCtx := dynamic.WithActorID(ctx, user.String())
	out, err := h.InvokeFor(callCtx, org, uuid.New(), "inventory", "ctx_test", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ok, data, code := ctxDecode(t, out)
	if !ok {
		t.Fatalf("ctx_get failed: %s (%s)", out, code)
	}
	if data["org_id"] != org.String() || data["user_id"] != user.String() {
		t.Fatalf("guest saw wrong identity: %v", data)
	}
	if data["org"].(map[string]any)["currency_code"] != "MXN" {
		t.Fatalf("org config missing: %v", data)
	}
	if _, leaked := data["roles"]; leaked {
		t.Fatal("roles leaked without ctx:roles")
	}
}

// A create the embedder's CreateCheckFn rejects with dynamic.ErrDeletedRef is
// rolled back before the INSERT and reported as invalid_reference (QA
// VEN-N08: a guest created a sale line for a soft-deleted product).
func TestDataMutate_CreateCheckRejectsDeletedRef(t *testing.T) {
	gdb, mock, cleanup := newMockGorm(t)
	defer cleanup()
	org := uuid.New()
	bus, _, _ := captureBus(t, "customers.SalesOrderItem.created")

	mock.ExpectBegin()
	mock.ExpectRollback() // rejected before any INSERT

	inv := testInvocation(gdb, bus, org, nil, nil)
	inv.createCheck = func(_ context.Context, _ *gorm.DB, o uuid.UUID, model, table string, row map[string]any) error {
		if model != "SalesOrderItem" || table != "sales_order_items" || row["product_id"] != "p-deleted" {
			t.Errorf("check got model=%q table=%q row=%v", model, table, row)
		}
		return fmt.Errorf("%w: product_id → products", dynamic.ErrDeletedRef)
	}
	out := executeDataMutate(context.Background(), inv, []byte(`{"op":"create","table":"sales_order_items","model":"SalesOrderItem","data":{"product_id":"p-deleted"}}`))
	env := unmarshalMutate(t, out)
	if env.Success || env.Error == nil || env.Error.Code != "invalid_reference" {
		t.Fatalf("expected invalid_reference, got %s", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
