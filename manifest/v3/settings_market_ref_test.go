package v3

import (
	"strings"
	"testing"
)

func addSetting(m jm, s jm) {
	m["settings"] = append(m["settings"].([]interface{}), s)
}

// Settings with and without the new fields (description, ref, market scope)
// validate and parse; the base example (no new fields) stays valid.
func TestSettingsRefDescriptionMarketScope(t *testing.T) {
	base := loadSetupExample(t)
	if err := Validate(mustJSON(t, base)); err != nil {
		t.Fatalf("base example: %v", err)
	}

	m := loadSetupExample(t)
	addSetting(m, jm{"key": "default_warehouse_id", "type": "dynamic_select", "label": "w", "description": "settings.wh.help", "ref": "Warehouse", "scope": "market"})
	addSetting(m, jm{"key": "series", "type": "dynamic_select", "label": "s", "options_source": "list_series", "scope": "branch"})
	if err := Validate(mustJSON(t, m)); err != nil {
		t.Fatalf("with new fields: %v", err)
	}
	parsed, err := Parse(mustJSON(t, m))
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, s := range parsed.Settings {
		if s.Key == "default_warehouse_id" {
			found = s.Ref == "Warehouse" && s.Scope == "market" && s.Description == "settings.wh.help"
		}
	}
	if !found {
		t.Fatalf("ref/scope/description not decoded: %+v", parsed.Settings)
	}
}

func TestSettingsRefNegative(t *testing.T) {
	cases := []struct {
		name string
		s    jm
		want string
	}{
		{"ref on a string", jm{"key": "some_key", "type": "string", "ref": "Warehouse"}, "ref requires type dynamic_select"},
		{"ref and options_source", jm{"key": "some_key", "type": "dynamic_select", "ref": "Warehouse", "options_source": "f"}, "mutually exclusive"},
		{"unknown scope", jm{"key": "some_key", "type": "string", "scope": "galaxy"}, "scope"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := loadSetupExample(t)
			addSetting(m, c.s)
			err := Validate(mustJSON(t, m))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want error containing %q, got %+v", c.want, err)
			}
		})
	}
}
