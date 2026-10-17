package wasm

import (
	"context"

	"github.com/asteby/metacore-kernel/security"
	"github.com/google/uuid"
)

// EmitPolicyFunc decides whether addonKey may emit event in org from the
// `event_emit` import. It returns every violation found (each one is counted
// in wasm_event_emit_violations_total and published as
// security.EmitViolationEvent) and a non-nil error to deny the publish, which
// the guest sees as a `forbidden` envelope. (*security.EmitPolicy).Check has
// this signature.
type EmitPolicyFunc func(ctx context.Context, org uuid.UUID, addonKey, event string) ([]security.EmitViolation, error)

// WithEmitPolicy gates the `metacore_host.event_emit` import with an emit
// policy, typically security.NewEmitPolicy(source).Check. It runs only on
// guest emissions: data_mutate / data_batch canonical events (published as
// "kernel") and host publications never reach it. nil = no policy.
func (h *Host) WithEmitPolicy(fn EmitPolicyFunc) *Host {
	h.emitPolicy = fn
	return h
}

// checkEmit runs the policy for one event_emit call and reports violations.
// It returns the denial error, or nil when the publish may proceed.
func (inv *invocation) checkEmit(ctx context.Context, event string) error {
	if inv.emitPolicy == nil {
		return nil
	}
	violations, err := inv.emitPolicy(ctx, inv.orgID, inv.addonKey, event)
	for _, v := range violations {
		inv.metrics.emitViolation(inv.addonKey, string(v.Rule), v.Denied)
		if inv.bus != nil {
			// Host event, published as "kernel": it never re-enters the policy.
			if perr := inv.bus.Publish(ctx, "kernel", security.EmitViolationEvent, inv.orgID, v); perr != nil {
				inv.logf("event_emit violation_publish_error addon=%s event=%s err=%v", inv.addonKey, event, perr)
			}
		}
	}
	return err
}
