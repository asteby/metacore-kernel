package modelbase_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/modelbase"
)

func TestTableMetadataJSONShape(t *testing.T) {
	tm := modelbase.TableMetadata{
		Title: "models.users.table.title",
		Columns: []modelbase.ColumnDef{
			{Key: "name", Label: "Name", Type: "text", Sortable: true, Filterable: true},
			{Key: "role", Label: "Role", Type: "select", UseOptions: true, CellStyle: "badge"},
		},
		SearchColumns:     []string{"name", "email"},
		EnableCRUDActions: true,
		SearchPlaceholder: "Search…",
	}

	raw, err := json.Marshal(tm)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(raw)

	// Frontend-contract keys must use camelCase.
	for _, want := range []string{
		`"title":"models.users.table.title"`,
		`"columns":[`,
		`"searchColumns":["name","email"]`,
		`"enableCRUDActions":true`,
		`"searchPlaceholder":"Search…"`,
		`"key":"name"`,
		`"sortable":true`,
		`"filterable":true`,
		`"useOptions":true`,
		`"cellStyle":"badge"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("marshalled TableMetadata missing %q\nfull: %s", want, s)
		}
	}

	// Omitempty must drop unset fields so the payload stays small.
	for _, forbid := range []string{
		`"filters":`,
		`"actions":`,
		`"perPageOptions":`,
		`"defaultPerPage":`,
	} {
		if strings.Contains(s, forbid) {
			t.Errorf("marshalled TableMetadata should omit %q\nfull: %s", forbid, s)
		}
	}
}

func TestModalMetadataJSONShape(t *testing.T) {
	mm := modelbase.ModalMetadata{
		Title:       "Edit",
		CreateTitle: "Create",
		Fields: []modelbase.FieldDef{
			{Key: "email", Label: "Email", Type: "email", Required: true},
			{Key: "password", Label: "Password", Type: "password", Required: true, HideInView: true},
		},
	}

	raw, err := json.Marshal(mm)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(raw)

	for _, want := range []string{
		`"title":"Edit"`,
		`"createTitle":"Create"`,
		`"fields":[`,
		`"required":true`,
		`"hideInView":true`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("marshalled ModalMetadata missing %q\nfull: %s", want, s)
		}
	}
}

// Sanity-check: KV is an alias of OptionDef (same JSON shape).
func TestKVAliasesOptionDef(t *testing.T) {
	kv := modelbase.KV{Value: "a", Label: "A"}
	od := modelbase.OptionDef(kv)
	b1, _ := json.Marshal(kv)
	b2, _ := json.Marshal(od)
	if string(b1) != string(b2) {
		t.Fatalf("KV and OptionDef JSON differ: %s vs %s", b1, b2)
	}
}

func TestSearchConfigSQLFieldsNotSerialized(t *testing.T) {
	cfg := modelbase.SearchConfig{
		SearchIn:  []string{"name"},
		BaseWhere: "x.id IN (SELECT id FROM y)",
		BaseArgs:  []any{1},
		Joins:     []string{"JOIN users ON users.id = x.user_id"},
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"baseWhere", "baseArgs", "joins", "BaseWhere", "BaseArgs", "Joins", "JOIN users"} {
		if strings.Contains(string(b), k) {
			t.Fatalf("%q leaked into JSON: %s", k, b)
		}
	}

	var got modelbase.SearchConfig
	in := `{"searchIn":["name"],"baseWhere":"1=1","baseArgs":[1],"joins":["JOIN z"],"BaseWhere":"1=1","Joins":["JOIN z"]}`
	if err := json.Unmarshal([]byte(in), &got); err != nil {
		t.Fatal(err)
	}
	if got.BaseWhere != "" || got.BaseArgs != nil || got.Joins != nil {
		t.Fatalf("client JSON populated SQL fields: %+v", got)
	}
	if len(got.SearchIn) != 1 {
		t.Fatalf("regular fields should still decode: %+v", got)
	}
}
