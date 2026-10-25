package wasm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/security"
	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type fakeEdge struct {
	devices map[string]EdgeDevice
	got     *EdgeCommandRequest
	res     EdgeCommandResult
	err     error
}

func (f *fakeEdge) Device(_ context.Context, _ uuid.UUID, id string) (EdgeDevice, error) {
	d, ok := f.devices[id]
	if !ok {
		return EdgeDevice{}, fmt.Errorf("lookup %s: %w", id, ErrEdgeDeviceNotFound)
	}
	return d, nil
}

func (f *fakeEdge) Send(_ context.Context, req EdgeCommandRequest) (EdgeCommandResult, error) {
	f.got = &req
	return f.res, f.err
}

func edgeInv(org uuid.UUID, e EdgeCommander, grants ...string) *invocation {
	var caps []manifest.Capability
	for _, g := range grants {
		caps = append(caps, manifest.Capability{Kind: "edge:command", Target: g})
	}
	return &invocation{addonKey: "cash_recycler", orgID: org, caps: security.Compile("cash_recycler", caps), edgeCommander: e}
}

func TestEdgeCommand(t *testing.T) {
	org := uuid.New()
	e := &fakeEdge{
		devices: map[string]EdgeDevice{
			"dev-1": {ID: "dev-1", AddonKey: "cash_recycler", DeviceKey: "cashdro"},
			"prn-1": {ID: "prn-1", AddonKey: "printing", DeviceKey: "thermal"},
		},
		res: EdgeCommandResult{CommandID: "cmd-9", DeviceID: "dev-1", OK: true, Data: json.RawMessage(`{"code":1}`), ReceivedAt: time.Unix(0, 0).UTC()},
	}
	inv := edgeInv(org, e, "cashdro.cashdro")
	ctx := context.Background()
	ok, d := capEnv(t, executeEdgeCommand(ctx, inv, []byte(`{"device_id":"dev-1","kind":"cashdro","payload":{"operation":"askOperation"},"idempotency_key":"op-1:ask:3","timeout_ms":90000,"org":"evil"}`)))
	if !ok || d["command_id"] != "cmd-9" || d["ok"] != true {
		t.Fatalf("allowed command failed: %v", d)
	}
	if data, _ := d["data"].(map[string]any); data["code"] != float64(1) {
		t.Fatalf("device answer not transported: %v", d)
	}
	// Org and addon come from the invocation; the timeout is capped.
	if e.got.Org != org || e.got.Addon != "cash_recycler" || e.got.Kind != "cashdro" || e.got.IdempotencyKey != "op-1:ask:3" || e.got.Timeout != edgeCommandMaxTimeout ||
		!strings.Contains(string(e.got.Payload), "askOperation") {
		t.Fatalf("commander got %+v", e.got)
	}

	cases := []struct {
		name string
		inv  *invocation
		req  string
		code string
	}{
		{"no commander", edgeInv(org, nil, "cashdro.*"), `{"device_id":"dev-1","kind":"cashdro"}`, "edge_unavailable"},
		{"no device", inv, `{"kind":"cashdro"}`, "invalid_request"},
		{"bad payload", inv, `{"device_id":"dev-1","kind":"cashdro","payload":"x`, "invalid_request"},
		{"unknown device", inv, `{"device_id":"nope","kind":"cashdro"}`, "device_not_found"},
		{"another addon's device", edgeInv(org, e, "thermal.*", "cashdro.*"), `{"device_id":"prn-1","kind":"print"}`, "forbidden"},
		{"kind not granted", inv, `{"device_id":"dev-1","kind":"open_url"}`, "forbidden"},
		{"no grant at all", edgeInv(org, e), `{"device_id":"dev-1","kind":"cashdro"}`, "forbidden"},
		{"no org", &invocation{addonKey: "cash_recycler", edgeCommander: e}, `{"device_id":"dev-1","kind":"cashdro"}`, "no_active_org"},
		{"key too long", inv, `{"device_id":"dev-1","kind":"cashdro","idempotency_key":"` + strings.Repeat("k", 201) + `"}`, "invalid_request"},
	}
	for _, c := range cases {
		e.got = nil
		ok, d := capEnv(t, executeEdgeCommand(ctx, c.inv, []byte(c.req)))
		if ok || d["code"] != c.code {
			t.Errorf("%s: ok=%v %v, want %s", c.name, ok, d, c.code)
		}
		if e.got != nil {
			t.Errorf("%s: command sent", c.name)
		}
	}

	// Commander errors map to stable codes.
	for err, code := range map[error]string{
		ErrEdgeOffline: "device_offline", ErrEdgeTimeout: "timeout", ErrEdgeIdempotencyConflict: "idempotency_conflict",
		context.DeadlineExceeded: "timeout", fmt.Errorf("boom"): "edge_error",
	} {
		e.err = fmt.Errorf("send: %w", err)
		if ok, d := capEnv(t, executeEdgeCommand(ctx, inv, []byte(`{"device_id":"dev-1","kind":"cashdro"}`))); ok || d["code"] != code {
			t.Errorf("%v: %v, want %s", err, d, code)
		}
	}
	e.err = nil
	// Default timeout when the guest gives none; oversized answers refused.
	capEnv(t, executeEdgeCommand(ctx, inv, []byte(`{"device_id":"dev-1","kind":"cashdro"}`)))
	if e.got.Timeout != edgeCommandDefaultTimeout {
		t.Fatalf("default timeout %v", e.got.Timeout)
	}
	e.res.Data = json.RawMessage(`"` + strings.Repeat("x", edgeCommandMaxResponseBytes) + `"`)
	if ok, d := capEnv(t, executeEdgeCommand(ctx, inv, []byte(`{"device_id":"dev-1","kind":"cashdro"}`))); ok || d["code"] != "response_too_large" {
		t.Fatalf("oversized: %v", d)
	}
}

