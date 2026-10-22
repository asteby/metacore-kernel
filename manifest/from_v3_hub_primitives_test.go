package manifest_test

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
	"github.com/asteby/metacore-kernel/modelbase"
)

// hubFixture is the hub_fleet spike manifest with the hub-primitives contract
// applied (webhook + set_fields actions, confirm_typed, ingest, upsert webhook).
func hubFixture(t *testing.T) (*v3.Manifest, manifest.Manifest) {
	t.Helper()
	raw, err := os.ReadFile("v3/testdata/hub_fleet_v230.json")
	if err != nil {
		t.Fatal(err)
	}
	m, err := v3.Parse(raw)
	if err != nil {
		t.Fatalf("v3.Parse: %v", err)
	}
	return m, manifest.FromV3(m)
}

func findAction(t *testing.T, defs []manifest.ActionDef, key string) manifest.ActionDef {
	t.Helper()
	for _, d := range defs {
		if d.Key == key {
			return d
		}
	}
	t.Fatalf("action %q not found", key)
	return manifest.ActionDef{}
}

func TestFromV3_HubPrimitives_Projection(t *testing.T) {
	_, legacy := hubFixture(t)
	defs := legacy.Actions["Instance"]

	rr := findAction(t, defs, "request_revoke")
	tr := rr.Trigger
	if tr == nil || tr.Type != "webhook" {
		t.Fatalf("request_revoke trigger: %+v", tr)
	}
	if tr.URLRef != "trust_core.base_url" || tr.Path != "/core/v1/fleet/instances/{record.instance_id}/revoke-request" || tr.Method != "POST" {
		t.Fatalf("destination lost: %+v", tr)
	}
	if tr.Signing == nil || tr.Signing.Scheme != "metacore-hmac-v1" || tr.Signing.SecretRef != "trust_core.signing_secret" {
		t.Fatalf("signing lost: %+v", tr.Signing)
	}
	if !reflect.DeepEqual(tr.SendRecord, []string{"instance_id", "license_status"}) || tr.TimeoutMS != 10000 || tr.Retries == nil || *tr.Retries != 2 {
		t.Fatalf("budget/send_record lost: %+v", tr)
	}
	if tr.OnSuccess == nil || tr.OnSuccess.Set["license_status"] != "revoke_requested" {
		t.Fatalf("on_success lost: %+v", tr.OnSuccess)
	}
	if rr.ConfirmTyped == nil || rr.ConfirmTyped.Field != "name" || rr.Tone != "danger" || !rr.Confirm {
		t.Fatalf("confirm_typed/tone/confirm: %+v %q %v", rr.ConfirmTyped, rr.Tone, rr.Confirm)
	}

	ret := findAction(t, defs, "retire")
	if ret.Trigger == nil || ret.Trigger.Type != "set_fields" || ret.Trigger.Set["retired_at"] != "$now" || ret.Trigger.Set["retired_reason"] != "$payload.reason" {
		t.Fatalf("set_fields trigger: %+v", ret.Trigger)
	}

	var inst *manifest.ModelDefinition
	for i := range legacy.ModelDefinitions {
		if legacy.ModelDefinitions[i].ModelKey == "Instance" {
			inst = &legacy.ModelDefinitions[i]
		}
	}
	if inst == nil || inst.Ingest == nil || !inst.Ingest.Exclusive || inst.Ingest.Cursor != "source_seq" || inst.Ingest.MaxBatch != 500 || inst.Ingest.OnDelete != "soft" {
		t.Fatalf("ingest not projected: %+v", inst)
	}
	if len(legacy.Webhooks) != 1 || legacy.Webhooks[0].Do != "upsert:Instance" || legacy.Webhooks[0].Verify != "metacore-hmac-v1" {
		t.Fatalf("upsert webhook: %+v", legacy.Webhooks)
	}

	// Dual validation: the strict legacy plane accepts what v3 accepted.
	if err := legacy.Validate("3.5.0"); err != nil {
		t.Fatalf("legacy plane rejected a v3-valid manifest: %v", err)
	}
}

// TestFromV3_HubPrimitives_RoundTrip: v3 -> ActionDef -> JSON -> modelbase.ActionDef
// -> JSON -> ActionDef loses nothing (same JSON tags on both twins).
func TestFromV3_HubPrimitives_RoundTrip(t *testing.T) {
	_, legacy := hubFixture(t)
	for _, key := range []string{"request_revoke", "retire"} {
		def := findAction(t, legacy.Actions["Instance"], key)
		raw, err := json.Marshal(def)
		if err != nil {
			t.Fatal(err)
		}
		var served modelbase.ActionDef
		if err := json.Unmarshal(raw, &served); err != nil {
			t.Fatal(err)
		}
		raw2, err := json.Marshal(served)
		if err != nil {
			t.Fatal(err)
		}
		var back manifest.ActionDef
		if err := json.Unmarshal(raw2, &back); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(def.Trigger, back.Trigger) {
			t.Fatalf("%s: trigger changed across the round-trip:\n  before %+v\n  after  %+v", key, def.Trigger, back.Trigger)
		}
		if !reflect.DeepEqual(def.ConfirmTyped, back.ConfirmTyped) || def.Tone != back.Tone || def.Confirm != back.Confirm {
			t.Fatalf("%s: confirm_typed/tone lost: %+v/%q vs %+v/%q", key, def.ConfirmTyped, def.Tone, back.ConfirmTyped, back.Tone)
		}
	}

	// Model-level: ingest survives a JSON round-trip of the host manifest.
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	var back manifest.Manifest
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	for i := range legacy.ModelDefinitions {
		if !reflect.DeepEqual(legacy.ModelDefinitions[i].Ingest, back.ModelDefinitions[i].Ingest) {
			t.Fatalf("model %s: ingest changed across the round-trip", legacy.ModelDefinitions[i].ModelKey)
		}
	}
}

