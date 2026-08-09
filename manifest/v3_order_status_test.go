package manifest_test

import (
	"encoding/json"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// orderStatusManifestJSON exercises the declarative order-status routing
// surface: an action gated on requires_state and a nav entry carrying a static
// status filter. Both are additive v3 fields the strict schema must accept.
const orderStatusManifestJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "orders", "name": "Orders", "version": "1.0.0" },
  "compatibility": { "requires": [{ "key": "kernel", "version": ">=3.0.0 <4.0.0" }] },
  "models": [
    {
      "key": "Order",
      "table": "orders",
      "columns": [
        { "name": "id", "type": "uuid", "primary_key": true, "default": "gen_random_uuid()" },
        { "name": "organization_id", "type": "uuid", "not_null": true },
        { "name": "status", "type": "text", "not_null": true }
      ]
    }
  ],
  "contributions": {
    "navigation": [
      {
        "title": "Orders",
        "items": [
          { "title": "En recepción", "model": "Order", "filter": { "status": "reception" } },
          { "title": "Todas", "model": "Order" }
        ]
      }
    ],
    "actions": [
      {
        "key": "receive",
        "label": "Recibir",
        "target_model": "orders",
        "handler": { "type": "wasm", "function": "receive" },
        "requires_state": ["reception", "inspecting"]
      },
      {
        "key": "note",
        "label": "Nota",
        "target_model": "orders",
        "handler": { "type": "wasm", "function": "note" }
      }
    ]
  }
}`

func TestParse_RequiresState_And_NavFilter_Accepted(t *testing.T) {
	m, err := v3.Parse([]byte(orderStatusManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse rejected manifest with requires_state/nav filter: %v", err)
	}

	if m.Contributions == nil {
		t.Fatal("contributions missing")
	}

	// requires_state survives the parse on the gated action and is absent on the
	// ungated one.
	var receive, note *v3.Action
	for i := range m.Contributions.Actions {
		switch m.Contributions.Actions[i].Key {
		case "receive":
			receive = &m.Contributions.Actions[i]
		case "note":
			note = &m.Contributions.Actions[i]
		}
	}
	if receive == nil || note == nil {
		t.Fatalf("expected both actions, got %+v", m.Contributions.Actions)
	}
	if len(receive.RequiresState) != 2 || receive.RequiresState[0] != "reception" || receive.RequiresState[1] != "inspecting" {
		t.Errorf("receive.RequiresState = %v, want [reception inspecting]", receive.RequiresState)
	}
	if len(note.RequiresState) != 0 {
		t.Errorf("note.RequiresState = %v, want empty", note.RequiresState)
	}

	// nav filter survives the parse on the filtered entry and is absent on the
	// unfiltered one.
	if len(m.Contributions.Navigation) != 1 || len(m.Contributions.Navigation[0].Items) != 2 {
		t.Fatalf("unexpected navigation shape: %+v", m.Contributions.Navigation)
	}
	items := m.Contributions.Navigation[0].Items
	if got := items[0].Filter["status"]; got != "reception" {
		t.Errorf("filtered nav item filter[status] = %q, want reception", got)
	}
	if items[1].Filter != nil {
		t.Errorf("unfiltered nav item filter = %v, want nil", items[1].Filter)
	}
}

func TestFromV3_RequiresState_And_NavFilter_Mapped(t *testing.T) {
	m, err := v3.Parse([]byte(orderStatusManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse: %v", err)
	}
	out := manifest.FromV3(m)

	// requires_state maps onto ActionDef.RequiresState, keyed by target model.
	defs := out.Actions["orders"]
	if len(defs) != 2 {
		t.Fatalf("expected 2 actions under model orders, got %d (%+v)", len(defs), out.Actions)
	}
	var receive, note *manifest.ActionDef
	for i := range defs {
		switch defs[i].Key {
		case "receive":
			receive = &defs[i]
		case "note":
			note = &defs[i]
		}
	}
	if receive == nil || note == nil {
		t.Fatalf("missing mapped actions: %+v", defs)
	}
	if len(receive.RequiresState) != 2 || receive.RequiresState[0] != "reception" {
		t.Errorf("mapped receive.RequiresState = %v, want [reception inspecting]", receive.RequiresState)
	}
	if len(note.RequiresState) != 0 {
		t.Errorf("mapped note.RequiresState = %v, want empty (additive)", note.RequiresState)
	}

	// nav filter maps onto NavItem.Filter.
	if len(out.Navigation) != 1 || len(out.Navigation[0].Items) != 2 {
		t.Fatalf("unexpected mapped navigation: %+v", out.Navigation)
	}
	mitems := out.Navigation[0].Items
	if got := mitems[0].Filter["status"]; got != "reception" {
		t.Errorf("mapped nav filter[status] = %q, want reception", got)
	}
	if mitems[1].Filter != nil {
		t.Errorf("mapped unfiltered nav item filter = %v, want nil", mitems[1].Filter)
	}
}

// TestNavItem_Filter_JSONRoundTrip asserts the runtime NavItem serialises its
// filter back out (host serves it to the SDK) and omits it when empty.
func TestNavItem_Filter_JSONRoundTrip(t *testing.T) {
	in := manifest.NavItem{Title: "En recepción", Model: "Order", Filter: map[string]string{"status": "reception"}}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back manifest.NavItem
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Filter["status"] != "reception" {
		t.Errorf("round-tripped filter = %v", back.Filter)
	}

	// Empty filter must be omitted (omitempty), so existing nav payloads are byte
	// identical to before this field existed.
	bare, _ := json.Marshal(manifest.NavItem{Title: "Todas"})
	if string(bare) != `{"title":"Todas"}` {
		t.Errorf("bare nav item JSON = %s, want no filter key", string(bare))
	}
}
