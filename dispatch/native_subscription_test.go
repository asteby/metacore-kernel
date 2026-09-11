package dispatch_test

// Fase D (asteby-platform-continuation-2026-09-10.md §6, step 2): contract
// tests for the "native" subscription tier — WithNativeInvoker must dispatch
// through the SAME NativeInvoker seam (isolated per org+installation, just
// like WasmInvoker.InvokeFor), a non-matching event must be a silent no-op
// (never a dead-lettered "error"), and a subscription lacking the addon's
// effective event:subscribe grant must never reach the invoker at all.

import (
	"context"
	"testing"

	"github.com/asteby/metacore-kernel/dispatch"
	"github.com/asteby/metacore-kernel/events"
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/google/uuid"
)

const nativeFixtureAddon = "link_inbox"

func nativeCaps() []manifest.Capability {
	return []manifest.Capability{
		{Kind: "event:subscribe", Target: "connector_whatsapp.whatsapp.message.received"},
	}
}

// TestDispatch_NativeSubscription_Delivered verifies a native subscription is
// invoked via NativeInvoker with the correct (org, installation, addonKey,
// operation) tuple — the same tenant/installation isolation actions get.
func TestDispatch_NativeSubscription_Delivered(t *testing.T) {
	enf := enforcerFor(nativeFixtureAddon, nativeCaps())
	bus := events.NewBus(enf)
	ldb := ledgerDB(t)

	installation := uuid.New()
	orgID := uuid.New()
	provider := staticProvider(nativeFixtureAddon, installation, "connector_whatsapp.whatsapp.message.received", "native", "ingest_native_event")

	var gotOrg, gotInstallation uuid.UUID
	var gotAddon, gotOperation string
	invoker := dispatch.NativeInvokerFunc(func(_ context.Context, org, inst uuid.UUID, addonKey, operation string, _ []byte) ([]byte, error) {
		gotOrg, gotInstallation, gotAddon, gotOperation = org, inst, addonKey, operation
		return []byte(`{"success":true}`), nil
	})

	barrier := make(chan dispatch.DeliveryResult, 4)
	cancel, err := dispatch.Wire(bus, nil, ldb, provider,
		dispatch.WithNativeInvoker(invoker),
		dispatch.WithCapabilityChecker(dispatch.NewEnforcerChecker(enf)),
		dispatch.WithOnDelivery(func(r dispatch.DeliveryResult) { barrier <- r }),
		dispatch.WithLogger(silentLogger()),
	)
	if err != nil {
		t.Fatalf("Wire: %v", err)
	}
	defer cancel()

	publishCanonical(t, bus, "connector_whatsapp", "whatsapp", "message.received", orgID, "msg-1")
	awaitDeliveries(t, barrier, 1)

	if gotOrg != orgID || gotInstallation != installation {
		t.Fatalf("native invoke did not carry the tenant/installation identity: org=%v (want %v) installation=%v (want %v)", gotOrg, orgID, gotInstallation, installation)
	}
	if gotAddon != nativeFixtureAddon || gotOperation != "ingest_native_event" {
		t.Fatalf("native invoke addon/operation mismatch: addon=%q operation=%q", gotAddon, gotOperation)
	}
	got := singleDelivery(t, ldb)
	if got.Status != dispatch.StatusDelivered {
		t.Fatalf("delivery status = %q, want delivered (err=%q)", got.Status, got.LastError)
	}
}

// TestDispatch_NativeSubscription_NoMatch_IsNoOp: an event that matches no
// declared subscription must not enqueue a delivery, must not invoke
// anything, and must not appear in the ledger as an error — it's an expected
// no-op, not a failure.
func TestDispatch_NativeSubscription_NoMatch_IsNoOp(t *testing.T) {
	enf := enforcerFor(nativeFixtureAddon, nativeCaps())
	bus := events.NewBus(enf)
	ldb := ledgerDB(t)

	// Provider declares a subscription to a DIFFERENT event than what fires.
	provider := staticProvider(nativeFixtureAddon, uuid.New(), "connector_whatsapp.whatsapp.message.received", "native", "ingest_native_event")

	invoked := false
	invoker := dispatch.NativeInvokerFunc(func(context.Context, uuid.UUID, uuid.UUID, string, string, []byte) ([]byte, error) {
		invoked = true
		return []byte(`{"success":true}`), nil
	})

	cancel, err := dispatch.Wire(bus, nil, ldb, provider,
		dispatch.WithNativeInvoker(invoker),
		dispatch.WithCapabilityChecker(dispatch.NewEnforcerChecker(enf)),
		dispatch.WithLogger(silentLogger()),
	)
	if err != nil {
		t.Fatalf("Wire: %v", err)
	}
	defer cancel()

	// A completely unrelated event fires — no subscription matches it.
	publishCanonical(t, bus, "connector_whatsapp", "whatsapp", "connection.updated", uuid.New(), "conn-1")

	if invoked {
		t.Fatalf("native invoker was called for a non-matching event")
	}
	if n := deliveryCount(t, ldb); n != 0 {
		t.Fatalf("ledger rows = %d, want 0 (no-op, not an error)", n)
	}
}

// TestDispatch_NativeSubscription_DeniedWithoutGrant: an addon that declares
// the subscription but has NO effective event:subscribe grant (a permissive
// enforcer for a DIFFERENT addon, mirroring "installed but not entitled")
// must never reach the invoker; the delivery is dead, not retried, not
// silently dropped.
func TestDispatch_NativeSubscription_DeniedWithoutGrant(t *testing.T) {
	// Enforcer only grants capabilities to "other_addon" — link_inbox has NO
	// effective grant even though it declares the subscription below.
	enf := enforcerFor("other_addon", nativeCaps())
	bus := events.NewBus(enf)
	ldb := ledgerDB(t)

	provider := staticProvider(nativeFixtureAddon, uuid.New(), "connector_whatsapp.whatsapp.message.received", "native", "ingest_native_event")

	invoked := false
	invoker := dispatch.NativeInvokerFunc(func(context.Context, uuid.UUID, uuid.UUID, string, string, []byte) ([]byte, error) {
		invoked = true
		return []byte(`{"success":true}`), nil
	})

	barrier := make(chan dispatch.DeliveryResult, 4)
	cancel, err := dispatch.Wire(bus, nil, ldb, provider,
		dispatch.WithNativeInvoker(invoker),
		dispatch.WithCapabilityChecker(dispatch.NewEnforcerChecker(enf)),
		dispatch.WithOnDelivery(func(r dispatch.DeliveryResult) { barrier <- r }),
		dispatch.WithLogger(silentLogger()),
	)
	if err != nil {
		t.Fatalf("Wire: %v", err)
	}
	defer cancel()

	// Publish under the trusted "kernel" key (bypasses the PRODUCER check) so
	// this test isolates the SUBSCRIBER-side capability gate exactly.
	publishCanonical(t, bus, "connector_whatsapp", "whatsapp", "message.received", uuid.New(), "msg-1")
	awaitDeliveries(t, barrier, 1)

	if invoked {
		t.Fatalf("native invoker was called despite the addon lacking an effective event:subscribe grant")
	}
	got := singleDelivery(t, ldb)
	if got.Status != dispatch.StatusDead {
		t.Fatalf("delivery status = %q, want dead (denied grant is terminal, not retryable)", got.Status)
	}
}
