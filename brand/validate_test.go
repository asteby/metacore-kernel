package brand

import (
	"errors"
	"strings"
	"testing"
)

func validSource() Source {
	return Source{
		Key: "vantora", Name: "Vantora", Color: "#0033f0",
		Icon: File{Bytes: svg("0 0 1 1"), Width: 1, Height: 1},
	}
}

func TestSourceValidate(t *testing.T) {
	if err := validSource().Validate(); err != nil {
		t.Fatal(err)
	}
	if err := Check(testSource()); err != nil {
		t.Fatalf("existing test source: %v", err)
	}
	cases := map[string]func(*Source){
		"key":          func(s *Source) { s.Key = "V" },
		"name empty":   func(s *Source) { s.Name = "" },
		"name control": func(s *Source) { s.Name = "a\x00b" },
		"name long":    func(s *Source) { s.Name = strings.Repeat("a", 61) },
		"color":        func(s *Source) { s.Color = "red" },
		"theme color":  func(s *Source) { s.Theme.Accent = "#12" },
		"dark color":   func(s *Source) { s.Theme.Dark = &ThemeColors{Primary: "x"} },
		"gradient 5":   func(s *Source) { s.Theme.Gradient = []string{"#111", "#222", "#333", "#444", "#555"} },
		"gradient 1":   func(s *Source) { s.Theme.Gradient = []string{"#111"} },
		"namespace":    func(s *Source) { s.StorageNamespace = "Bad_NS" },
		"names locale": func(s *Source) { s.Names = map[string]string{"not a locale": "X"} },
		"no icon":      func(s *Source) { s.Icon = File{} },
		"svg script":   func(s *Source) { s.Logo = File{Bytes: []byte(`<svg ` + svgNS + `><script/></svg>`)} },
		"mp4":          func(s *Source) { s.Animated = File{Bytes: []byte("x"), Type: "video/mp4"} },
		"fake png":     func(s *Source) { s.Maskable = File{Bytes: []byte("x"), Type: "image/png"} },
		"maskable svg": func(s *Source) { s.Maskable = File{Bytes: svg("0 0 1 1")} },
		"oversize":     func(s *Source) { s.Logo = File{Bytes: append(svg("0 0 1 1"), make([]byte, MaxAssetBytes)...)} },
		"animated smil": func(s *Source) {
			s.Icon = File{Bytes: []byte(`<svg ` + svgNS + `><animate attributeName="opacity"/></svg>`)}
		},
		"animated href": func(s *Source) {
			s.Animated = File{Bytes: []byte(`<svg ` + svgNS + `><animate attributeName="href"/></svg>`)}
		},
		"bad mascot":     func(s *Source) { s.Assistant.Mascot = &MascotSkin{ID: "x"} },
		"assistant name": func(s *Source) { s.Assistant.Name = strings.Repeat("a", 41) },
	}
	for name, mut := range cases {
		s := validSource()
		mut(&s)
		err := s.Validate()
		if err == nil {
			t.Errorf("%s: accepted", name)
		} else if !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: not ErrInvalid: %v", name, err)
		}
	}
}

func TestSourceValidateAcceptsSMILInAnimated(t *testing.T) {
	s := validSource()
	s.Animated = File{Bytes: []byte(`<svg ` + svgNS + `><rect><animate attributeName="opacity" from="0" to="1" dur="1s"/></rect></svg>`)}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestMustValidatePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("no panic")
		}
	}()
	MustValidate(Source{})
}

func TestNewHandlerDoesNotValidate(t *testing.T) {
	// compat: a partial Source (no key) still serves what it has.
	m := manifestOf(t, Handler(Source{Name: "X", Icon: File{Bytes: svg("0 0 1 1")}}), "/api/brand")
	if m.Name != "X" {
		t.Fatalf("manifest = %+v", m)
	}
}

func TestWarnings(t *testing.T) {
	s := validSource()
	s.Favicon = File{Bytes: append(svg("0 0 1 1"), make([]byte, faviconSVGWarnBytes)...)}
	w := strings.Join(s.Warnings(), "|")
	if !strings.Contains(w, "favicon is an SVG") || !strings.Contains(w, "maskable") {
		t.Fatalf("warnings = %q", w)
	}
}

func TestMerge(t *testing.T) {
	base := validSource()
	base.Names = map[string]string{"en": "Vantora", "es": "Vantora ES"}
	base.Theme = Theme{Primary: "#0033f0", Gradient: []string{"#00fcff", "#0033f0"}, Dark: &ThemeColors{Primary: "#00fcff", Background: "#020846"}}
	base.Logo = File{Bytes: svg("0 0 2 1")}
	base.Assistant = Assistant{Name: "Vanta"}

	over := Source{
		Name:      "Vantora Gov",
		Names:     map[string]string{"es": "Vantora Gobierno"},
		Theme:     Theme{Accent: "#111111", Dark: &ThemeColors{Primary: "#ffffff"}},
		Logo:      File{Bytes: svg("0 0 3 1")},
		Assistant: Assistant{Mascot: &MascotSkin{ID: "custom"}},
	}
	got := Merge(base, over)
	if got.Key != "vantora" || got.Name != "Vantora Gov" || got.Color != "#0033f0" {
		t.Fatalf("scalars: %+v", got)
	}
	if got.Names["en"] != "Vantora" || got.Names["es"] != "Vantora Gobierno" {
		t.Fatalf("names: %v", got.Names)
	}
	if got.Theme.Primary != "#0033f0" || got.Theme.Accent != "#111111" || len(got.Theme.Gradient) != 2 {
		t.Fatalf("theme: %+v", got.Theme)
	}
	if got.Theme.Dark.Primary != "#ffffff" || got.Theme.Dark.Background != "#020846" {
		t.Fatalf("theme.dark: %+v", got.Theme.Dark)
	}
	if string(got.Logo.Bytes) != string(svg("0 0 3 1")) || string(got.Icon.Bytes) != string(base.Icon.Bytes) {
		t.Fatal("files not merged per slot")
	}
	if got.Assistant.Name != "Vanta" || got.Assistant.Mascot == nil || got.Assistant.Mascot.ID != "custom" {
		t.Fatalf("assistant: %+v", got.Assistant)
	}
	// base is untouched
	if base.Theme.Dark.Primary != "#00fcff" || base.Names["es"] != "Vantora ES" || base.Name != "Vantora" {
		t.Fatal("Merge mutated base")
	}
	// the zero overlay is the identity
	id := Merge(base, Source{})
	if id.Name != base.Name || id.Theme.Dark.Primary != "#00fcff" || len(id.Theme.Gradient) != 2 {
		t.Fatalf("zero overlay changed base: %+v", id)
	}
}