// TestHubPrimitives_ServedDropsDispatchConfig: only the trigger type may reach
// the browser.
func TestHubPrimitives_ServedDropsDispatchConfig(t *testing.T) {
	_, legacy := hubFixture(t)
	rr := findAction(t, legacy.Actions["Instance"], "request_revoke")
	raw, _ := json.Marshal(rr)
	var served modelbase.ActionDef
	if err := json.Unmarshal(raw, &served); err != nil {
		t.Fatal(err)
	}
	served.Trigger = served.Trigger.Served()
	out, _ := json.Marshal(served)
	s := string(out)
	for _, leak := range []string{"url_ref", "trust_core", "signing", "secret", "path", "send_record", "on_success", "license_status"} {
		if strings.Contains(s, leak) {
			t.Fatalf("served action leaks %q: %s", leak, s)
		}
	}
	if !strings.Contains(s, `"trigger":{"type":"webhook"}`) || !strings.Contains(s, `"confirmTyped":{"field":"name"}`) || !strings.Contains(s, `"tone":"danger"`) {
		t.Fatalf("served action lost presentation fields: %s", s)
	}
	if (*modelbase.ActionTrigger)(nil).Served() != nil {
		t.Fatal("nil trigger must stay nil")
	}
}

// ---- legacy plane (install-time) rejections ----

func TestValidate_HubPrimitivesTriggers(t *testing.T) {
	base := func() manifest.Manifest {
		_, legacy := hubFixture(t)
		return legacy
	}
	with := func(edit func(m *manifest.Manifest)) error {
		m := base()
		edit(&m)
		return m.Validate("3.5.0")
	}
	trig := func(m *manifest.Manifest, key string) *manifest.ActionTrigger {
		defs := m.Actions["Instance"]
		for i := range defs {
			if defs[i].Key == key {
				return defs[i].Trigger
			}
		}
		t.Fatalf("no action %s", key)
		return nil
	}
	cases := []struct {
		name string
		edit func(m *manifest.Manifest)
		want string
	}{
		{"two url sources", func(m *manifest.Manifest) { trig(m, "request_revoke").URL = "https://x.example.com" }, "mutually exclusive"},
		{"bad url_ref", func(m *manifest.Manifest) { trig(m, "request_revoke").URLRef = "nope" }, "url_ref"},
		{"hmac without secret_ref", func(m *manifest.Manifest) { trig(m, "request_revoke").Signing.SecretRef = "" }, "secret_ref"},
		{"ed25519 with secret_ref", func(m *manifest.Manifest) {
			s := trig(m, "request_revoke").Signing
			s.Scheme = "instance-ed25519-v1"
		}, "not allowed with instance-ed25519-v1"},
		{"bad method", func(m *manifest.Manifest) { trig(m, "request_revoke").Method = "GET" }, "method"},
		{"timeout", func(m *manifest.Manifest) { trig(m, "request_revoke").TimeoutMS = 5 }, "timeout_ms"},
		{"retries", func(m *manifest.Manifest) { n := 7; trig(m, "request_revoke").Retries = &n }, "retries"},
		{"on_success bad token", func(m *manifest.Manifest) { trig(m, "request_revoke").OnSuccess.Set["license_status"] = "$nope" }, "unknown token"},
		{"set_fields bad token", func(m *manifest.Manifest) { trig(m, "retire").Set["status"] = "$nope" }, "unknown token"},
		{"set_fields managed column", func(m *manifest.Manifest) { trig(m, "retire").Set["organization_id"] = "x" }, "managed by the kernel"},
		{"set_fields empty", func(m *manifest.Manifest) { trig(m, "retire").Set = map[string]any{} }, "set"},
		{"webhook fields on set_fields", func(m *manifest.Manifest) { trig(m, "retire").URL = "https://x.example.com" }, "only allowed when type=webhook"},
		{"set on webhook", func(m *manifest.Manifest) { trig(m, "request_revoke").Set = map[string]any{"a": 1} }, "only allowed when type=set_fields"},
		{"upsert to foreign model", func(m *manifest.Manifest) { m.Webhooks[0].Do = "upsert:Other" }, "not a model of this manifest"},
		{"upsert without ingest", func(m *manifest.Manifest) {
			for i := range m.ModelDefinitions {
				m.ModelDefinitions[i].Ingest = nil
			}
		}, "no ingest block"},
		{"upsert with verify addon", func(m *manifest.Manifest) { m.Webhooks[0].Verify = "addon" }, "needs verify"},
		{"ingest key size", func(m *manifest.Manifest) {
			for i := range m.ModelDefinitions {
				if m.ModelDefinitions[i].Ingest != nil {
					m.ModelDefinitions[i].Ingest.Key = nil
				}
			}
		}, "ingest.key"},
		{"empty confirmTyped field", func(m *manifest.Manifest) {
			defs := m.Actions["Instance"]
			for i := range defs {
				if defs[i].Key == "retire" {
					defs[i].ConfirmTyped.Field = ""
				}
			}
		}, "confirmTyped"},
		{"bad tone", func(m *manifest.Manifest) {
			defs := m.Actions["Instance"]
			for i := range defs {
				if defs[i].Key == "retire" {
					defs[i].Tone = "loud"
				}
			}
		}, "tone"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := with(c.edit)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to contain %q", err, c.want)
			}
		})
	}
}
