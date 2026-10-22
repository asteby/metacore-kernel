package manifest_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/modelbase"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// recordPresentationManifest builds a v3 addon exercising the mobile-UX
// vocabulary: action priority/destructive and the column/model record roles.
func recordPresentationManifest(models, actions string) []byte {
	return []byte(`{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "wh", "name": "Warehouse", "version": "0.1.0" },
  "compatibility": { "requires": [{ "key": "kernel", "version": ">=3.0.0 <4.0.0" }] },
  "models": [` + models + `],
  "contributions": { "actions": [` + actions + `] }
}`)
}

const rpModel = `{
  "key": "Order", "table": "orders",
  "title_field": "number", "subtitle_field": "customer_name",
  "columns": [
    { "name": "id", "type": "uuid", "primary_key": true },
    { "name": "organization_id", "type": "uuid", "not_null": true },
    { "name": "number", "type": "text", "card_role": "title" },
    { "name": "customer_id", "type": "uuid", "ref": "customers" },
    { "name": "customer_name", "type": "text", "display_for": "customer_id", "card_role": "subtitle" },
    { "name": "total", "type": "numeric", "card_role": "metric" },
    { "name": "internal", "type": "text", "card_role": "hidden" }
  ]
}`

const rpActions = `
  { "key": "dispatch", "label": "Despachar", "target_model": "Order",
    "handler": { "type": "wasm", "function": "dispatch" }, "priority": "primary" },
  { "key": "refresh", "label": "Actualizar", "target_model": "Order",
    "handler": { "type": "wasm", "function": "refresh" }, "priority": "overflow" },
  { "key": "release", "label": "Liberar apartado", "target_model": "Order",
    "handler": { "type": "wasm", "function": "release" }, "destructive": true },
  { "key": "plain", "label": "Plain", "target_model": "Order",
    "handler": { "type": "wasm", "function": "plain" } }`

func TestFromV3_RecordPresentation_ProjectsAndRoundTrips(t *testing.T) {
	m, err := v3.Parse(recordPresentationManifest(rpModel, rpActions))
	if err != nil {
		t.Fatalf("v3.Parse: %v", err)
	}
	host := manifest.FromV3(m)

	acts := map[string]manifest.ActionDef{}
	for _, a := range host.Actions["Order"] {
		acts[a.Key] = a
	}
	if acts["dispatch"].Priority != "primary" || acts["dispatch"].Destructive {
		t.Fatalf("dispatch: %+v", acts["dispatch"])
	}
	if acts["refresh"].Priority != "overflow" {
		t.Fatalf("refresh priority = %q", acts["refresh"].Priority)
	}
	if !acts["release"].Destructive || acts["release"].Priority != "" {
		t.Fatalf("release: %+v", acts["release"])
	}

	if len(host.ModelDefinitions) != 1 {
		t.Fatalf("model definitions: %d", len(host.ModelDefinitions))
	}
	md := host.ModelDefinitions[0]
	if md.TitleField != "number" || md.SubtitleField != "customer_name" {
		t.Fatalf("title/subtitle = %q/%q", md.TitleField, md.SubtitleField)
	}

	// Derived served columns carry display_for / card_role.
	cols := map[string]modelbase.ColumnDef{}
	for _, c := range dynamic.DeriveTableColumns(md) {
		cols[c.Key] = c
	}
	if cols["customer_name"].DisplayFor != "customer_id" || cols["customer_name"].CardRole != "subtitle" {
		t.Fatalf("customer_name: %+v", cols["customer_name"])
	}
	if cols["total"].CardRole != "metric" || cols["internal"].CardRole != "hidden" || cols["number"].DisplayFor != "" {
		t.Fatalf("roles: total=%q internal=%q", cols["total"].CardRole, cols["internal"].CardRole)
	}

	// host (manifest) ActionDef -> JSON -> served modelbase.ActionDef keeps the
	// fields (this is the host→SDK round-trip), under the exact JSON keys.
	for _, key := range []string{"dispatch", "refresh", "release"} {
		raw, err := json.Marshal(acts[key])
		if err != nil {
			t.Fatal(err)
		}
		var served modelbase.ActionDef
		if err := json.Unmarshal(raw, &served); err != nil {
			t.Fatal(err)
		}
		if served.Priority != acts[key].Priority || served.Destructive != acts[key].Destructive {
			t.Fatalf("%s lost on round-trip: %+v vs %+v", key, served, acts[key])
		}
		back, _ := json.Marshal(served)
		if !strings.Contains(string(back), `"destructive":true`) != !acts[key].Destructive {
			t.Fatalf("%s: destructive key mismatch in %s", key, back)
		}
	}
	rawCol, _ := json.Marshal(cols["customer_name"])
	for _, want := range []string{`"display_for":"customer_id"`, `"card_role":"subtitle"`} {
		if !strings.Contains(string(rawCol), want) {
			t.Fatalf("served column JSON lacks %s: %s", want, rawCol)
		}
	}
	var backCol modelbase.ColumnDef
	if err := json.Unmarshal(rawCol, &backCol); err != nil || backCol.DisplayFor != "customer_id" || backCol.CardRole != "subtitle" {
		t.Fatalf("column round-trip: %+v err=%v", backCol, err)
	}
	rawMeta, _ := json.Marshal(modelbase.TableMetadata{TitleField: md.TitleField, SubtitleField: md.SubtitleField})
	if !strings.Contains(string(rawMeta), `"title_field":"number"`) || !strings.Contains(string(rawMeta), `"subtitle_field":"customer_name"`) {
		t.Fatalf("table metadata JSON: %s", rawMeta)
	}

	// The projected host manifest passes the legacy validation plane too.
	if err := host.Validate("99.0.0"); err != nil && strings.Contains(err.Error(), "display_for") {
		t.Fatalf("legacy validation rejected the vocabulary: %v", err)
	}
}

