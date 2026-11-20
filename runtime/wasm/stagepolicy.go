package wasm

import (
	"context"
	"errors"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/google/uuid"
)

// StagePolicyFn is the per-transition policy gate of data_mutate / data_batch
// (K3b): roles, approval, guard, requires_fields and the locked-stage rule that
// dynamic.Service.Update enforces. Hosts wire dynamic.Service.EnforceWasmStageWrite.
// A nil return lets the write proceed; any error refuses it (and the batch
// rolls back). Config.WasmStagePolicy (warn by default) decides inside the
// function whether a violation is logged or returned.
type StagePolicyFn func(ctx context.Context, w dynamic.WasmStageWrite) error

// WithStagePolicy wires the per-transition policy gate into data_mutate and
// data_batch for updates and deletes of rows whose model has a stage machine
// (Host.WithStageMachine). Without it only the from -> to whitelist applies
// (legacy). See docs/wasm-abi.md § 14.11.
func (h *Host) WithStagePolicy(fn StagePolicyFn) *Host {
	h.stagePolicy = fn
	return h
}

// stagePolicy binds one invocation's gate to the mutation being applied. A nil
// *stagePolicyGate is a no-op so applyMutation can call it unconditionally.
type stagePolicyGate struct {
	fn      StagePolicyFn
	ctx     context.Context
	addon   string
	org     uuid.UUID
	table   string
	model   string
	machine *dynamic.StageMachine
}

// stagePolicyFn is the name applyMutation takes the gate under.
type stagePolicyFn = *stagePolicyGate

// stagePolicyFor builds the gate for one request: nil when the host wired no
// policy or the table has no stage machine.
func stagePolicyFor(ctx context.Context, inv *invocation, addonKey string, orgID uuid.UUID, req *dataMutateRequest) *stagePolicyGate {
	if inv == nil || inv.stagePolicy == nil || req == nil {
		return nil
	}
	sm := stageFor(inv, req.Table, req.Model)
	if sm == nil {
		return nil
	}
	return &stagePolicyGate{fn: inv.stagePolicy, ctx: ctx, addon: addonKey, org: orgID, table: req.Table, model: req.Model, machine: sm}
}

func (g *stagePolicyGate) check(op, recordID string, before, data, inc map[string]any) error {
	if g == nil {
		return nil
	}
	return g.fn(g.ctx, dynamic.WasmStageWrite{
		Model: g.model, Table: g.table, AddonKey: g.addon, Op: op, RecordID: recordID,
		OrgID: g.org, Machine: g.machine, Before: before, Patch: data, Inc: inc,
	})
}

// stagePolicyCode maps a refusal of the gate to the envelope code.
func stagePolicyCode(err error) string {
	var ar *dynamic.ApprovalRequiredError
	var ve *dynamic.ValidationError
	switch {
	case errors.As(err, &ar):
		return "approval_required"
	case errors.Is(err, dynamic.ErrForbidden):
		return "forbidden"
	case errors.As(err, &ve):
		for _, fes := range ve.Fields {
			for _, fe := range fes {
				if fe.Code == dynamic.CodeRecordLocked {
					return "record_locked"
				}
			}
		}
		return "validation_failed"
	case errors.Is(err, dynamic.ErrInvalidTransition):
		return "invalid_transition"
	}
	return "db_error"
}
