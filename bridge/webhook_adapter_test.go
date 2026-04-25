package bridge

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

// TestSignedWebhookInterceptor_FallbackOnNilDispatcher verifies the
// adapter delegates to the supplied fallback when neither a DB nor a
// dispatcher is wired. This is the path host tests rely on so they can
// exercise their existing unsigned interceptor without a real signer.
func TestSignedWebhookInterceptor_FallbackOnNilDispatcher(t *testing.T) {
	called := false
	fallback := func(ctx *ActionContext, recordID interface{}, payload map[string]interface{}) (interface{}, error) {
		called = true
		return "fallback-result", nil
	}
	fn := SignedWebhookInterceptor(nil, nil, "addon", "https://example.test", fallback)
	got, err := fn(&ActionContext{OrgID: uuid.New()}, "rec", map[string]interface{}{"x": 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Fatal("fallback was not invoked")
	}
	if got != "fallback-result" {
		t.Fatalf("got = %v, want fallback-result", got)
	}
}

// TestSignedWebhookInterceptor_NilFallbackPasses verifies the adapter
// returns (nil, nil) when neither dispatcher nor fallback are wired —
// matching the "host-local pass" semantics.
func TestSignedWebhookInterceptor_NilFallbackPasses(t *testing.T) {
	fn := SignedWebhookInterceptor(nil, nil, "addon", "https://example.test", nil)
	got, err := fn(&ActionContext{OrgID: uuid.New()}, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("got = %v, want nil", got)
	}
}

// TestEventNameFromPayload covers the small extraction helper so we know
// the X-Metacore-Event header has a deterministic source.
func TestEventNameFromPayload(t *testing.T) {
	cases := []struct {
		name   string
		input  map[string]interface{}
		expect string
	}{
		{"nil", nil, ""},
		{"empty", map[string]interface{}{}, ""},
		{"event-key", map[string]interface{}{"event": "order.created"}, "order.created"},
		{"underscore-key", map[string]interface{}{"_event": "order.refunded"}, "order.refunded"},
		{"non-string", map[string]interface{}{"event": 42}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := eventNameFromPayload(tc.input); got != tc.expect {
				t.Fatalf("eventNameFromPayload(%v) = %q, want %q", tc.input, got, tc.expect)
			}
		})
	}
}

// guard: ensure the helper signature matches errors.Is so future refactors
// keep the smoke test compiling. (No call — just a compile-time check.)
var _ = errors.Is
