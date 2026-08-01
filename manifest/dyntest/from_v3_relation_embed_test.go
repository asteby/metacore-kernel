package dyntest

import (
	"encoding/json"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
	"github.com/asteby/metacore-kernel/modelbase"
)

// relationEmbedManifestJSON declares an Order with two one_to_many relations:
// `items` is a COMPOSITION (the document's lines, embedded in the record modal)
// while `shipments` is a plain association that must NOT be dragged into the
// modal.
const relationEmbedManifestJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "sales", "name": "Sales", "version": "0.1.0" },
  "compatibility": { "requires": [ { "key": "kernel", "version": ">=0.1.0" } ] },
  "models": [
    {
      "key": "Order",
      "table": "sales_orders",
      "label": "Orders",
      "columns": [
        { "name": "id", "type": "uuid", "primary_key": true },
        { "name": "folio", "type": "text", "not_null": true }
      ],
      "relations": [
        { "name": "items", "kind": "one_to_many", "through": "OrderItem", "foreign_key": "order_id", "embed": true },
        { "name": "shipments", "kind": "one_to_many", "through": "Shipment", "foreign_key": "order_id" }
      ]
    }
  ]
}`

// TestRelationEmbedParseAndProject walks the relation `embed` flag through the
// kernel-side chain: v3.Parse (strict jsonschema + typed decode) → FromV3
// (legacy carrier) → legacy Validate → the served RelationMeta JSON shape the
// SDK consumes. A relation that omits the flag must stay false, since embedding
// is opt-in.
func TestRelationEmbedParseAndProject(t *testing.T) {
	m, err := v3.Parse([]byte(relationEmbedManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse rejected manifest with relation embed: %v", err)
	}
	v3rels := map[string]v3.ModelRelation{}
	for _, r := range m.Models[0].Relations {
		v3rels[r.Name] = r
	}
	if !v3rels["items"].Embed {
		t.Errorf("v3 items.Embed = false, want true")
	}
	if v3rels["shipments"].Embed {
		t.Errorf("v3 shipments.Embed = true, want false (embedding is opt-in)")
	}

	host := manifest.FromV3(m)
	var def manifest.ModelDefinition
	for _, d := range host.ModelDefinitions {
		if d.ModelKey == "Order" {
			def = d
		}
	}
	legacy := map[string]manifest.RelationDef{}
	for _, r := range def.Relations {
		legacy[r.Name] = r
	}
	if !legacy["items"].Embed {
		t.Errorf("legacy items.Embed = false, want true")
	}
	if legacy["shipments"].Embed {
		t.Errorf("legacy shipments.Embed = true, want false")
	}
	if err := host.Validate("2.0.0"); err != nil {
		t.Fatalf("legacy Validate rejected converted manifest: %v", err)
	}

	// The served TableMetadata.Relations shape must keep the flag.
	raw, err := json.Marshal(legacy["items"])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var meta modelbase.RelationMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("unmarshal into RelationMeta: %v", err)
	}
	if !meta.Embed {
		t.Errorf("RelationDef → RelationMeta round-trip dropped embed: %+v", meta)
	}
}
