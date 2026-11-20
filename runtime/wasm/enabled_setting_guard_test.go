package wasm

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/manifest"
)

// rules[].enabled_setting end to end through data_mutate: the guard ctx carries
// the org (OrgIDFromContext) and two orgs with a different setting behave
// differently.
func TestExecuteDataMutate_EnabledSettingPerOrg(t *testing.T) {
	orgOn, orgOff := uuid.New(), uuid.New()
	mc := &dynamic.ModelConstraints{Constraints: []manifest.ConstraintDef{{
		Expr: "quantity > 100", ErrorKey: "rule:cap", Invert: true,
		OnViolation: "request_approval", Approval: &manifest.ApprovalDef{Roles: []string{"manager"}},
		EnabledSetting: "enforce_cap", AddonKey: "stockcap",
	}}}
	var seenOrg uuid.UUID
	resolver := func(_ context.Context, org uuid.UUID, addon, key string) (bool, error) {
		if addon != "stockcap" || key != "enforce_cap" {
			t.Fatalf("resolver args %s/%s", addon, key)
		}
		return org == orgOn, nil
	}

	run := func(org uuid.UUID) envelopeForTest {
		gdb, mock, cleanup := newMockGorm(t)
		defer cleanup()
		rowID := uuid.NewString()
		bus, _, _ := captureBus(t, "inventory.Stock.created")
		mock.ExpectBegin()
		mock.ExpectQuery(`SELECT \* FROM "stock" LIMIT 0`).WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id"}))
		mock.ExpectQuery(`INSERT INTO "stock" .* RETURNING \*`).
			WillReturnRows(sqlmock.NewRows([]string{"id", "organization_id", "quantity"}).AddRow(rowID, org.String(), int64(500)))
		if org == orgOn {
			mock.ExpectRollback()
		} else {
			mock.ExpectCommit()
		}
		inv := testInvocation(gdb, bus, org, stockWriteEnforcer(), nil)
		inv.mutationGuard = func(ctx context.Context, _ string, row map[string]any) error {
			seenOrg = OrgIDFromContext(ctx)
			return dynamic.EvalRowConstraintsFor(ctx, OrgIDFromContext(ctx), mc, row, resolver)
		}
		out := executeDataMutate(context.Background(), inv, []byte(`{"op":"create","table":"stock","model":"Stock","id":"`+rowID+`","data":{"quantity":500}}`))
		env := unmarshalMutate(t, out)
		if seenOrg != org {
			t.Fatalf("guard ctx org = %s, want %s", seenOrg, org)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("expectations: %v", err)
		}
		return envelopeForTest{ok: env.Success, code: func() string {
			if env.Error != nil {
				return env.Error.Code
			}
			return ""
		}()}
	}

	if r := run(orgOn); r.ok {
		t.Fatalf("org with setting ON must be gated, got success")
	} else if r.code == "" {
		t.Fatal("expected an error code")
	}
	if r := run(orgOff); !r.ok {
		t.Fatalf("org with setting OFF must pass, got code %q", r.code)
	}
}

type envelopeForTest struct {
	ok   bool
	code string
}
