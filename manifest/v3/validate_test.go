package v3

import (
	"encoding/json"
	"strings"
	"testing"
)

// baseValid is a minimal but complete v3 Addon manifest used as the seed
// for every test case. Tests mutate a parsed copy to exercise one rule
// at a time.
func baseValid() map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "asteby.com/v3",
		"kind":       "Addon",
		"metadata": map[string]interface{}{
			"key":     "inventory",
			"name":    "Inventory",
			"version": "1.0.0",
		},
		"compatibility": map[string]interface{}{
			"requires": []interface{}{
				map[string]interface{}{
					"key":     "kernel",
					"version": ">=3.0.0 <4.0.0",
				},
			},
		},
		"tenancy": map[string]interface{}{
			"isolation":  "shared",
			"rls_column": "organization_id",
		},
		"capabilities": []interface{}{
			map[string]interface{}{
				"kind":   "db:write",
				"target": "addon_inventory.*",
			},
		},
	}
}

func mustJSON(t *testing.T, v interface{}) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestValidate_ValidManifest(t *testing.T) {
	if err := Validate(mustJSON(t, baseValid())); err != nil {
		t.Fatalf("expected valid manifest, got error: %v", err)
	}
}

func TestValidate_MissingAPIVersion(t *testing.T) {
	m := baseValid()
	delete(m, "apiVersion")

	err := Validate(mustJSON(t, m))
	if err == nil {
		t.Fatal("expected error for missing apiVersion, got nil")
	}
	if !strings.Contains(err.Error(), "apiVersion") {
		t.Fatalf("expected error to mention apiVersion, got: %v", err)
	}
}

func TestValidate_UnknownKind(t *testing.T) {
	m := baseValid()
	m["kind"] = "Sidecar" // not in the enum

	err := Validate(mustJSON(t, m))
	if err == nil {
		t.Fatal("expected error for unknown kind, got nil")
	}
	// jsonschema reports the enum violation; either wording is acceptable
	// but it must mention the offending field.
	if !strings.Contains(err.Error(), "kind") {
		t.Fatalf("expected error to mention kind, got: %v", err)
	}
}

func TestValidate_InvalidSemverInRequires(t *testing.T) {
	m := baseValid()
	m["compatibility"] = map[string]interface{}{
		"requires": []interface{}{
			map[string]interface{}{
				"key":     "kernel",
				"version": "not-a-semver-range",
			},
		},
	}

	err := Validate(mustJSON(t, m))
	if err == nil {
		t.Fatal("expected error for invalid semver range, got nil")
	}
	if !strings.Contains(err.Error(), "semver") && !strings.Contains(err.Error(), "version") {
		t.Fatalf("expected error to mention semver/version, got: %v", err)
	}
}

func TestValidate_EmptyDocument(t *testing.T) {
	if err := Validate(nil); err == nil {
		t.Fatal("expected error for empty manifest, got nil")
	}
	if err := Validate([]byte(``)); err == nil {
		t.Fatal("expected error for empty manifest, got nil")
	}
}

func TestValidate_PresetWithoutPresetBlock(t *testing.T) {
	m := baseValid()
	m["kind"] = "Preset"
	// no preset block — schema-level allOf should catch this.

	err := Validate(mustJSON(t, m))
	if err == nil {
		t.Fatal("expected error for Preset without preset block, got nil")
	}
}

func TestValidate_AddonWithPresetBlockRejected(t *testing.T) {
	m := baseValid()
	m["preset"] = map[string]interface{}{
		"addons": []interface{}{
			map[string]interface{}{"key": "inventory", "version": "^1.0.0"},
		},
	}

	err := Validate(mustJSON(t, m))
	if err == nil {
		t.Fatal("expected error for Addon with preset block, got nil")
	}
}

func TestValidate_UnknownCapabilityKindRejected(t *testing.T) {
	m := baseValid()
	m["capabilities"] = []interface{}{
		map[string]interface{}{
			"kind":   "db:drop-database", // not in the closed enum
			"target": "*",
		},
	}

	err := Validate(mustJSON(t, m))
	if err == nil {
		t.Fatal("expected error for unknown capability kind, got nil")
	}
}

func TestParse_ReturnsTypedManifestOnSuccess(t *testing.T) {
	got, err := Parse(mustJSON(t, baseValid()))
	if err != nil {
		t.Fatalf("Parse returned unexpected error: %v", err)
	}
	if got.APIVersion != APIVersion {
		t.Fatalf("APIVersion = %q, want %q", got.APIVersion, APIVersion)
	}
	if got.Kind != KindAddon {
		t.Fatalf("Kind = %q, want %q", got.Kind, KindAddon)
	}
	if got.Metadata.Key != "inventory" {
		t.Fatalf("Metadata.Key = %q, want %q", got.Metadata.Key, "inventory")
	}
}

