package wasm

import (
	"fmt"
	"testing"

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
