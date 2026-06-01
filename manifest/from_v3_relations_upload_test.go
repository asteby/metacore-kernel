package manifest_test

import (
	"encoding/json"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
	"github.com/asteby/metacore-kernel/modelbase"
)

// crmManifestJSON declares a Customer model with the inverse relations a detail
// page renders (vehicles, addresses, a polymorphic attachments child and an N:M
// tags edge) plus an action with an `upload` field. It exercises the additive
// v3 contract for declarative cross-module relations and file uploads.
const crmManifestJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "crm_lite", "name": "CRM", "version": "0.2.0" },
  "compatibility": { "requires": [{ "key": "kernel", "version": ">=3.0.0 <4.0.0" }] },
  "models": [
    {
      "key": "Customer",
      "table": "customers",
      "columns": [
        { "name": "id", "type": "uuid", "primary_key": true },
        { "name": "name", "type": "text", "not_null": true }
      ],
      "relations": [
        { "name": "vehicles", "kind": "one_to_many", "through": "Vehicle", "foreign_key": "customer_id", "label": "Vehículos" },
        { "name": "addresses", "kind": "one_to_many", "through": "Address", "foreign_key": "customer_id" },
        {
          "name": "attachments",
          "kind": "one_to_many",
          "through": "Attachment",
          "foreign_key": "owner_id",
          "scope": { "owner_model": "Customer" }
        },
        { "name": "tags", "kind": "many_to_many", "through": "Tag", "foreign_key": "customer_id" }
      ]
    }
  ],
  "contributions": {
    "actions": [
      {
        "key": "attach_file",
        "label": "Adjuntar archivo",
        "target_model": "Customer",
        "handler": { "type": "webhook", "url": "https://example.com/attach" },
        "fields": [
          {
            "key": "file",
            "label": "Archivo",
            "type": "upload",
            "required": true,
            "accept": "image/*,.pdf",
            "max_size": 10485760,
            "storage_path": "attachments/customers"
          }
        ]
      }
    ]
  }
}`

func TestV3Parse_AcceptsRelationsAndUpload(t *testing.T) {
	m, err := v3.Parse([]byte(crmManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse: %v", err)
	}
	if len(m.Models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(m.Models))
	}
	rels := m.Models[0].Relations
	if len(rels) != 4 {
		t.Fatalf("expected 4 relations, got %d", len(rels))
	}
	if rels[2].Name != "attachments" || rels[2].Scope["owner_model"] != "Customer" {
		t.Errorf("polymorphic scope lost on parse: %+v", rels[2])
	}
	// Upload field props survive parse.
	f := m.Contributions.Actions[0].Fields[0]
	if f.Type != "upload" || f.Accept != "image/*,.pdf" || f.MaxSize != 10485760 || f.StoragePath != "attachments/customers" {
		t.Errorf("upload field props lost on parse: %+v", f)
	}
}

func TestFromV3_MapsRelationsAndUpload(t *testing.T) {
	m, err := v3.Parse([]byte(crmManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse: %v", err)
	}
	out := manifest.FromV3(m)

	// 1. Relations land on the legacy ModelDefinition.
	if len(out.ModelDefinitions) != 1 {
		t.Fatalf("expected 1 model def, got %d", len(out.ModelDefinitions))
	}
	def := out.ModelDefinitions[0]
	if len(def.Relations) != 4 {
		t.Fatalf("expected 4 relations on ModelDefinition, got %d", len(def.Relations))
	}
	byName := map[string]manifest.RelationDef{}
	for _, r := range def.Relations {
		byName[r.Name] = r
	}
	veh := byName["vehicles"]
	if veh.Kind != "one_to_many" || veh.Through != "Vehicle" || veh.ForeignKey != "customer_id" || veh.Label != "Vehículos" {
		t.Errorf("vehicles relation mismatched: %+v", veh)
	}
	att := byName["attachments"]
	if att.Scope["owner_model"] != "Customer" {
		t.Errorf("polymorphic scope lost in FromV3: %+v", att)
	}
	if byName["tags"].Kind != "many_to_many" {
		t.Errorf("tags many_to_many lost: %+v", byName["tags"])
	}

	// 2. Upload field props land on the legacy FieldDef.
	fields := out.Actions["Customer"][0].Fields
	if len(fields) != 1 {
		t.Fatalf("expected 1 field, got %d", len(fields))
	}
	up := fields[0]
	if up.Key != "file" || up.Type != "upload" {
		t.Errorf("upload field key/type lost: %+v", up)
	}
	if up.Accept != "image/*,.pdf" || up.MaxSize != 10485760 || up.StoragePath != "attachments/customers" {
		t.Errorf("upload props lost in FromV3: %+v", up)
	}
}

func TestFromV3_UploadField_HostRoundTrip(t *testing.T) {
	m, err := v3.Parse([]byte(crmManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse: %v", err)
	}
	out := manifest.FromV3(m)

	// The host serves modelbase.ActionDef verbatim (json.Marshal). Confirm the
	// upload props survive the kernel ActionDef → host ActionDef JSON round-trip.
	raw, err := json.Marshal(out.Actions)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var host map[string][]modelbase.ActionDef
	if err := json.Unmarshal(raw, &host); err != nil {
		t.Fatalf("unmarshal into modelbase: %v", err)
	}
	hf := host["Customer"][0].Fields[0]
	if hf.Type != "upload" || hf.Accept != "image/*,.pdf" || hf.MaxSize != 10485760 || hf.StoragePath != "attachments/customers" {
		t.Errorf("host lost upload props: %+v", hf)
	}
}

func TestModelDefinitionRelations_HostMetadataRoundTrip(t *testing.T) {
	// A manifest.RelationDef must survive marshalling to and from the host's
	// modelbase.RelationMeta (the served TableMetadata.Relations shape) so a
	// bridge that re-emits relations onto metadata keeps every field, including
	// the polymorphic scope.
	src := manifest.RelationDef{
		Name:       "attachments",
		Kind:       "one_to_many",
		Through:    "Attachment",
		ForeignKey: "owner_id",
		Scope:      map[string]string{"owner_model": "Customer"},
		Label:      "Adjuntos",
	}
	raw, err := json.Marshal(src)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var meta modelbase.RelationMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("unmarshal into RelationMeta: %v", err)
	}
	if meta.Name != "attachments" || meta.Kind != "one_to_many" || meta.Through != "Attachment" ||
		meta.ForeignKey != "owner_id" || meta.Scope["owner_model"] != "Customer" || meta.Label != "Adjuntos" {
		t.Errorf("RelationDef → RelationMeta round-trip dropped a field: %+v", meta)
	}
}
