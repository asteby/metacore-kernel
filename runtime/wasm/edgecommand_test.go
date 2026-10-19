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
