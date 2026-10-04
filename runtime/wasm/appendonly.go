package wasm

import (
	"fmt"

	"github.com/asteby/metacore-kernel/dynamic"
)

// StageMachineFn resolves the declarative stage machine for one logical
// table. model is the guest's ModelKey (may be empty); logicalTable is the
// unqualified name data_mutate addresses. Return nil when that table has no
// machine — the write stays unrestricted, which is the pre-gate behaviour.
type StageMachineFn func(logicalTable, model string) *dynamic.StageMachine

// WithStageMachine wires the single lifecycle gate into data_mutate and
// data_batch. A create or update that changes the machine's field to a pair
// StageMachine.Allows rejects is rolled back with code invalid_transition —
// the same rule dynamic.Service.Create/Update enforce. Incrementing the
// lifecycle column is refused. When unset, wasm writes do not consult a
// machine (legacy).
func (h *Host) WithStageMachine(fn StageMachineFn) *Host {
	h.stageMachine = fn
	return h
}

// WithAppendOnly injects the embedder's append-only ledger lookup for the
// `metacore_host.data_mutate` / `data_batch` imports. When the LOGICAL table of
// an update or delete is reported append-only (manifest Model.append_only) the
// mutation is refused before any DB work with the stable `append_only` code —
// the same rule dynamic.Service enforces on the REST path
// (Config.AppendOnlyResolver). Creates are never affected. When unset, every
// table stays writable (the pre-ledger behaviour).
func (h *Host) WithAppendOnly(f func(logicalTable string) bool) *Host {
	h.appendOnly = f
	return h
}

// refuseAppendOnly reports the `append_only` refusal for an update/delete on an
// append-only table, ("", nil) otherwise. An `inc` update is an update.
func refuseAppendOnly(inv *invocation, req *dataMutateRequest) (string, error) {
	if inv == nil || inv.appendOnly == nil || req == nil {
		return "", nil
	}
	if req.Op != "update" && req.Op != "delete" {
		return "", nil
	}
	if !inv.appendOnly(req.Table) {
		return "", nil
	}
	return "append_only", &dynamic.AppendOnlyError{Model: req.Table, Op: req.Op}
}

func appendOnlyBatchMsg(i int, err error) string {
	return fmt.Sprintf("mutations[%d]: %s", i, err.Error())
}
