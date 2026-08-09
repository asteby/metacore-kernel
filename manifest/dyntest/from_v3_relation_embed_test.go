package dyntest

import (
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// embedManifestJSON declares an Order with TWO one_to_many relations: `items`
// is a COMPOSITION (the document's lines, embedded in the record modal) and
// `movements` points at a large, independently-managed ledger that must NOT be
// dragged into the modal.
const embedManifestJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "sales", "name": "Sales", "version": "1.0.0" },
  "compatibility": { "requires": [ { "key": "kernel", "version": ">=0.1.0" } ] },
  "models": [
    {
      "key": "Order",
      "table": "orders",
      "label": "Orders",
      "columns": [
        { "name": "id", "type": "uuid", "primary_key": true },
        { "name": "organization_id", "type": "uuid", "not_null": true },
        { "name": "code", "type": "text" }
      ],
      "relations": [
        {
          "name": "items",
          "kind": "one_to_many",
          "through": "OrderItem",
          "foreign_key": "order_id",
          "embed": true
        },
        {
          "name": "movements",
          "kind": "one_to_many",
          "through": "StockMovement",
          "foreign_key": "order_id"
        }
      ]
    },
    {
      "key": "OrderItem",
      "table": "order_items",
      "label": "Order items",
      "columns": [
        { "name": "id", "type": "uuid", "primary_key": true },
        { "name": "organization_id", "type": "uuid", "not_null": true },
        { "name": "order_id", "type": "uuid" }
      ]
    },
    {
      "key": "StockMovement",
      "table": "stock_movements",
      "label": "Stock movements",
      "columns": [
        { "name": "id", "type": "uuid", "primary_key": true },
        { "name": "organization_id", "type": "uuid", "not_null": true },
        { "name": "order_id", "type": "uuid" }
      ]
    }
  ]
}`

// TestRelationEmbedParseAndProject walks the relation `embed` flag through the
// kernel-side chain: v3.Parse (strict jsonschema + typed decode) → FromV3
// (legacy carrier) → legacy Validate → the served RelationMeta the SDK reads to
// decide which sub-tables the record modal embeds.
func TestRelationEmbedParseAndProject(t *testing.T) {
	// (a) strict schema + typed decode accepts and carries embed.
	m, err := v3.Parse([]byte(embedManifestJSON))
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
	if v3rels["movements"].Embed {
		t.Errorf("v3 movements.Embed = true, want false (embed is opt-in)")
	}

	// (b) FromV3 carries it onto the legacy RelationDef; (c) legacy Validate agrees.
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
	if legacy["movements"].Embed {
		t.Errorf("legacy movements.Embed = true, want false")
	}
	if err := host.Validate("2.0.0"); err != nil {
		t.Fatalf("legacy Validate rejected converted manifest: %v", err)
	}

	// (d) the served RelationMeta projection is covered in
	// metadata.TestService_ProjectsRelationEmbedOntoTableMetadata.
}
