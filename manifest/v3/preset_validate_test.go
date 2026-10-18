package v3

import (
	"strings"
	"testing"
)

func presetManifest(preset map[string]interface{}) map[string]interface{} {
	m := baseValid()
	m["kind"] = "Preset"
	delete(m, "capabilities")
	delete(m, "tenancy")
	md := m["metadata"].(map[string]interface{})
	md["key"] = "sieteleguas"
	m["preset"] = preset
	return m
}

func TestValidate_PresetOverlayAccepted(t *testing.T) {
	m := presetManifest(map[string]interface{}{
		"extends":       map[string]interface{}{"key": "pitsline", "version": ">=0.9.1 <1.0.0"},
		"addons":        []interface{}{},
		"remove_addons": []interface{}{"vulcanizado"},
		"defaults":      map[string]interface{}{"branding": map[string]interface{}{"platform_name": "Siete Leguas"}},
	})
	md := m["metadata"].(map[string]interface{})
	md["visibility"] = "private"
	md["owner"] = "sieteleguas"
	if err := Validate(mustJSON(t, m)); err != nil {
		t.Fatalf("overlay preset should validate: %v", err)
	}
}

func TestValidate_PresetBlockRules(t *testing.T) {
	cases := []struct {
		name   string
		preset map[string]interface{}
		want   string
	}{
		{"standalone without addons", map[string]interface{}{"addons": []interface{}{}}, "at least one addon"},
		{"remove without extends", map[string]interface{}{
			"addons":        []interface{}{map[string]interface{}{"key": "pos", "version": "^0.42.0"}},
			"remove_addons": []interface{}{"pos"},
		}, "requires preset.extends"},
		{"extends itself", map[string]interface{}{
			"extends": map[string]interface{}{"key": "sieteleguas", "version": "^1.0.0"},
			"addons":  []interface{}{},
		}, "must not be the preset itself"},
		{"bad range", map[string]interface{}{
			"extends": map[string]interface{}{"key": "pitsline", "version": "latest-ish"},
			"addons":  []interface{}{},
		}, "not a semver range"},
		{"add and remove the same addon", map[string]interface{}{
			"extends":       map[string]interface{}{"key": "pitsline", "version": "^0.9.0"},
			"addons":        []interface{}{map[string]interface{}{"key": "caja", "version": "^0.4.0"}},
			"remove_addons": []interface{}{"caja"},
		}, "also lists"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(mustJSON(t, presetManifest(tc.preset)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestValidate_MetadataVisibilityEnum(t *testing.T) {
	m := baseValid()
	m["metadata"].(map[string]interface{})["visibility"] = "secret"
	if err := Validate(mustJSON(t, m)); err == nil {
		t.Fatal("expected error for an unknown visibility")
	}
}