// Backward compatibility: a manifest without any of the new fields projects
// and serves exactly as before — the keys never appear in the payload.
func TestFromV3_RecordPresentation_OmittedWhenUnset(t *testing.T) {
	plain := `{ "key": "Order", "table": "orders", "columns": [
    { "name": "id", "type": "uuid", "primary_key": true },
    { "name": "organization_id", "type": "uuid", "not_null": true },
    { "name": "number", "type": "text" } ] }`
	act := `{ "key": "plain", "label": "Plain", "target_model": "Order", "handler": { "type": "wasm", "function": "plain" } }`
	m, err := v3.Parse(recordPresentationManifest(plain, act))
	if err != nil {
		t.Fatal(err)
	}
	host := manifest.FromV3(m)
	a := host.Actions["Order"][0]
	if a.Priority != "" || a.Destructive {
		t.Fatalf("defaults changed: %+v", a)
	}
	rawA, _ := json.Marshal(a)
	for _, k := range []string{"destructive", "priority"} {
		if strings.Contains(string(rawA), `"`+k+`"`) {
			t.Fatalf("%s leaked into %s", k, rawA)
		}
	}
	md := host.ModelDefinitions[0]
	var served []byte
	for _, c := range dynamic.DeriveTableColumns(md) {
		b, _ := json.Marshal(c)
		served = append(served, b...)
	}
	for _, k := range []string{"display_for", "card_role"} {
		if strings.Contains(string(served), k) {
			t.Fatalf("%s leaked into served columns", k)
		}
	}
	rawMeta, _ := json.Marshal(modelbase.TableMetadata{Title: "x"})
	if strings.Contains(string(rawMeta), "title_field") || strings.Contains(string(rawMeta), "subtitle_field") {
		t.Fatalf("table metadata leaked: %s", rawMeta)
	}
}

