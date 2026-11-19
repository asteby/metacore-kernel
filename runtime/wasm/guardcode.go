package wasm

import (
	"encoding/json"
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

// withApprovalDetail enriches an `approval_required` failure envelope with the
// structured detail a guest needs to ask for the authorization: the approval
// policy of the guard that fired ({key, label, roles, reason_required,
// expires_hours}) and the context that triggered it ({expr, values}). Any other
// failure (or an envelope it cannot decode) is returned untouched. Additive:
// the envelope keeps `error.code`, `error.message` and `error.message_key`.
func withApprovalDetail(env []byte, gErr error) []byte {
	var ce *dynamic.ConstraintError
	if !errors.As(gErr, &ce) || !ce.Def.RequestsApproval() {
		return env
	}
	var doc map[string]any
	if err := json.Unmarshal(env, &doc); err != nil {
		return env
	}
	errBody, ok := doc["error"].(map[string]any)
	if !ok {
		return env
	}
	pol := ce.Def.Approval
	errBody["policy"] = map[string]any{
		"key":             ce.ErrorKey,
		"label":           pol.Label,
		"roles":           pol.Roles,
		"reason_required": pol.ReasonRequired,
		"expires_hours":   pol.ExpiresHours,
	}
	errBody["context"] = ce.ViolationMap()
	out, err := json.Marshal(doc)
	if err != nil {
		return env
	}
	return out
}
