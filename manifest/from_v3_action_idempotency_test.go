package manifest_test

import (
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

const idempotentActionJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "billing", "name": "Billing", "version": "1.0.0" },
  "compatibility": { "requires": [{ "key": "kernel", "version": ">=3.0.0 <4.0.0" }] },
  "contributions": {
    "actions": [
      {
        "key": "capture",
        "label": "Capture Payment",
        "target_model": "invoice",
        "handler": { "type": "wasm", "function": "Capture" },
        "idempotency": { "key_field": "request_id" }
      }
    ]
  }
}`

func TestFromV3_ActionIdempotency_MapsThrough(t *testing.T) {
	m, err := v3.Parse([]byte(idempotentActionJSON))
	if err != nil {
		t.Fatalf("v3 parse: %v", err)
	}
	legacy := manifest.FromV3(m)
	defs := legacy.Actions["invoice"]
	if len(defs) != 1 {
		t.Fatalf("want 1 action, got %d", len(defs))
	}
	if defs[0].Idempotency == nil || defs[0].Idempotency.KeyField != "request_id" {
		t.Fatalf("idempotency did not map through: %+v", defs[0].Idempotency)
	}
	// The strict legacy validator must also accept it (dual-validation parity).
	if err := legacy.Validate("3.0.0"); err != nil {
		t.Fatalf("strict validate rejected a valid idempotent action: %v", err)
	}
}

func TestFromV3_ActionIdempotency_EmptyKeyFieldRejected(t *testing.T) {
	bad := strings.Replace(idempotentActionJSON,
		`"idempotency": { "key_field": "request_id" }`,
		`"idempotency": { "key_field": "" }`, 1)
	if _, err := v3.Parse([]byte(bad)); err == nil {
		t.Fatal("v3.Parse accepted an idempotency block with an empty key_field")
	}
}
