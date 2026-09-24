package manifest_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// seedManifestJSON declares a PaymentMethod model with a declarative seed block
// (key "code" + two default rows). It exercises the additive v3 seeders contract
// end-to-end: parse → validate → FromV3 carries it onto the host ModelDefinition.
const seedManifestJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "pos", "name": "POS", "version": "0.1.0" },
  "compatibility": { "requires": [{ "key": "kernel", "version": ">=3.0.0 <4.0.0" }] },
  "models": [
    {
      "key": "PaymentMethod",
      "table": "payment_methods",
      "columns": [
        { "name": "id", "type": "uuid", "primary_key": true },
        { "name": "organization_id", "type": "uuid", "not_null": true },
        { "name": "name", "type": "text", "not_null": true },
        { "name": "code", "type": "text", "not_null": true },
        { "name": "is_active", "type": "boolean" },
        { "name": "sort_order", "type": "integer" }
      ],
      "seed": {
        "key": "code",
        "rows": [
          { "name": "Efectivo", "code": "cash", "is_active": true, "sort_order": 0 },
          { "name": "Tarjeta", "code": "card", "is_active": true, "sort_order": 1 }
        ]
      }
    }
  ]
}`

func TestV3Parse_AcceptsModelSeed(t *testing.T) {
	m, err := v3.Parse([]byte(seedManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse: %v", err)
	}
	s := m.Models[0].Seed
	if s == nil {
		t.Fatal("expected seed on model, got nil")
	}
	if s.Key != "code" {
		t.Fatalf("expected seed.key=code, got %q", s.Key)
	}
	if len(s.Rows) != 2 || s.Rows[0]["code"] != "cash" || s.Rows[1]["code"] != "card" {
		t.Fatalf("seed rows not parsed: %+v", s.Rows)
	}
}

func TestFromV3_CarriesSeedToHostModelDefinition(t *testing.T) {
	m, err := v3.Parse([]byte(seedManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse: %v", err)
	}
	out := manifest.FromV3(m)
	if len(out.ModelDefinitions) != 1 {
		t.Fatalf("expected 1 model def, got %d", len(out.ModelDefinitions))
	}
	def := out.ModelDefinitions[0]
	if def.Seed == nil {
		t.Fatal("expected def.Seed on host ModelDefinition, got nil")
	}
	if def.Seed.Key != "code" {
		t.Fatalf("expected def.Seed.Key=code, got %q", def.Seed.Key)
	}
	if len(def.Seed.Rows) != 2 || def.Seed.Rows[0]["name"] != "Efectivo" {
		t.Fatalf("seed rows lost in FromV3: %+v", def.Seed.Rows)
	}

	// The host (ops executor) consumes def.Seed as JSON; confirm the round-trip
	// keeps the natural key + rows so the seeder can read them.
	raw, err := json.Marshal(def.Seed)
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	var back manifest.SeedDef
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal seed: %v", err)
	}
	if back.Key != "code" || len(back.Rows) != 2 || back.Rows[1]["code"] != "card" {
		t.Fatalf("host seed JSON round-trip dropped data: %+v", back)
	}
}

// TestFromV3_NoSeedIsNil confirms a model without a seed block maps to a nil
// host ModelDefinition.Seed (additive, backward-compatible).
func TestFromV3_NoSeedIsNil(t *testing.T) {
	const noSeed = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "pos", "name": "POS", "version": "0.1.0" },
  "compatibility": { "requires": [{ "key": "kernel", "version": ">=3.0.0 <4.0.0" }] },
  "models": [
    {
      "key": "PaymentMethod",
      "table": "payment_methods",
      "columns": [ { "name": "id", "type": "uuid", "primary_key": true },
        { "name": "organization_id", "type": "uuid", "not_null": true } ]
    }
  ]
}`
	m, err := v3.Parse([]byte(noSeed))
	if err != nil {
		t.Fatalf("v3.Parse: %v", err)
	}
	out := manifest.FromV3(m)
	if out.ModelDefinitions[0].Seed != nil {
		t.Fatalf("expected nil Seed, got %+v", out.ModelDefinitions[0].Seed)
	}
}

// starterSeedManifestJSON is the "starter record" shape (QA pitsline LIVE-25):
// a default warehouse seeded only while the org has none (when:"empty") and
// linked to the main branch another addon seeded (refs).
const starterSeedManifestJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "inventory", "name": "Inventory", "version": "0.1.0" },
  "compatibility": { "requires": [{ "key": "kernel", "version": ">=3.0.0 <4.0.0" }] },
  "models": [
    {
      "key": "Warehouse",
      "table": "warehouses",
      "columns": [
        { "name": "id", "type": "uuid", "primary_key": true },
        { "name": "organization_id", "type": "uuid", "not_null": true },
        { "name": "code", "type": "text", "not_null": true },
        { "name": "name", "type": "text", "not_null": true },
        { "name": "branch_id", "type": "uuid" }
      ],
      "seed": {
        "key": "code",
        "when": "empty",
        "refs": { "branch_id": { "model": "locations.Branch", "match": { "code": "main" } } },
        "rows": [ { "code": "ALM-01", "name": "Almacén principal" } ]
      }
    }
  ]
}`

func TestFromV3_CarriesSeedWhenAndRefs(t *testing.T) {
	m, err := v3.Parse([]byte(starterSeedManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse: %v", err)
	}
	def := manifest.FromV3(m).ModelDefinitions[0]
	if def.Seed == nil || def.Seed.When != v3.SeedWhenEmpty || !def.Seed.SeedOnlyWhenEmpty() {
		t.Fatalf("seed.when lost in FromV3: %+v", def.Seed)
	}
	ref, ok := def.Seed.Refs["branch_id"]
	if !ok || ref.Model != "locations.Branch" || ref.Match["code"] != "main" {
		t.Fatalf("seed.refs lost in FromV3: %+v", def.Seed.Refs)
	}
	// The host reads def.Seed as JSON.
	raw, _ := json.Marshal(def.Seed)
	var back manifest.SeedDef
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.When != "empty" || back.Refs["branch_id"].Match["code"] != "main" {
		t.Fatalf("host seed JSON round-trip dropped when/refs: %s", raw)
	}
}

func TestV3Parse_RejectsBadSeedWhenAndRefs(t *testing.T) {
	cases := map[string][2]string{
		"unknown when":      {`"when": "empty"`, `"when": "once"`},
		"ref to undeclared": {`"branch_id": {`, `"nope": {`},
		"ref without match": {`"match": { "code": "main" }`, `"match": {}`},
		"ref without model": {`"model": "locations.Branch"`, `"model": ""`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			bad := strings.Replace(starterSeedManifestJSON, c[0], c[1], 1)
			if bad == starterSeedManifestJSON {
				t.Fatalf("replacement %q did not apply", c[0])
			}
			if _, err := v3.Parse([]byte(bad)); err == nil {
				t.Fatalf("v3.Parse accepted %s", name)
			}
		})
	}
}
