package preset

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

func pitslineBase(t *testing.T) Resolved {
	t.Helper()
	r, err := ResolveFromManifest(&v3.Manifest{
		Kind:     v3.KindPreset,
		Metadata: v3.Metadata{Key: "pitsline", Name: "Pitsline", Version: "0.9.3"},
		Preset: &v3.Preset{
			Addons: []v3.PresetAddon{
				{Key: "products", Version: ">=0.20.0 <1.0.0"},
				{Key: "fiscal_mexico", Version: ">=0.43.0 <1.0.0"},
				{Key: "pos", Version: ">=0.42.0 <1.0.0"},
				{Key: "vulcanizado", Version: ">=0.5.0 <1.0.0"},
				{Key: "caja", Version: ">=0.4.0 <1.0.0", Optional: true},
				{Key: "kiosk", Version: ">=0.2.0 <1.0.0", Optional: true},
			},
			Defaults: decode(t, `{
				"branding": {"platform_name": "Pitsline", "primary_color": "#EB2221", "support_email": "hola@pitsline.mx"},
				"ui_theme": {"theme_key": "classic", "font": "manrope"},
				"onboarding": {"context": "llantera", "chips": [{"label": "a"}]},
				"roles": [
					{"slug": "cajero", "name": "Cajero", "permissions": ["pos.session.open", "sales.salesorder.delete"]},
					{"slug": "fleet_manager", "name": "Flotillas"}
				],
				"records": {"kiosk.KioskProfile": {"key": "code", "rows": [{"code": "retail", "name": "Retail"}]}}
			}`),
		},
	})
	if err != nil {
		t.Fatalf("resolve base: %v", err)
	}
	return r
}

func overlayManifest(t *testing.T, addons []v3.PresetAddon, remove []string, defaults string) *v3.Manifest {
	t.Helper()
	return &v3.Manifest{
		Kind:     v3.KindPreset,
		Metadata: v3.Metadata{Key: "sieteleguas", Name: "Siete Leguas", Version: "1.0.0", Visibility: v3.VisibilityPrivate},
		Preset: &v3.Preset{
			Extends:      &v3.PresetExtends{Key: "pitsline", Version: ">=0.9.1 <1.0.0"},
			Addons:       addons,
			RemoveAddons: remove,
			Defaults:     decode(t, defaults),
		},
	}
}

func decode(t *testing.T, s string) map[string]interface{} {
	t.Helper()
	if s == "" {
		return nil
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return m
}

func keys(addons []Addon) []string {
	out := make([]string, 0, len(addons))
	for _, a := range addons {
		out = append(out, a.Key)
	}
	return out
}

func TestResolveFromManifestRejectsOverlay(t *testing.T) {
	_, err := ResolveFromManifest(overlayManifest(t, nil, nil, ""))
	if !errors.Is(err, ErrNeedsBase) {
		t.Fatalf("want ErrNeedsBase, got %v", err)
	}
}

func TestFlattenAddons(t *testing.T) {
	base := pitslineBase(t)
	ov := overlayManifest(t, []v3.PresetAddon{
		{Key: "caja", Version: ">=0.5.0"}, // optional → required, range tightened
		{Key: "fiscal_savings", Version: ">=0.1.0 <1.0.0", Optional: true, Requires: []string{"fiscal_mexico"}}, // new private addon
	}, []string{"vulcanizado"}, "")

	got, err := Flatten(base, ov)
	if err != nil {
		t.Fatalf("flatten: %v", err)
	}
	if want := []string{"products", "fiscal_mexico", "pos", "caja", "kiosk", "fiscal_savings"}; !reflect.DeepEqual(keys(got.Addons), want) {
		t.Fatalf("addons = %v, want %v", keys(got.Addons), want)
	}
	for _, a := range got.Addons {
		if a.Key == "caja" {
			if a.Optional {
				t.Fatalf("caja should be required after the overlay")
			}
			if a.Version != ">=0.4.0 <1.0.0, >=0.5.0" {
				t.Fatalf("caja version = %q", a.Version)
			}
		}
	}
	if got.Key != "sieteleguas" || got.Version != "1.0.0" {
		t.Fatalf("identity = %s@%s", got.Key, got.Version)
	}
	wantLineage := []LineageEntry{{Key: "pitsline", Version: "0.9.3"}, {Key: "sieteleguas", Version: "1.0.0"}}
	if !reflect.DeepEqual(got.Lineage, wantLineage) {
		t.Fatalf("lineage = %v", got.Lineage)
	}
	// The base must not be mutated.
	if len(base.Addons) != 6 || !base.Addons[len(base.Addons)-2].Optional {
		t.Fatalf("base mutated: %+v", base.Addons)
	}
}

func TestFlattenErrors(t *testing.T) {
	base := pitslineBase(t)
	cases := []struct {
		name string
		mut  func(m *v3.Manifest)
		base func(r Resolved) Resolved
		want error
	}{
		{"base out of range", func(m *v3.Manifest) { m.Preset.Extends.Version = ">=1.0.0" }, nil, ErrBaseMismatch},
		{"wrong base key", func(m *v3.Manifest) { m.Preset.Extends.Key = "retail" }, nil, ErrBaseMismatch},
		{"remove unknown", func(m *v3.Manifest) { m.Preset.RemoveAddons = []string{"nope"} }, nil, ErrUnknownRemoval},
		{"remove a dependency", func(m *v3.Manifest) {
			m.Preset.Addons = []v3.PresetAddon{{Key: "fiscal_savings", Version: "^0.1.0", Requires: []string{"fiscal_mexico"}}}
			m.Preset.RemoveAddons = []string{"fiscal_mexico"}
		}, nil, ErrUnknownDependency},
		{"or range", func(m *v3.Manifest) {
			m.Preset.Addons = []v3.PresetAddon{{Key: "pos", Version: "^0.42.0 || ^0.50.0"}}
		}, nil, ErrIncompatibleRange},
		{"cycle", nil, func(r Resolved) Resolved {
			r.Lineage = append([]LineageEntry{{Key: "sieteleguas", Version: "0.1.0"}}, r.Lineage...)
			return r
		}, ErrCyclicExtends},
		{"too deep", nil, func(r Resolved) Resolved {
			r.Lineage = []LineageEntry{{Key: "a", Version: "1.0.0"}, {Key: "b", Version: "1.0.0"}, {Key: "pitsline", Version: "0.9.3"}}
			return r
		}, ErrLineageTooDeep},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ov := overlayManifest(t, nil, nil, "")
			if tc.mut != nil {
				tc.mut(ov)
			}
			b := base
			if tc.base != nil {
				b = tc.base(base)
			}
			if _, err := Flatten(b, ov); !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
		})
	}
}

