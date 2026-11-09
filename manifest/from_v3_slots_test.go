package manifest_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

const slotsFromV3JSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "slotdemo", "name": "Slot demo", "version": "1.0.0", "visibility": "private" },
  "compatibility": { "requires": [{ "key": "kernel", "version": ">=3.1.0 <4.0.0" }] },
  "tenancy": { "isolation": "shared", "rls_column": "organization_id" },
  "frontend": { "entry": "remoteEntry.js", "format": "module", "container": "slotdemo", "expose": "./RecordPanel" },
  "contributions": {
    "slots": [
      { "slot_kind": "record.panel", "entry": "./RecordPanel", "order": 10,
        "props": { "title": "Hola", "n": 2 }, "timeout_ms": 8000, "fallback": "notice" },
      { "slot_kind": "topbar.item", "entry": "./Badge" }
    ]
  }
}`

// The new slot fields survive v3 decode -> encode -> decode, and FromV3 (the
// legacy projection the host consumes) accepts a manifest that uses them. A
// private manifest without frontend.integrity is a warning, not an error, even
// in strict mode.
func TestFromV3_SlotFieldsRoundTrip(t *testing.T) {
	m, err := v3.Parse([]byte(slotsFromV3JSON))
	if err != nil {
		t.Fatalf("v3.Parse: a private manifest without integrity must not be an error: %v", err)
	}
	out := manifest.FromV3(m)
	if out.Key != "slotdemo" || out.Frontend == nil {
		t.Fatalf("FromV3 lost the manifest: %+v", out)
	}

	enc, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	m2, err := v3.Parse(enc)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if !reflect.DeepEqual(m.Contributions.Slots, m2.Contributions.Slots) {
		t.Fatalf("slots changed on round trip:\n%#v\n%#v", m.Contributions.Slots, m2.Contributions.Slots)
	}
	s := m2.Contributions.Slots[0]
	if s.TimeoutMS != 8000 || s.Fallback != "notice" || s.Props["title"] != "Hola" {
		t.Fatalf("new fields lost: %+v", s)
	}
	if got := manifest.FromV3(m2); got.Key != out.Key {
		t.Fatalf("FromV3 not stable across the round trip")
	}

	warns, err := v3.ValidateWithOptions([]byte(slotsFromV3JSON), v3.Options{Strict: true})
	if err != nil {
		t.Fatalf("strict: %v", err)
	}
	found := false
	for _, w := range warns {
		if len(w) > len("[private_frontend_integrity]") && w[:len("[private_frontend_integrity]")] == "[private_frontend_integrity]" {
			found = true
		}
	}
	if !found {
		t.Fatalf("want a private_frontend_integrity warning, got %v", warns)
	}
}
