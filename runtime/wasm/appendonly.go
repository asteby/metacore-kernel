package wasm

import (
	"fmt"

	"github.com/asteby/metacore-kernel/dynamic"
)

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
