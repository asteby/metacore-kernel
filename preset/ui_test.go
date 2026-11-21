package preset

import (
	"testing"

	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// Loom (RFC 0005): a preset's ui.contributions[] travel in Resolved.UI; an
// overlay replaces a base graft by id and appends its own.
func TestResolvedUI_BaseAndOverlay(t *testing.T) {
	base, err := ResolveFromManifest(&v3.Manifest{
		Kind:     v3.KindPreset,
		Metadata: v3.Metadata{Key: "pitsline", Name: "Pitsline", Version: "0.9.3"},
		Preset:   &v3.Preset{Addons: []v3.PresetAddon{{Key: "fiscal_mexico", Version: ">=0.43.0 <1.0.0"}}},
		UI: &v3.UIBlock{Contributions: []v3.UIGraft{
			{ID: "pitsline.csf", Part: "fiscal_mexico.csf-import", Seam: "record.dialog.header", Order: 10},
			{ID: "pitsline.hint", Part: "fiscal_mexico.hint", Seam: "record.form.section"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(base.UI) != 2 {
		t.Fatalf("base ui = %#v", base.UI)
	}
	overlay := overlayManifest(t, nil, nil, `{}`)
	overlay.UI = &v3.UIBlock{Contributions: []v3.UIGraft{
		{ID: "pitsline.csf", Part: "fiscal_mexico.csf-import", Seam: "record.dialog.header", Order: 1},
		{ID: "sieteleguas.vehicle", Part: "vehicles.picker", Seam: "field.widget"},
	}}
	out, err := Flatten(base, overlay)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"pitsline.csf", "pitsline.hint", "sieteleguas.vehicle"}
	if len(out.UI) != len(want) {
		t.Fatalf("flattened ui = %#v", out.UI)
	}
	for i, id := range want {
		if out.UI[i].ID != id {
			t.Fatalf("ui[%d] = %s, want %s", i, out.UI[i].ID, id)
		}
	}
	if out.UI[0].Order != 1 {
		t.Fatal("overlay must replace the base graft with the same id")
	}
	if base.UI[0].Order != 10 {
		t.Fatal("flatten must not mutate the base")
	}
}

func TestResolvedUI_AbsentIsNil(t *testing.T) {
	r, err := ResolveFromManifest(&v3.Manifest{
		Kind:     v3.KindPreset,
		Metadata: v3.Metadata{Key: "p", Version: "1.0.0"},
		Preset:   &v3.Preset{},
	})
	if err != nil || r.UI != nil {
		t.Fatalf("ui = %#v err = %v", r.UI, err)
	}
}