func TestValidate_RecordPresentationRejects(t *testing.T) {
	cases := []struct {
		name, model, want string
	}{
		{"display_for unknown column", `{"key":"Order","table":"orders","columns":[
			{"name":"id","type":"uuid","primary_key":true},{"name":"organization_id","type":"uuid","not_null":true},
			{"name":"customer_name","type":"text","display_for":"nope"}]}`, "display_for"},
		{"display_for target without ref", `{"key":"Order","table":"orders","columns":[
			{"name":"id","type":"uuid","primary_key":true},{"name":"organization_id","type":"uuid","not_null":true},
			{"name":"customer_id","type":"uuid"},{"name":"customer_name","type":"text","display_for":"customer_id"}]}`, "ref"},
		{"display_for itself", `{"key":"Order","table":"orders","columns":[
			{"name":"id","type":"uuid","primary_key":true},{"name":"organization_id","type":"uuid","not_null":true},
			{"name":"customer_id","type":"uuid","ref":"customers","display_for":"customer_id"}]}`, "itself"},
		{"card_role bad", `{"key":"Order","table":"orders","columns":[
			{"name":"id","type":"uuid","primary_key":true},{"name":"organization_id","type":"uuid","not_null":true},
			{"name":"n","type":"text","card_role":"big"}]}`, "card_role"},
		{"title_field unknown", `{"key":"Order","table":"orders","title_field":"zzz","columns":[
			{"name":"id","type":"uuid","primary_key":true},{"name":"organization_id","type":"uuid","not_null":true}]}`, "title_field"},
		{"subtitle_field unknown", `{"key":"Order","table":"orders","subtitle_field":"zzz","columns":[
			{"name":"id","type":"uuid","primary_key":true},{"name":"organization_id","type":"uuid","not_null":true}]}`, "subtitle_field"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := v3.Parse(recordPresentationManifest(tc.model, ""))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestValidate_ActionPriorityOverflowAndDestructive(t *testing.T) {
	model := `{"key":"Order","table":"orders","columns":[
		{"name":"id","type":"uuid","primary_key":true},{"name":"organization_id","type":"uuid","not_null":true}]}`
	bad := `{ "key": "x", "label": "X", "target_model": "Order", "handler": {"type":"wasm","function":"x"}, "priority": "footer" }`
	if _, err := v3.Parse(recordPresentationManifest(model, bad)); err == nil || !strings.Contains(err.Error(), "priority") {
		t.Fatalf("priority=footer must fail, got %v", err)
	}
	badD := `{ "key": "x", "label": "X", "target_model": "Order", "handler": {"type":"wasm","function":"x"}, "destructive": "yes" }`
	if _, err := v3.Parse(recordPresentationManifest(model, badD)); err == nil {
		t.Fatal("destructive must be a boolean")
	}
}

func TestLegacyValidate_RecordPresentation(t *testing.T) {
	host := manifest.Manifest{
		Key: "wh", Name: "W", Version: "0.1.0",
		ModelDefinitions: []manifest.ModelDefinition{{
			TableName: "orders", ModelKey: "Order", TitleField: "ghost",
			Columns: []manifest.ColumnDef{{Name: "id", Type: "uuid"}},
		}},
	}
	if err := host.Validate("99.0.0"); err == nil || !strings.Contains(err.Error(), "title_field") {
		t.Fatalf("legacy plane must reject unknown title_field, got %v", err)
	}
	host.ModelDefinitions[0].TitleField = ""
	host.ModelDefinitions[0].Columns[0].CardRole = "huge"
	if err := host.Validate("99.0.0"); err == nil || !strings.Contains(err.Error(), "card_role") {
		t.Fatalf("legacy plane must reject card_role, got %v", err)
	}
	host.ModelDefinitions[0].Columns[0].CardRole = ""
	host.Actions = map[string][]manifest.ActionDef{"Order": {{Key: "a", Name: "a", Label: "A", Priority: "footer"}}}
	if err := host.Validate("99.0.0"); err == nil || !strings.Contains(err.Error(), "priority") {
		t.Fatalf("legacy plane must reject priority, got %v", err)
	}
}