func TestMergeDefaults(t *testing.T) {
	base := pitslineBase(t)
	ov := overlayManifest(t, nil, nil, `{
		"branding": {"platform_name": "Siete Leguas", "primary_color": "#C8102E", "support_email": null},
		"onboarding": {"context": "siete leguas"},
		"roles": [
			{"slug": "cajero", "icon": "Wallet", "permissions": ["caja.cashcut.create", "-sales.salesorder.delete"]},
			{"slug": "vendedor", "name": "Vendedor"},
			{"slug": "fleet_manager", "_remove": true}
		],
		"records": {"kiosk.KioskProfile": {"rows": [{"code": "retail", "name": "Mostrador"}, {"code": "llantera", "name": "Llantera"}]}},
		"terminology": {"es": {"sales.SalesOrder": "Orden"}}
	}`)
	got, err := Flatten(base, ov)
	if err != nil {
		t.Fatalf("flatten: %v", err)
	}
	want := decode(t, `{
		"branding": {"platform_name": "Siete Leguas", "primary_color": "#C8102E"},
		"ui_theme": {"theme_key": "classic", "font": "manrope"},
		"onboarding": {"context": "siete leguas"},
		"roles": [
			{"slug": "cajero", "name": "Cajero", "icon": "Wallet", "permissions": ["pos.session.open", "caja.cashcut.create"]},
			{"slug": "vendedor", "name": "Vendedor"}
		],
		"records": {"kiosk.KioskProfile": {"key": "code", "rows": [{"code": "retail", "name": "Mostrador"}, {"code": "llantera", "name": "Llantera"}]}},
		"terminology": {"es": {"sales.SalesOrder": "Orden"}}
	}`)
	if !reflect.DeepEqual(got.Defaults, want) {
		g, _ := json.MarshalIndent(got.Defaults, "", " ")
		t.Fatalf("defaults:\n%s", g)
	}
	// Base defaults untouched.
	if base.Defaults["branding"].(map[string]interface{})["platform_name"] != "Pitsline" {
		t.Fatalf("base defaults mutated")
	}
}

func TestInlineFiles(t *testing.T) {
	files := map[string]string{
		"defaults/roles.json": `[{"slug": "cajero"}]`,
		"defaults/bad.json":   `{`,
	}
	read := func(p string) ([]byte, error) {
		s, ok := files[p]
		if !ok {
			return nil, errors.New("missing")
		}
		return []byte(s), nil
	}
	in := decode(t, `{
		"roles_file": "./defaults/roles.json",
		"onboarding_file": "./defaults/missing.json",
		"onboarding": {"context": "inline wins"},
		"categories_file": "./defaults/missing.json",
		"branding_file": "./defaults/bad.json"
	}`)
	out, failed := InlineFiles(in, read)
	if !reflect.DeepEqual(out["roles"], []interface{}{map[string]interface{}{"slug": "cajero"}}) {
		t.Fatalf("roles = %v", out["roles"])
	}
	if out["onboarding"].(map[string]interface{})["context"] != "inline wins" {
		t.Fatalf("inline value must win")
	}
	if want := []string{"branding_file", "categories_file"}; !reflect.DeepEqual(failed, want) {
		t.Fatalf("failed = %v, want %v", failed, want)
	}
	if _, ok := in["roles"]; ok {
		t.Fatalf("input mutated")
	}
}
