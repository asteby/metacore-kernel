package wasm

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/google/uuid"
)

// Every host import failure carries code + message + message_key so the guest
// can propagate a translatable, specific cause instead of a generic code.
func TestHostErrorEnvelopesCarryMessageKey(t *testing.T) {
	org := uuid.New()
	now := time.Now()
	builders := map[string][]byte{
		"data_mutate":      dataMutateErr("a", "db_error", "boom", org, now, ""),
		"data_batch":       dataBatchErr("a", "db_error", "boom", org, now, ""),
		"ctx_get":          ctxGetErr("a", "db_error", "boom", org, now),
		"sequence_next":    sequenceNextErr("a", "db_error", "boom", org, now),
		"event_emit":       eventEmitErr("a", "db_error", "boom", org, now),
		"routing_resolve":  routingResolveErr("a", "db_error", "boom", org, now),
		"approval_request": approvalRequestErr("a", "db_error", "boom", org, now),
		"db_exec":          dbExecErr("s", "db_error", "boom", 1),
		"db_query":         dbQueryErr("s", "db_error", "boom", 1),
	}
	for name, raw := range builders {
		var keyed struct {
			Success bool              `json:"success"`
			Error   map[string]string `json:"error"`
		}
		if err := json.Unmarshal(raw, &keyed); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if keyed.Success || keyed.Error["code"] != "db_error" || keyed.Error["message"] != "boom" ||
			keyed.Error["message_key"] != "wasm.host.error.db_error" {
			t.Fatalf("%s: envelope %s", name, raw)
		}
	}

	var legacy map[string]string
	_ = json.Unmarshal(jsonError("forbidden", "no"), &legacy)
	if legacy["error"] != "forbidden" || legacy["message_key"] != "wasm.host.error.forbidden" {
		t.Fatalf("jsonError: %v", legacy)
	}
	if hostErrorMessageKey("") != "wasm.host.error.unknown" {
		t.Fatal("empty code must map to unknown")
	}
}

func TestDataMutateAndBatchErrCarryCorrelationID(t *testing.T) {
	now := time.Now()
	for name, raw := range map[string][]byte{
		"data_mutate": dataMutateErr("a", "db_error", "boom", uuid.Nil, now, "corr-wasm"),
		"data_batch":  dataBatchErr("a", "db_error", "boom", uuid.Nil, now, "corr-wasm"),
	} {
		var env struct {
			Error map[string]string `json:"error"`
			Meta  map[string]any    `json:"meta"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if env.Error["correlation_id"] != "corr-wasm" || env.Meta["correlation_id"] != "corr-wasm" {
			t.Fatalf("%s: %s", name, raw)
		}
		if env.Error["code"] != "db_error" || env.Error["message_key"] != "wasm.host.error.db_error" {
			t.Fatalf("%s: existing error keys changed: %s", name, raw)
		}
	}
	for name, raw := range map[string][]byte{
		"data_mutate": dataMutateErr("a", "db_error", "boom", uuid.Nil, now, "  "),
		"data_batch":  dataBatchErr("a", "db_error", "boom", uuid.Nil, now, ""),
	} {
		if strings.Contains(string(raw), "correlation_id") {
			t.Fatalf("%s minted a correlation id: %s", name, raw)
		}
	}
}

// End to end through a real import path: the cross-tenant refusal is keyed.
func TestDataMutateForbiddenCarriesMessageKey(t *testing.T) {
	gdb, _, cleanup := newMockGorm(t)
	defer cleanup()
	bus, _, _ := captureBus(t, "inventory.Stock.created")
	inv := testInvocation(gdb, bus, uuid.New(), stockWriteEnforcer(), nil)
	out := executeDataMutate(context.Background(), inv, []byte(`{
		"op": "create", "table": "stock", "model": "Stock",
		"data": {"organization_id": "`+uuid.NewString()+`"}
	}`))
	var env struct {
		Error map[string]string `json:"error"`
	}
	if err := json.Unmarshal(out, &env); err != nil || env.Error["message_key"] != "wasm.host.error.forbidden" {
		t.Fatalf("want message_key wasm.host.error.forbidden, got %s", out)
	}
	if strings.Contains(string(out), "correlation_id") {
		t.Fatalf("empty context must not invent a correlation id: %s", out)
	}

	out = executeDataMutate(dynamic.WithCorrelationID(context.Background(), "corr-forbidden"), inv, []byte(`{
		"op": "create", "table": "stock", "model": "Stock",
		"data": {"organization_id": "`+uuid.NewString()+`"}
	}`))
	env = struct {
		Error map[string]string `json:"error"`
	}{}
	if err := json.Unmarshal(out, &env); err != nil || env.Error["correlation_id"] != "corr-forbidden" {
		t.Fatalf("want correlation_id corr-forbidden, got %s", out)
	}
}
