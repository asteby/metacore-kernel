package query

import "testing"

func TestParseFromMap_SkipCountAndFields(t *testing.T) {
	p, err := ParseFromMap(map[string][]string{
		"count":  {"0"},
		"fields": {"quantity, reserved, id;drop"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !p.SkipCount {
		t.Fatal("count=0 must set SkipCount")
	}
	if len(p.Fields) != 2 || p.Fields[0] != "quantity" || p.Fields[1] != "reserved" {
		t.Fatalf("fields = %#v, want quantity, reserved (unsafe token dropped)", p.Fields)
	}

	on, err := ParseFromMap(map[string][]string{"count": {"1"}})
	if err != nil {
		t.Fatal(err)
	}
	if on.SkipCount {
		t.Fatal("count=1 must keep the COUNT")
	}
}

func TestProjectMaps_KeepsID(t *testing.T) {
	rows := []map[string]any{
		{"id": "a", "quantity": 3, "name": "tire"},
	}
	got := ProjectMaps(rows, []string{"quantity"})
	if _, ok := got[0]["name"]; ok {
		t.Fatalf("name must be stripped: %#v", got[0])
	}
	if got[0]["id"] != "a" || got[0]["quantity"] != 3 {
		t.Fatalf("projection = %#v", got[0])
	}
	same := ProjectMaps(rows, nil)
	if same[0]["name"] != "tire" {
		t.Fatal("empty fields must keep every column")
	}
}
