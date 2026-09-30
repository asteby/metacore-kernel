package v3

import (
	"strings"
	"testing"
)

// enablesManifest declares a Device model with two actions and a
// compatibility.requires list, so Validate runs the JSON schema and then
// validateRequiresEnables.
func enablesManifest(requires string) []byte {
	return []byte(`{
      "apiVersion": "asteby.com/v3",
      "kind": "Addon",
      "metadata": {"key": "devices", "name": "Devices", "version": "1.0.0"},
      "compatibility": {"requires": [{"key": "kernel", "version": ">=3.0.0 <4.0.0"}, ` + requires + `]},
      "models": [
        {"key": "Device", "table": "devices", "columns": [{"name": "organization_id", "type": "uuid"}, {"name": "name", "type": "text"}]}
      ],
      "contributions": {"actions": [
        {"key": "connect_device", "target_model": "Device", "handler": {"type": "wasm", "function": "connect"}},
        {"key": "create_and_connect", "target_model": "Device", "placement": "create", "handler": {"type": "wasm", "function": "create"}}
      ]}
    }`)
}

func TestRequiresEnablesValid(t *testing.T) {
	raw := enablesManifest(`{"key": "connector_whatsapp", "version": ">=0.4.0", "optional": true,
      "reason": "Conectar dispositivos por WhatsApp",
      "enables": ["action:Device.connect_device", "action:Device.create_and_connect", "nav:devices.pairing", "widget:device_status"]}`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	opt := m.OptionalRequirements()
	if len(opt) != 1 {
		t.Fatalf("want 1 optional requirement, got %+v", opt)
	}
	got := opt[0]
	if got.Key != "connector_whatsapp" || got.Version != ">=0.4.0" || got.Reason == "" || len(got.Enables) != 4 {
		t.Fatalf("optional requirement not projected: %+v", got)
	}
	if got.Enables[0] != "action:Device.connect_device" {
		t.Fatalf("enables order not kept: %v", got.Enables)
	}
	// The projection is a copy: mutating it does not touch the manifest.
	got.Enables[0] = "x"
	if m.Compatibility.Requires[1].Enables[0] != "action:Device.connect_device" {
		t.Fatal("OptionalRequirements aliases the manifest slice")
	}
}

func TestOptionalRequirementsSkipsMandatory(t *testing.T) {
	m, err := Parse(enablesManifest(`{"key": "inventory", "version": ">=1.0.0"}, {"key": "caja", "version": ">=1.0.0", "optional": true}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	opt := m.OptionalRequirements()
	if len(opt) != 1 || opt[0].Key != "caja" || opt[0].Enables != nil {
		t.Fatalf("want only caja without enables, got %+v", opt)
	}
	var nilM *Manifest
	if nilM.OptionalRequirements() != nil {
		t.Fatal("nil manifest must return nil")
	}
}

func TestRequiresEnablesRejected(t *testing.T) {
	cases := map[string]struct {
		require string
		want    string
	}{
		"enables on a mandatory require": {
			`{"key": "connector_whatsapp", "version": ">=0.4.0", "enables": ["action:Device.connect_device"]}`,
			"only valid with optional: true",
		},
		"unknown kind": {
			`{"key": "connector_whatsapp", "version": ">=0.4.0", "optional": true, "enables": ["page:devices"]}`,
			"/enables/0': 'page:devices' does not match pattern",
		},
		"action without model": {
			`{"key": "connector_whatsapp", "version": ">=0.4.0", "optional": true, "enables": ["action:connect_device"]}`,
			"'action:connect_device' does not match pattern",
		},
		"action names no declared action": {
			`{"key": "connector_whatsapp", "version": ">=0.4.0", "optional": true, "enables": ["action:Device.pair"]}`,
			`names no contributions.actions entry with target_model "Device" and key "pair"`,
		},
		"action on the wrong model": {
			`{"key": "connector_whatsapp", "version": ">=0.4.0", "optional": true, "enables": ["action:Printer.connect_device"]}`,
			"names no contributions.actions entry",
		},
		"duplicate item": {
			`{"key": "connector_whatsapp", "version": ">=0.4.0", "optional": true, "enables": ["action:Device.connect_device", "action:Device.connect_device"]}`,
			"/enables': items at 0 and 1 are equal",
		},
		"not a string": {
			`{"key": "connector_whatsapp", "version": ">=0.4.0", "optional": true, "enables": [1]}`,
			"/enables/0': got number, want string",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := Validate(enablesManifest(tc.require))
			if err == nil {
				t.Fatal("want error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q in error, got: %v", tc.want, err)

			}
		})
	}
}

// Manifests built in Go skip the JSON schema, so the grammar and duplicate
// checks must hold in validateRequiresEnables on its own.
func TestValidateRequiresEnablesWithoutSchema(t *testing.T) {
	m := &Manifest{
		Compatibility: Compatibility{Requires: []Requirement{{
			Key: "connector_whatsapp", Version: ">=0.4.0", Optional: true,
			Enables: []string{"page:devices", "action:Device.connect_device", "action:Device.connect_device"},
		}}},
		Contributions: &Contributions{Actions: []Action{{Key: "connect_device", TargetModel: "Device"}}},
	}
	errs := validateRequiresEnables(m)
	joined := strings.Join(errs, "\n")
	if len(errs) != 2 || !strings.Contains(joined, "must be action:<ModelKey>.<action_key>") || !strings.Contains(joined, "listed twice") {
		t.Fatalf("want grammar + duplicate errors, got %v", errs)
	}
}

func TestParseEnableRef(t *testing.T) {
	cases := map[string]EnableRef{
		"action:Device.connect_device": {Kind: EnableKindAction, Model: "Device", Action: "connect_device"},
		"nav:devices.pairing":          {Kind: EnableKindNav, Key: "devices.pairing"},
		"widget:device_status":         {Kind: EnableKindWidget, Key: "device_status"},
	}
	for in, want := range cases {
		got, err := ParseEnableRef(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if got != want {
			t.Fatalf("%s: want %+v, got %+v", in, want, got)
		}
		if got.String() != in {
			t.Fatalf("round trip: %q != %q", got.String(), in)
		}
	}
	for _, bad := range []string{"", "action:", "action:Device", "action:Device.Connect", "nav:", "nav:Devices", "widget:a b", "action:Device.connect.extra"} {
		if _, err := ParseEnableRef(bad); err == nil {
			t.Fatalf("%q: want error", bad)
		}
	}
}
