package manifest

import "testing"

// TestValidateSeed_WhenAndRefs mirrors the v3 validator on the legacy/install
// path: a starter seed (when:"empty" + refs) is accepted, malformed ones fail.
func TestValidateSeed_WhenAndRefs(t *testing.T) {
	cols := []ColumnDef{{Name: "code"}, {Name: "name"}, {Name: "branch_id"}}
	good := func() *SeedDef {
		return &SeedDef{
			Key:  "code",
			When: "empty",
			Refs: map[string]SeedRefDef{"branch_id": {Model: "locations.Branch", Match: map[string]any{"code": "main"}}},
			Rows: []map[string]any{{"code": "ALM-01", "name": "Almacén principal"}},
		}
	}
	if err := validateSeed(good(), cols); err != nil {
		t.Fatalf("valid starter seed rejected: %v", err)
	}
	bad := map[string]func(s *SeedDef){
		"unknown when":      func(s *SeedDef) { s.When = "once" },
		"ref to undeclared": func(s *SeedDef) { s.Refs = map[string]SeedRefDef{"nope": s.Refs["branch_id"]} },
		"ref without model": func(s *SeedDef) { s.Refs["branch_id"] = SeedRefDef{Match: map[string]any{"code": "main"}} },
		"ref without match": func(s *SeedDef) { s.Refs["branch_id"] = SeedRefDef{Model: "locations.Branch"} },
	}
	for name, mutate := range bad {
		s := good()
		mutate(s)
		if err := validateSeed(s, cols); err == nil {
			t.Errorf("%s: validateSeed accepted it", name)
		}
	}
}
