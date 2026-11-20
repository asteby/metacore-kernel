package conformance_test

import (
	"testing"

	"github.com/asteby/metacore-kernel/payments/conformance"
	"github.com/asteby/metacore-kernel/payments/conformance/refsim"
)

// ExampleRunT is the exact shape docs/conformance-payments.md shows: it is
// compiled with the package, so the documented API cannot drift. In an addon,
// replace refsim.New().Target with the provider's own Target.
func ExampleRunT() {
	var t *testing.T // inside a real Test function this is the test's t
	_ = func() {
		target := refsim.New().Target("acme") // Target{Name, Invoke, Sandbox, Supports}
		conformance.RunT(t, target, conformance.Options{
			// Lenient: true,                                  // tolerate skips (verdict conforms_lenient)
			// WebhookMode: conformance.WebhookHintReread,     // provider without signature, re-reads
			// Waive: map[string]string{"MON-02": "reason"},   // max 5, never the CoreCases
		})
	}
}
