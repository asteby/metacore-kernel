package wasm

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

func seqEnv(t *testing.T, b []byte) (bool, map[string]any, map[string]any) {
	t.Helper()
	var env struct {
		Success bool           `json:"success"`
		Data    map[string]any `json:"data"`
		Error   map[string]any `json:"error"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatal(err)
	}
	return env.Success, env.Data, env.Error
}

// ABI 1.21: sequence_next carries scope_value to the scoped backend; a host
// wired only with the legacy backend refuses a scoped request instead of
// drawing from the wrong series.
func TestSequenceNext_ScopeValue(t *testing.T) {
	org := uuid.New()
	var gotScope string
	scoped := &invocation{addonKey: "a", orgID: org,
		sequenceScoped: func(_ context.Context, _ uuid.UUID, model, key, scope string) (string, error) {
			gotScope = scope
			return "S-" + scope, nil
		}}
	ok, data, _ := seqEnv(t, executeSequenceNext(context.Background(), scoped, []byte(`{"model":"m","key":"k","scope_value":"emi-1"}`)))
	if !ok || data["value"] != "S-emi-1" || gotScope != "emi-1" {
		t.Fatalf("scoped: ok=%v data=%v scope=%q", ok, data, gotScope)
	}

	legacy := &invocation{addonKey: "a", orgID: org,
		sequenceNext: func(context.Context, uuid.UUID, string, string) (string, error) { return "L-1", nil }}
	ok, data, _ = seqEnv(t, executeSequenceNext(context.Background(), legacy, []byte(`{"model":"m","key":"k"}`)))
	if !ok || data["value"] != "L-1" {
		t.Fatalf("legacy request must still work: ok=%v data=%v", ok, data)
	}
	ok, _, errBody := seqEnv(t, executeSequenceNext(context.Background(), legacy, []byte(`{"model":"m","key":"k","scope_value":"x"}`)))
	if ok || errBody["code"] != "scope_unsupported" {
		t.Fatalf("legacy backend + scope_value must be refused, got ok=%v err=%v", ok, errBody)
	}
}
