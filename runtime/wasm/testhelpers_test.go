package wasm

import (
	"testing"

	"github.com/google/uuid"
)

// anyModule returns an idle pooled instance of (addon, installation) without
// taking it out of the pool (tests poke at its wazero module).
func (h *Host) anyModule(t *testing.T, addonKey string, installation uuid.UUID) *Module {
	t.Helper()
	v, ok := h.modules.Load(instanceKey(addonKey, installation))
	if !ok {
		t.Fatal("expected a pool after a warm Invoke")
	}
	p := v.(*instPool)
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.idle) == 0 {
		t.Fatal("pool has no idle instance")
	}
	return p.idle[0]
}

// poolStats returns (idle, busy) for the installation's pool, (0,0) if none.
func (h *Host) poolStats(addonKey string, installation uuid.UUID) (int, int) {
	v, ok := h.modules.Load(instanceKey(addonKey, installation))
	if !ok {
		return 0, 0
	}
	return v.(*instPool).stats()
}
