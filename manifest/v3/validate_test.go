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

// actionWithBalance builds a baseValid manifest carrying one action whose
// declarative form has a line-items field with the given balance block.
func actionWithBalance(balance map[string]interface{}) map[string]interface{} {
	m := baseValid()
	field := map[string]interface{}{
		"key":  "lines",
		"type": "array",
		"item_fields": []interface{}{
			map[string]interface{}{"key": "account_id", "type": "string"},
			map[string]interface{}{"key": "debit", "type": "number", "total": true},
			map[string]interface{}{"key": "credit", "type": "number", "total": true},
		},
	}
	if balance != nil {
		field["balance"] = balance
	}
	m["contributions"] = map[string]interface{}{
		"actions": []interface{}{
			map[string]interface{}{
				"key":          "create_entry",
				"target_model": "Entry",
				"handler":      map[string]interface{}{"type": "wasm", "function": "OnCreate"},
				"fields":       []interface{}{field},
			},
		},
	}
	return m
}

func TestValidate_BalanceRule_Valid(t *testing.T) {
	m := actionWithBalance(map[string]interface{}{
		"debit_column":  "debit",
		"credit_column": "credit",
	})
	if err := Validate(mustJSON(t, m)); err != nil {
		t.Fatalf("expected valid balance rule, got error: %v", err)
	}
}

func TestValidate_BalanceRule_UnknownColumn(t *testing.T) {
	m := actionWithBalance(map[string]interface{}{
		"debit_column":  "debit",
		"credit_column": "nope",
	})
	err := Validate(mustJSON(t, m))
	if err == nil || !strings.Contains(err.Error(), "credit_column") {
		t.Fatalf("expected credit_column validation error, got: %v", err)
	}
}

func TestValidate_BalanceRule_RequiresItemFields(t *testing.T) {
	m := baseValid()
	m["contributions"] = map[string]interface{}{
		"actions": []interface{}{
			map[string]interface{}{
				"key":          "create_entry",
				"target_model": "Entry",
				"handler":      map[string]interface{}{"type": "wasm", "function": "OnCreate"},
				"fields": []interface{}{
					map[string]interface{}{
						"key":     "lines",
						"type":    "array",
						"balance": map[string]interface{}{"debit_column": "debit", "credit_column": "credit"},
					},
				},
			},
		},
	}
	err := Validate(mustJSON(t, m))
	if err == nil || !strings.Contains(err.Error(), "no item_fields") {
		t.Fatalf("expected no item_fields error, got: %v", err)
	}
}

func TestParse_BalanceRoundTrip(t *testing.T) {
	m := actionWithBalance(map[string]interface{}{
		"debit_column":  "debit",
		"credit_column": "credit",
		"message":       "Σdebit must equal Σcredit",
	})
	parsed, err := Parse(mustJSON(t, m))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	f := parsed.Contributions.Actions[0].Fields[0]
	if f.Balance == nil || f.Balance.DebitColumn != "debit" || f.Balance.CreditColumn != "credit" {
		t.Fatalf("balance not parsed: %+v", f.Balance)
	}
	if !f.ItemFields[1].Total {
		t.Fatalf("expected debit column total=true, got %+v", f.ItemFields[1])
	}
}

// modelWithSeed builds a baseValid Addon carrying one model whose columns
// include `code` and an optional seed block (key + rows).
func modelWithSeed(key string, rows []interface{}) map[string]interface{} {
	m := baseValid()
	model := map[string]interface{}{
		"key":   "PaymentMethod",
		"table": "payment_methods",
		"columns": []interface{}{
			map[string]interface{}{"name": "id", "type": "uuid", "primary_key": true},
			map[string]interface{}{"name": "name", "type": "text", "not_null": true},
			map[string]interface{}{"name": "code", "type": "text", "not_null": true},
			map[string]interface{}{"name": "is_active", "type": "boolean"},
			map[string]interface{}{"name": "sort_order", "type": "integer"},
		},
	}
	if key != "" || rows != nil {
		model["seed"] = map[string]interface{}{"key": key, "rows": rows}
	}
	m["models"] = []interface{}{model}
	return m
}

func seedRows() []interface{} {
	return []interface{}{
		map[string]interface{}{"name": "Efectivo", "code": "cash", "is_active": true, "sort_order": 0},
		map[string]interface{}{"name": "Tarjeta", "code": "card", "is_active": true, "sort_order": 1},
	}
}

// TestValidate_Seed_Valid confirms a model.seed{key,rows} where key names a
// declared column passes the strict v3 schema + cross-field validator.
func TestValidate_Seed_Valid(t *testing.T) {
	if err := Validate(mustJSON(t, modelWithSeed("code", seedRows()))); err != nil {
		t.Fatalf("expected valid seed, got error: %v", err)
	}
}

// TestParse_Seed_RoundTrip confirms the seed block parses onto the typed model.
func TestParse_Seed_RoundTrip(t *testing.T) {
	parsed, err := Parse(mustJSON(t, modelWithSeed("code", seedRows())))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	s := parsed.Models[0].Seed
	if s == nil {
		t.Fatal("expected seed on model, got nil")
	}
	if s.Key != "code" {
		t.Fatalf("expected seed.key=code, got %q", s.Key)
	}
	if len(s.Rows) != 2 || s.Rows[0]["code"] != "cash" {
		t.Fatalf("seed rows not parsed: %+v", s.Rows)
	}
}

// TestValidate_Seed_KeyNotAColumn rejects a seed whose key is not a declared
// column on the model.
func TestValidate_Seed_KeyNotAColumn(t *testing.T) {
	err := Validate(mustJSON(t, modelWithSeed("nope", seedRows())))
	if err == nil || !strings.Contains(err.Error(), "not a declared column") {
		t.Fatalf("expected not-a-column error, got: %v", err)
	}
}

// TestValidate_Seed_EmptyRow rejects a seed with an empty row object.
func TestValidate_Seed_EmptyRow(t *testing.T) {
	err := Validate(mustJSON(t, modelWithSeed("code", []interface{}{map[string]interface{}{}})))
	if err == nil {
		t.Fatalf("expected error for empty seed row, got nil")
	}
}

// TestValidate_Seed_OmittedIsBackwardsCompat confirms a model without a seed
// block still validates (additive, backward-compatible).
func TestValidate_Seed_OmittedIsBackwardsCompat(t *testing.T) {
	if err := Validate(mustJSON(t, modelWithSeed("", nil))); err != nil {
		t.Fatalf("expected valid model without seed, got error: %v", err)
	}
}
