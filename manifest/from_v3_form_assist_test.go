package manifest

import (
	"testing"

	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

func TestMapFormLayout_CarriesAssist(t *testing.T) {
	fl := mapFormLayout(&v3.FormLayout{Mode: "steps", Sections: []v3.FormSection{{
		Key: "site",
		Assist: &v3.FormAssist{Provider: "brand.website_dna", Input: []string{"website"}, Output: []string{"tagline"}, Trigger: "auto"},
	}}})
	if fl == nil || fl.Sections[0].Assist == nil || fl.Sections[0].Assist.Provider != "brand.website_dna" || fl.Sections[0].Assist.Trigger != "auto" {
		t.Fatalf("assist dropped in mapFormLayout: %+v", fl)
	}
}
