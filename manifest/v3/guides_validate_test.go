package v3

import (
	"strings"
	"testing"
)

func withGuides(guides []interface{}) map[string]interface{} {
	m := baseValid()
	m["contributions"] = map[string]interface{}{"guides": guides}
	return m
}

func guideStep(id string) map[string]interface{} {
	return map[string]interface{}{"id": id, "target": "sales.new", "title": "t", "description": "d"}
}

func TestGuides_Valid(t *testing.T) {
	m := withGuides([]interface{}{
		map[string]interface{}{
			"key": "cashier_first_sale", "title": "help.cashier.first_sale",
			"roles": []interface{}{"cashier"}, "trigger": "first_visit", "route": "/pos",
			"steps": []interface{}{
				guideStep("open"),
				map[string]interface{}{"id": "intro", "kind": "concept", "title": "t", "description": "d"},
			},
		},
		map[string]interface{}{
			"key": "manual_one", "title": "help.manual",
			"steps": []interface{}{guideStep("a")},
		},
	})
	if err := Validate(mustJSON(t, m)); err != nil {
		t.Fatalf("expected valid, got: %v", err)
	}
}

func TestGuides_Rejects(t *testing.T) {
	cases := map[string]struct {
		guides []interface{}
		want   string
	}{
		"dup key": {[]interface{}{
			map[string]interface{}{"key": "g", "title": "t", "steps": []interface{}{guideStep("a")}},
			map[string]interface{}{"key": "g", "title": "t", "steps": []interface{}{guideStep("a")}},
		}, "duplicated"},
		"first_visit sin route": {[]interface{}{
			map[string]interface{}{"key": "g", "title": "t", "trigger": "first_visit", "steps": []interface{}{guideStep("a")}},
		}, "route is required"},
		"trigger invalido": {[]interface{}{
			map[string]interface{}{"key": "g", "title": "t", "trigger": "always", "steps": []interface{}{guideStep("a")}},
		}, "trigger"},
		"step dup": {[]interface{}{
			map[string]interface{}{"key": "g", "title": "t", "steps": []interface{}{guideStep("a"), guideStep("a")}},
		}, "duplicated"},
		"step ui sin target": {[]interface{}{
			map[string]interface{}{"key": "g", "title": "t", "steps": []interface{}{
				map[string]interface{}{"id": "a", "title": "t", "description": "d"},
			}},
		}, "target is required"},
		"key no snake_case": {[]interface{}{
			map[string]interface{}{"key": "Bad-Key", "title": "t", "steps": []interface{}{guideStep("a")}},
		}, "key"},
		"route sin slash": {[]interface{}{
			map[string]interface{}{"key": "g", "title": "t", "trigger": "first_visit", "route": "pos", "steps": []interface{}{guideStep("a")}},
		}, "route"},
		"sin pasos": {[]interface{}{
			map[string]interface{}{"key": "g", "title": "t", "steps": []interface{}{}},
		}, "steps"},
	}
	for name, c := range cases {
		err := Validate(mustJSON(t, withGuides(c.guides)))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want error containing %q, got %v", name, c.want, err)
		}
	}
}

// A preset has no contributions of its own: it declares guides inline in
// preset.defaults (free-form), which the host materializes per org.
func TestGuides_PresetDefaults(t *testing.T) {
	m := baseValid()
	m["kind"] = "Preset"
	delete(m, "capabilities")
	m["preset"] = map[string]interface{}{
		"addons": []interface{}{map[string]interface{}{"key": "pos", "version": ">=0.1.0"}},
		"defaults": map[string]interface{}{
			"guides": []interface{}{
				map[string]interface{}{
					"key": "cajero_caja", "title": "t", "roles": []interface{}{"cajero"},
					"trigger": "first_visit", "route": "/addons/pos/terminal",
					"steps": []interface{}{guideStep("a")},
				},
			},
		},
	}
	if err := Validate(mustJSON(t, m)); err != nil {
		t.Fatalf("preset with defaults.guides must validate, got: %v", err)
	}
}
