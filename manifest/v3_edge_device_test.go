package manifest_test

import (
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// edgeDeviceManifestJSON exercises the EdgeDevice primitive: a cash-recycler
// class with pairing credentials, a wasm-dispatched event and a command. All
// additive v3 fields the strict schema + struct validator must accept on
// both surfaces (v3.Parse and the legacy install-surface manifest.Validate).
const edgeDeviceManifestJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "cash_recycler", "name": "Cash Recycler", "version": "1.0.0" },
  "compatibility": { "requires": [{ "key": "kernel", "version": ">=3.0.0 <4.0.0" }] },
  "edge_devices": [
    {
      "key": "cashdro",
      "label": "CashDro",
      "kind": "cash_recycler",
      "transport": "ws",
      "heartbeat_interval_seconds": 15,
      "pairing_credentials": [
        { "key": "branch_id", "type": "string", "required": true },
        { "key": "serial_number", "type": "string", "required": true },
        { "key": "pairing_pin", "type": "secret", "required": true }
      ],
      "events": [
        { "type": "cash.deposited", "do": "wasm:on_cash_deposited", "idempotent": true },
        { "type": "device.error", "do": "wasm:on_device_error" }
      ],
      "commands": [
        { "type": "open_session", "timeout_seconds": 5 },
        { "type": "dispense_change", "timeout_seconds": 30 }
      ]
    }
  ]
}`

func TestParse_EdgeDevice_Accepted(t *testing.T) {
	m, err := v3.Parse([]byte(edgeDeviceManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse rejected edge device manifest: %v", err)
	}
	if len(m.EdgeDevices) != 1 {
		t.Fatalf("edge_devices = %+v", m.EdgeDevices)
	}
	d := m.EdgeDevices[0]
	if d.Key != "cashdro" || d.Kind != "cash_recycler" || d.Transport != "ws" {
		t.Fatalf("device = %+v", d)
	}
	if d.HeartbeatIntervalSeconds != 15 {
		t.Fatalf("heartbeat_interval_seconds = %d", d.HeartbeatIntervalSeconds)
	}
	if len(d.PairingCredentials) != 3 || d.PairingCredentials[2].Type != "secret" {
		t.Fatalf("pairing_credentials = %+v", d.PairingCredentials)
	}
	if len(d.Events) != 2 || d.Events[0].Do != "wasm:on_cash_deposited" || !d.Events[0].Idempotent {
		t.Fatalf("events = %+v", d.Events)
	}
	if len(d.Commands) != 2 || d.Commands[0].Type != "open_session" || d.Commands[0].TimeoutSeconds != 5 {
		t.Fatalf("commands = %+v", d.Commands)
	}
}

func TestFromV3_EdgeDevice_Mapped(t *testing.T) {
	m, err := v3.Parse([]byte(edgeDeviceManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse: %v", err)
	}
	out := manifest.FromV3(m)
	if len(out.EdgeDevices) != 1 {
		t.Fatalf("mapped edge_devices = %+v", out.EdgeDevices)
	}
	dev := out.EdgeDevices[0]
	if dev.Key != "cashdro" || dev.Kind != "cash_recycler" || dev.Transport != "ws" {
		t.Fatalf("mapped device = %+v", dev)
	}
	// The "secret"-typed pairing credential maps to Secret=true, mirroring a
	// Connector credential, so the host stores it encrypted.
	pin := dev.PairingCredentials[2]
	if pin.Key != "pairing_pin" || !pin.Secret {
		t.Fatalf("mapped pairing credential = %+v, want pairing_pin secret=true", pin)
	}
	if len(dev.Events) != 2 || dev.Events[0].Do != "wasm:on_cash_deposited" {
		t.Fatalf("mapped events = %+v", dev.Events)
	}
	if len(dev.Commands) != 2 || dev.Commands[1].Type != "dispense_change" {
		t.Fatalf("mapped commands = %+v", dev.Commands)
	}

	// Every wasm export referenced by an event's `do` must land in
	// Backend.Exports — otherwise the wasm runtime whitelist
	// (runtime/wasm.wasm.go containsString(spec.Exports, fn)) rejects the
	// dispatch at first invocation despite the manifest declaring it.
	if out.Backend == nil || out.Backend.Runtime != "wasm" {
		t.Fatalf("expected a derived wasm backend, got %+v", out.Backend)
	}
	wantExports := map[string]bool{"on_cash_deposited": false, "on_device_error": false}
	for _, e := range out.Backend.Exports {
		if _, ok := wantExports[e]; ok {
			wantExports[e] = true
		}
	}
	for fn, found := range wantExports {
		if !found {
			t.Fatalf("backend.exports = %+v, missing edge device event export %q", out.Backend.Exports, fn)
		}
	}

	// Strict (install-surface) validation must also accept it.
	if err := out.Validate("3.5.0"); err != nil {
		t.Fatalf("strict manifest.Validate rejected mapped edge device manifest: %v", err)
	}
}

func TestValidate_EdgeDevice_Rejections(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(string) string
		want   string
	}{
		{
			name:   "unknown transport",
			mutate: func(s string) string { return strings.Replace(s, `"transport": "ws",`, `"transport": "mqtt",`, 1) },
			want:   "transport",
		},
		{
			name: "unknown kind",
			mutate: func(s string) string {
				return strings.Replace(s, `"kind": "cash_recycler",`, `"kind": "vending_machine",`, 1)
			},
			want: "kind",
		},
		{
			name: "event missing do",
			mutate: func(s string) string {
				return strings.Replace(s, `"do": "wasm:on_cash_deposited", "idempotent": true`, `"idempotent": true`, 1)
			},
			want: "do",
		},
		{
			name: "event bad do prefix",
			mutate: func(s string) string {
				return strings.Replace(s, `"do": "wasm:on_cash_deposited"`, `"do": "rpc:on_cash_deposited"`, 1)
			},
			want: "do",
		},
		{
			name: "command missing type",
			mutate: func(s string) string {
				return strings.Replace(s, `{ "type": "open_session", "timeout_seconds": 5 },`, `{ "timeout_seconds": 5 },`, 1)
			},
			want: "type",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := tc.mutate(edgeDeviceManifestJSON)
			err := v3.Validate([]byte(raw))
			if err == nil {
				t.Fatalf("expected validation error mentioning %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}
