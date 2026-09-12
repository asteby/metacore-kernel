package dyntest

import (
	"encoding/json"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
	"github.com/asteby/metacore-kernel/modelbase"
)

// Federated custom modal slot must survive v3 → host → modelbase JSON so the
// SDK can fail closed when the remote is missing (no generic confirm fallback).
const customModalManifestJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "credit_approval", "name": "Credit", "version": "0.1.0" },
  "compatibility": { "requires": [{ "key": "kernel", "version": ">=3.0.0 <4.0.0" }] },
  "contributions": {
    "actions": [
      {
        "key": "authorize_credit",
        "label": "Autorizar a crédito",
        "target_model": "SalesOrder",
        "handler": { "type": "wasm", "function": "handle_SalesOrder_authorize_credit" },
        "placement": "row",
        "modal": "credit_approval.authorize_credit",
        "confirm": true,
        "confirm_message": "customers.action.authorize_credit_confirm_message"
      }
    ]
  }
}`

func TestFromV3_ActionModalSurvivesHostRoundTrip(t *testing.T) {
	m, err := v3.Parse([]byte(customModalManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse: %v", err)
	}
	out := manifest.FromV3(m)
	acts := out.Actions["SalesOrder"]
	if len(acts) != 1 {
		t.Fatalf("actions len=%d want 1", len(acts))
	}
	if acts[0].Modal != "credit_approval.authorize_credit" {
		t.Fatalf("Modal=%q", acts[0].Modal)
	}
	// Custom modal ⇒ do not invent Confirm even if confirm/confirm_message set.
	if acts[0].Confirm {
		t.Fatalf("Confirm should be false when Modal is set; got true")
	}

	raw, err := json.Marshal(out.Actions)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var host map[string][]modelbase.ActionDef
	if err := json.Unmarshal(raw, &host); err != nil {
		t.Fatalf("unmarshal modelbase: %v", err)
	}
	hf := host["SalesOrder"][0]
	if hf.Modal != "credit_approval.authorize_credit" {
		t.Fatalf("host Modal dropped: %+v", hf)
	}
	if hf.Confirm {
		t.Fatalf("host Confirm should stay false for modal actions")
	}
}
