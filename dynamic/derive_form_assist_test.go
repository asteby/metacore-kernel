package dynamic

import (
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
)

// TestDeriveFormLayout_CarriesAssist guards the served projection of an
// AI-assisted step: dropping it here is what would make the SDK render a
// plain step (the v3 → manifest side is covered in manifest's tests).
func TestDeriveFormLayout_CarriesAssist(t *testing.T) {
	def := manifest.ModelDefinition{FormLayout: &manifest.FormLayoutDef{Mode: "steps", Sections: []manifest.FormSectionDef{{
		Key: "site", Title: "Sitio",
		Assist: &manifest.FormAssistDef{Provider: "brand.website_dna", Label: "Analizar", Input: []string{"website", "name"}, Output: []string{"tagline", "primary_color"}, Trigger: "button"},
	}, {Key: "voice"}}}}
	served := DeriveFormLayout(def)
	if served == nil || len(served.Sections) != 2 {
		t.Fatalf("layout = %+v", served)
	}
	a := served.Sections[0].Assist
	if a == nil || a.Provider != "brand.website_dna" || a.Label != "Analizar" || len(a.Input) != 2 || a.Output[1] != "primary_color" || a.Trigger != "button" {
		t.Fatalf("assist dropped/mangled: %+v", a)
	}
	if served.Sections[1].Assist != nil {
		t.Error("plain section must stay nil")
	}
}
