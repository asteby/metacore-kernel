package dyntest

import (
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

func managedColsManifest(createdAt, updatedAt string) string {
	return `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "store", "name": "Store", "version": "0.1.0" },
  "compatibility": { "requires": [ { "key": "kernel", "version": ">=0.1.0" } ] },
  "models": [
    {
      "key": "Product",
      "table": "addon_store_products",
      "label": "Products",
      "columns": [
        { "name": "id", "type": "uuid", "primary_key": true },
        { "name": "organization_id", "type": "uuid", "not_null": true },
        { "name": "name", "type": "text" },
        { "name": "created_at", "type": "timestamptz"` + createdAt + ` },
        { "name": "updated_at", "type": "timestamptz"` + updatedAt + ` }
      ]
    }
  ]
}`
}

func tableKeys(t *testing.T, raw string) (keys []string, def manifest.ModelDefinition) {
	t.Helper()
	m, err := v3.Parse([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	host := manifest.FromV3(m)
	def = host.ModelDefinitions[0]
	for _, c := range dynamic.DeriveTableColumns(def) {
		keys = append(keys, c.Key)
	}
	return keys, def
}

// Declaring a kernel-managed column documents the physical shape and keeps
// today's behaviour: it is not a table column. show_in_table opts it in.
func TestShowInTable_ManagedColumns(t *testing.T) {
	keys, def := tableKeys(t, managedColsManifest(``, ``))
	if strings.Join(keys, ",") != "name" {
		t.Fatalf("default behaviour changed: table columns = %v", keys)
	}
	if len(def.DisplayColumns) != 0 {
		t.Fatalf("no opt-in, no display columns: %+v", def.DisplayColumns)
	}

	keys, def = tableKeys(t, managedColsManifest(`, "show_in_table": true, "label": "Creado", "display": "date"`, ``))
	if strings.Join(keys, ",") != "name,created_at" {
		t.Fatalf("opted-in column must follow the declared ones: %v", keys)
	}
	cols := dynamic.DeriveTableColumns(def)
	created := cols[len(cols)-1]
	if created.Label != "Creado" || created.CellStyle != "date" || !created.Sortable {
		t.Fatalf("created_at table column = %+v", created)
	}
	// DDL and forms never see it: it is not part of Columns.
	for _, c := range def.Columns {
		if c.Name == "created_at" {
			t.Fatal("a managed column must not be re-declared in Columns (CreateTable would duplicate it)")
		}
	}
	for _, f := range dynamic.DeriveFormFields(def) {
		if f.Key == "created_at" {
			t.Fatal("a managed column never reaches the form")
		}
	}
}

func TestShowInTable_Validation(t *testing.T) {
	bad := `{"apiVersion":"asteby.com/v3","kind":"Addon","metadata":{"key":"store","name":"S","version":"0.1.0"},
"compatibility":{"requires":[{"key":"kernel","version":">=0.1.0"}]},
"models":[{"key":"Prod","table":"addon_store_p","columns":[{"name":"id","type":"uuid","primary_key":true},
{"name":"organization_id","type":"uuid","not_null":true},{"name":"name","type":"text","show_in_table":true}]}]}`
	if _, err := v3.Parse([]byte(bad)); err == nil || !strings.Contains(err.Error(), "show_in_table") {
		t.Fatalf("show_in_table on an ordinary column must be rejected, got %v", err)
	}
	if _, err := v3.Parse([]byte(managedColsManifest(`, "show_in_table": true, "visibility": "modal"`, ``))); err == nil || !strings.Contains(err.Error(), "visibility") {
		t.Fatalf("show_in_table with visibility modal must be rejected, got %v", err)
	}
}
