package dynamic

import (
	"context"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/gofiber/fiber/v3"
)

// TestAction_Idempotency_ReplaysStoredResponse proves a second invocation with
// the same key replays the first response WITHOUT re-dispatching the handler.
func TestAction_Idempotency_ReplaysStoredResponse(t *testing.T) {
	fx := setupActionFixture(t)
	fx.registerAction("test_products", &manifest.ActionDef{
		Key:         "capture",
		Trigger:     &manifest.ActionTrigger{Type: "wasm", Export: "capture"},
		Idempotency: &manifest.IdempotencyDef{KeyField: "request_id"},
	})

	fx.wasm.fn = func(_ context.Context, _ ActionRequest) (ActionResponse, error) {
		return ActionResponse{Success: true, Data: map[string]any{"charge_id": "ch_1"}}, nil
	}

	body := `{"request_id":"req-abc"}`
	status1, env1 := invokeAction(t, fx.app, "test_products", fx.productID, "capture", body)
	if status1 != fiber.StatusOK || env1["success"] != true {
		t.Fatalf("first call failed: %d %v", status1, env1)
	}
	if fx.wasm.calls != 1 {
		t.Fatalf("first call should dispatch once, got %d", fx.wasm.calls)
	}

	status2, env2 := invokeAction(t, fx.app, "test_products", fx.productID, "capture", body)
	if status2 != fiber.StatusOK || env2["success"] != true {
		t.Fatalf("replay failed: %d %v", status2, env2)
	}
	if fx.wasm.calls != 1 {
		t.Fatalf("replay must NOT re-dispatch: calls=%d", fx.wasm.calls)
	}
	meta, _ := env2["meta"].(map[string]any)
	if meta["idempotent_replay"] != true {
		t.Fatalf("replay should be flagged idempotent_replay=true, meta=%v", meta)
	}
	data, _ := env2["data"].(map[string]any)
	if data["charge_id"] != "ch_1" {
		t.Fatalf("replay lost the original data: %v", env2["data"])
	}
}

// TestAction_Idempotency_DistinctKeysDispatchSeparately proves different keys
// are independent invocations.
func TestAction_Idempotency_DistinctKeysDispatchSeparately(t *testing.T) {
	fx := setupActionFixture(t)
	fx.registerAction("test_products", &manifest.ActionDef{
		Key:         "capture",
		Trigger:     &manifest.ActionTrigger{Type: "wasm", Export: "capture"},
		Idempotency: &manifest.IdempotencyDef{KeyField: "request_id"},
	})
	fx.wasm.fn = func(_ context.Context, _ ActionRequest) (ActionResponse, error) {
		return ActionResponse{Success: true}, nil
	}

	invokeAction(t, fx.app, "test_products", fx.productID, "capture", `{"request_id":"a"}`)
	invokeAction(t, fx.app, "test_products", fx.productID, "capture", `{"request_id":"b"}`)
	if fx.wasm.calls != 2 {
		t.Fatalf("distinct keys should dispatch twice, got %d", fx.wasm.calls)
	}
}

// TestAction_Idempotency_FailureNotCached proves a failed action stays
// retryable — the failure is not stored for replay.
func TestAction_Idempotency_FailureNotCached(t *testing.T) {
	fx := setupActionFixture(t)
	fx.registerAction("test_products", &manifest.ActionDef{
		Key:         "capture",
		Trigger:     &manifest.ActionTrigger{Type: "wasm", Export: "capture"},
		Idempotency: &manifest.IdempotencyDef{KeyField: "request_id"},
	})

	// First attempt declines, second succeeds — the retry MUST reach the handler.
	fx.wasm.fn = func(_ context.Context, _ ActionRequest) (ActionResponse, error) {
		return ActionResponse{Success: false, Error: &ActionError{Code: "busy", Message: "try again"}}, nil
	}
	invokeAction(t, fx.app, "test_products", fx.productID, "capture", `{"request_id":"req-x"}`)

	fx.wasm.fn = func(_ context.Context, _ ActionRequest) (ActionResponse, error) {
		return ActionResponse{Success: true, Data: map[string]any{"ok": true}}, nil
	}
	status, env := invokeAction(t, fx.app, "test_products", fx.productID, "capture", `{"request_id":"req-x"}`)
	if status != fiber.StatusOK || env["success"] != true {
		t.Fatalf("retry after failure should dispatch and succeed: %d %v", status, env)
	}
	if fx.wasm.calls != 2 {
		t.Fatalf("failed action must not be cached: calls=%d", fx.wasm.calls)
	}
}