func TestParse_ActionWithLineItemsField(t *testing.T) {
	m := baseValid()
	m["contributions"] = map[string]interface{}{
		"actions": []interface{}{
			map[string]interface{}{
				"key":          "receive_goods",
				"label":        "Recibir mercancía",
				"target_model": "purchase_order",
				"handler": map[string]interface{}{
					"type":     "wasm",
					"function": "ReceiveGoods",
				},
				"fields": []interface{}{
					map[string]interface{}{
						"key":   "lines",
						"label": "Renglones",
						"type":  "array",
						"item_fields": []interface{}{
							map[string]interface{}{
								"key":   "product_id",
								"label": "Producto",
								"type":  "select",
								"ref":   "product",
							},
							map[string]interface{}{
								"key":      "quantity",
								"label":    "Cantidad",
								"type":     "number",
								"required": true,
							},
						},
					},
				},
			},
		},
	}

	got, err := Parse(mustJSON(t, m))
	if err != nil {
		t.Fatalf("Parse returned unexpected error: %v", err)
	}
	if got.Contributions == nil || len(got.Contributions.Actions) != 1 {
		t.Fatalf("expected 1 action, got %+v", got.Contributions)
	}
	fields := got.Contributions.Actions[0].Fields
	if len(fields) != 1 {
		t.Fatalf("expected 1 field, got %d", len(fields))
	}
	lines := fields[0]
	if lines.Type != "array" {
		t.Fatalf("line-items field Type = %q, want %q", lines.Type, "array")
	}
	if len(lines.ItemFields) != 2 {
		t.Fatalf("expected 2 item_fields, got %d", len(lines.ItemFields))
	}
	if lines.ItemFields[0].Key != "product_id" || lines.ItemFields[0].Ref != "product" {
		t.Fatalf("item_fields[0] = %+v, want product_id/product", lines.ItemFields[0])
	}
	if lines.ItemFields[1].Key != "quantity" || !lines.ItemFields[1].Required {
		t.Fatalf("item_fields[1] = %+v, want required quantity", lines.ItemFields[1])
	}
}

func TestSchemaJSON_ReturnsEmbeddedBytes(t *testing.T) {
	b := SchemaJSON()
	if len(b) == 0 {
		t.Fatal("expected embedded schema bytes, got 0 length")
	}
	if !strings.Contains(string(b), "asteby.com/v3") {
		t.Fatal("embedded schema does not look like the v3 schema")
	}
}

// TestParse_MetadataI18n confirms the strict v3 schema accepts metadata.i18n
// (marketplace catalog localizations) and Parse returns them typed. Without the
// schema/type entry, additionalProperties:false would reject the field.
func TestParse_MetadataI18n(t *testing.T) {
	m := baseValid()
	md := m["metadata"].(map[string]interface{})
	md["description"] = "Foundation addon for inventory management."
	md["i18n"] = map[string]interface{}{
		"es": map[string]interface{}{
			"name":        "Inventario",
			"description": "Addon base para gestión de inventario.",
			"features":    []interface{}{"Productos", "Almacenes"},
		},
		"en": map[string]interface{}{
			"name":        "Inventory",
			"description": "Foundation addon for inventory management.",
		},
	}

	got, err := Parse(mustJSON(t, m))
	if err != nil {
		t.Fatalf("Parse rejected metadata.i18n: %v", err)
	}
	es, ok := got.Metadata.I18n["es"]
	if !ok {
		t.Fatalf("expected es locale, got %+v", got.Metadata.I18n)
	}
	if es.Name != "Inventario" || es.Description == "" || len(es.Features) != 2 {
		t.Fatalf("es locale not mapped: %+v", es)
	}
}

// TestParse_MetadataCountries confirms metadata.countries (market scoping)
// is accepted by the strict v3 schema and parsed onto the typed manifest.
func TestParse_MetadataCountries(t *testing.T) {
	m := baseValid()
	md := m["metadata"].(map[string]interface{})
	md["countries"] = []interface{}{"MX"}
	got, err := Parse(mustJSON(t, m))
	if err != nil {
		t.Fatalf("Parse rejected metadata.countries: %v", err)
	}
	if len(got.Metadata.Countries) != 1 || got.Metadata.Countries[0] != "MX" {
		t.Fatalf("countries not parsed: %+v", got.Metadata.Countries)
	}
}
