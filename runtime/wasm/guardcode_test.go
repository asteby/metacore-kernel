package wasm

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/manifest"
)

func TestGuardErrCode(t *testing.T) {
	plain := &dynamic.ConstraintError{ErrorKey: "k", Def: manifest.ConstraintDef{Expr: "a >= 0"}}
	approvable := &dynamic.ConstraintError{ErrorKey: "k", Def: manifest.ConstraintDef{
		Expr: "a >= 0", OnViolation: "request_approval", Approval: &manifest.ApprovalDef{Roles: []string{"manager"}},
	}}
	if got := guardErrCode(plain); got != "constraint_violation" {
		t.Errorf("plain = %q", got)
	}
	if got := guardErrCode(fmt.Errorf("wrapped: %w", approvable)); got != "approval_required" {
		t.Errorf("approvable = %q", got)
	}
	if got := guardErrCode(fmt.Errorf("db down")); got != "constraint_violation" {
		t.Errorf("other = %q", got)
	}
}

// P5: el sobre approval_required lleva la politica y el contexto de la regla.
func TestWithApprovalDetail(t *testing.T) {
	ce := &dynamic.ConstraintError{ErrorKey: "rule:price_cap", Expr: "price > 100",
		Def: manifest.ConstraintDef{Expr: "price > 100", OnViolation: "request_approval",
			Approval: &manifest.ApprovalDef{Roles: []string{"manager"}, Label: "pricing.over_cap"}},
		Values: map[string]any{"price": 150.0}}
	env := dataMutateErr("a", "approval_required", "x", [16]byte{}, time.Now(), "")
	out := withApprovalDetail(env, fmt.Errorf("wrapped: %w", ce))
	var doc struct {
		Error struct {
			Code   string         `json:"code"`
			Policy map[string]any `json:"policy"`
			Ctx    map[string]any `json:"context"`
		} `json:"error"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Error.Code != "approval_required" || doc.Error.Policy["key"] != "rule:price_cap" || doc.Error.Policy["label"] != "pricing.over_cap" || doc.Error.Ctx["expr"] != "price > 100" {
		t.Fatalf("detail missing: %s", out)
	}
	// Un fallo sin aprobacion queda intacto.
	if got := withApprovalDetail(env, fmt.Errorf("db down")); string(got) != string(env) {
		t.Fatal("plain failure must be untouched")
	}
}
