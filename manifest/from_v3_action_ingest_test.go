package manifest_test

import (
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

const ingestActionJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "billing", "name": "Billing", "version": "1.0.0" },
  "compatibility": { "requires": [{ "key": "kernel", "version": ">=3.0.0 <4.0.0" }] },
  "contributions": {
    "actions": [
      { "key": "mark_paid", "label": "Mark paid", "target_model": "invoice", "ingest": true,
        "handler": { "type": "wasm", "function": "MarkPaid" } },
      { "key": "capture", "label": "Capture", "target_model": "invoice",
        "handler": { "type": "wasm", "function": "Capture" } }
    ]
  }
}`

func TestFromV3_ActionIngest_MapsThrough(t *testing.T) {
	m, err := v3.Parse([]byte(ingestActionJSON))
	if err != nil {
		t.Fatalf("v3 parse: %v", err)
	}
	defs := manifest.FromV3(m).Actions["invoice"]
	if len(defs) != 2 {
		t.Fatalf("want 2 actions, got %d", len(defs))
	}
	if !defs[0].Ingest {
		t.Fatalf("ingest did not map through: %+v", defs[0])
	}
	if defs[1].Ingest {
		t.Fatal("an action without ingest must stay closed")
	}
}

func TestFromV3_ActionIngest_WithApprovalRejected(t *testing.T) {
	bad := strings.Replace(ingestActionJSON, `"ingest": true,`,
		`"ingest": true, "approval": { "roles": ["manager"] },`, 1)
	if _, err := v3.Parse([]byte(bad)); err == nil {
		t.Fatal("v3.Parse accepted ingest:true together with approval")
	}
}
