package installer

import (
	"reflect"
	"testing"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/manifest"
)

// The host's MigrationSchema reaches dynamic.ApplyWithOptions together with
// the addon's model tables; without it the migration options stay zero, i.e.
// the historical `addon_<key>, public` search_path.
func TestMigrationOptions(t *testing.T) {
	m := manifest.Manifest{Key: "inventory", ModelDefinitions: []manifest.ModelDefinition{
		{ModelKey: "stock", TableName: "stock"},
		{ModelKey: "warehouses", TableName: "warehouses"},
	}}
	if got := migrationOptions(nil, m); !reflect.DeepEqual(got, dynamic.ApplyOptions{}) {
		t.Fatalf("no host schema: got %+v, want zero options", got)
	}
	var asked string
	inst := (&Installer{}).WithMigrationSchema(func(key string) string { asked = key; return "public" })
	got := migrationOptions(inst.MigrationSchema, m)
	want := dynamic.ApplyOptions{PrimarySchema: "public", ModelTables: []string{"stock", "warehouses"}}
	if !reflect.DeepEqual(got, want) || asked != "inventory" {
		t.Fatalf("got %+v (asked %q), want %+v", got, asked, want)
	}
	if a, ok := inst.upgradeApplier().(defaultSchemaApplier); !ok || a.migrationSchema == nil {
		t.Fatalf("Upgrade's default applier must carry the host MigrationSchema")
	}
}
