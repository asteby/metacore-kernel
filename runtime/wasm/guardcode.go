package wasm

import (
	"errors"

	"github.com/asteby/metacore-kernel/dynamic"
)

// guardErrCode picks the envelope code for a violated declarative guard. A
// guard declared `on_violation: request_approval` is not a plain refusal: the
// write needs a supervisor, so the guest gets `approval_required` and can ask
// for the authorization (PIN grant, then approval_consume) instead of treating
// it as invalid data. The batch is still rolled back and NOTHING is parked: a
// wasm write has no applier to replay it, so parking would strand a request
// nobody can apply.
func guardErrCode(err error) string {
	var ce *dynamic.ConstraintError
	if errors.As(err, &ce) && ce.Def.RequestsApproval() {
		return "approval_required"
	}
	return "constraint_violation"
}