func TestEdgeCommandLimitsAndGuards(t *testing.T) {
	org := uuid.New()
	e := &fakeEdge{
		devices: map[string]EdgeDevice{"dev-1": {ID: "dev-1", AddonKey: "cash_recycler", DeviceKey: "cashdro"}, "dev-2": {ID: "dev-2", AddonKey: "cash_recycler", DeviceKey: "cashdro"}},
		res:     EdgeCommandResult{CommandID: "c", OK: true},
	}
	ctx := context.Background()
	req := []byte(`{"device_id":"dev-1","kind":"cashdro"}`)

	// Nonce and response cap reach the embedder; the nonce comes back.
	inv := edgeInv(org, e, "cashdro.*")
	ok, d := capEnv(t, executeEdgeCommand(ctx, inv, req))
	if !ok || e.got.Nonce == "" || len(e.got.Nonce) != 32 || d["nonce"] != e.got.Nonce || e.got.MaxResponseBytes != edgeCommandMaxResponseBytes {
		t.Fatalf("nonce/cap: %v %+v", d, e.got)
	}
	capEnv(t, executeEdgeCommand(ctx, inv, req))
	if first := e.got.Nonce; first == d["nonce"] {
		t.Fatal("nonce reused across deliveries")
	}

	// Inside a transactional invocation: refused before anything is sent.
	poolDB, err := gorm.Open(sqlite.Open("file:edge_tx_"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	realTx := poolDB.Begin()
	defer realTx.Rollback()
	txInv := edgeInv(org, e, "cashdro.*")
	txInv.tx = realTx
	e.got = nil
	if ok, d := capEnv(t, executeEdgeCommand(ctx, txInv, req)); ok || d["code"] != "in_transaction" || e.got != nil {
		t.Fatalf("in tx: %v", d)
	}
	// A POOL handle in inv.tx (Native actions pass actx.DB) is not a transaction:
	// same bug class as flushEmits, the command must go out.
	poolInv := edgeInv(org, e, "cashdro.*")
	poolInv.tx = poolDB
	if ok, d := capEnv(t, executeEdgeCommand(ctx, poolInv, req)); !ok || e.got == nil {
		t.Fatalf("pool handle rejected as in_transaction: %v", d)
	}
	// And consumeDB only prefers inv.tx when it really is one.
	if consumeDB(&invocation{tx: poolDB, db: poolDB}) != poolDB || consumeDB(&invocation{tx: realTx, db: poolDB}) != realTx {
		t.Fatal("consumeDB must use inv.tx only when it is an open transaction")
	}
	if consumeDB(&invocation{tx: &gorm.DB{}, db: poolDB}) != poolDB {
		t.Fatal("a non-tx inv.tx must fall back to inv.db")
	}

	// Per invocation.
	lim := newEdgeLimiter(3, 1000)
	inv = edgeInv(org, e, "cashdro.*")
	inv.edgeLimits = lim
	for i := 0; i < 3; i++ {
		if ok, d := capEnv(t, executeEdgeCommand(ctx, inv, req)); !ok {
			t.Fatalf("command %d: %v", i, d)
		}
	}
	if ok, d := capEnv(t, executeEdgeCommand(ctx, inv, req)); ok || d["code"] != "rate_limited" {
		t.Fatalf("4th in one invocation: %v", d)
	}

	// Per device per minute, across invocations; another device is apart.
	lim = newEdgeLimiter(100, 2)
	for i := 0; i < 2; i++ {
		inv = edgeInv(org, e, "cashdro.*")
		inv.edgeLimits = lim
		capEnv(t, executeEdgeCommand(ctx, inv, req))
	}
	inv = edgeInv(org, e, "cashdro.*")
	inv.edgeLimits = lim
	if ok, d := capEnv(t, executeEdgeCommand(ctx, inv, req)); ok || d["code"] != "rate_limited" {
		t.Fatalf("3rd per minute: %v", d)
	}
	if ok, _ := capEnv(t, executeEdgeCommand(ctx, inv, []byte(`{"device_id":"dev-2","kind":"cashdro"}`))); !ok {
		t.Fatal("another device was limited")
	}
	if lim.allow(org, "cash_recycler", "dev-1", time.Now().Add(61*time.Second)) != true {
		t.Fatal("window did not slide")
	}

	// The timeout never passes the invocation's hard cap.
	dctx, d2, cancel := withGuestDeadline(ctx, 5*time.Second, 6*time.Second)
	defer cancel()
	inv = edgeInv(org, e, "cashdro.*")
	inv.deadline = d2
	capEnv(t, executeEdgeCommand(dctx, inv, []byte(`{"device_id":"dev-1","kind":"cashdro","timeout_ms":60000}`)))
	if e.got.Timeout > 5*time.Second || e.got.Timeout < 4*time.Second {
		t.Fatalf("timeout %v not clamped to the hard cap", e.got.Timeout)
	}
	_, d3, cancel3 := withGuestDeadline(ctx, time.Second, 1500*time.Millisecond)
	defer cancel3()
	inv.deadline = d3
	e.got = nil
	if ok, d := capEnv(t, executeEdgeCommand(dctx, inv, req)); ok || d["code"] != "timeout" || e.got != nil {
		t.Fatalf("no time left: %v", d)
	}
}
