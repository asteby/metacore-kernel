package dispatch_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/asteby/metacore-kernel/dispatch"
	"github.com/asteby/metacore-kernel/events"
	"github.com/google/uuid"
)

// With no CompiledRegistry wired, a "compiled" subscription is resolved through
// the invoker, the same seam a wasm export uses, so a native module's Go export
// (wasm.Host.LoadNative) answers declarative subscriptions with no second
// registry. A registered CompiledHandler still wins.
func TestDispatch_CompiledFallsBackToInvoker(t *testing.T) {
	bus := events.NewBus(nil)
	ldb := ledgerDB(t)

	var fn atomic.Value
	invoker := dispatch.WasmInvokerFunc(func(_ context.Context, _, _ uuid.UUID, _, function string, _ []byte, _ map[string]string) ([]byte, error) {
		fn.Store(function)
		return []byte(`{"success":true}`), nil
	})
	provider := staticProvider("firstparty", uuid.New(), "inventory.product.updated", "compiled", "recompute")

	barrier := make(chan dispatch.DeliveryResult, 4)
	cancel, err := dispatch.Wire(bus, invoker, ldb, provider,
		dispatch.WithOnDelivery(func(r dispatch.DeliveryResult) { barrier <- r }),
		dispatch.WithLogger(silentLogger()))
	if err != nil {
		t.Fatalf("Wire: %v", err)
	}
	defer cancel()

	publishCanonical(t, bus, "inventory", "product", "updated", uuid.New(), "row-n")
	awaitDeliveries(t, barrier, 1)

	if got, _ := fn.Load().(string); got != "recompute" {
		t.Fatalf("invoker got function %q, want recompute", got)
	}
	if singleDelivery(t, ldb).Status != dispatch.StatusDelivered {
		t.Fatalf("compiled delivery via invoker not marked delivered")
	}
}
