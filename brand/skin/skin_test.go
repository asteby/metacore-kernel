package skin

import (
	"encoding/json"
	"strings"
	"testing"
)

func valid() MascotSkin {
	return MascotSkin{
		ID: "hex-navy", Name: "Asistente", ToothShape: ToothHexFrame, ScreenTone: ScreenDark,
		Colors: MascotColors{Tire: "#051C35", TreadOrTooth: "#102E6B", Accent: "#2160DE", Glow: "#628FEA", Screen: "#041324", ScreenCenter: "#071A36"},
		Face:   &MascotFace{Stroke: "#f4f8ff", StrokeWidth: 6.5, TreadDot: "#f4f8ff"},
	}
}

func TestValidate(t *testing.T) {
	if err := valid().Validate(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*MascotSkin){
		"id":         func(s *MascotSkin) { s.ID = "Bad Id" },
		"tooth":      func(s *MascotSkin) { s.ToothShape = "round" },
		"tone":       func(s *MascotSkin) { s.ScreenTone = "" },
		"color":      func(s *MascotSkin) { s.Colors.Glow = "red" },
		"face":       func(s *MascotSkin) { s.Face.Stroke = "x" },
		"face width": func(s *MascotSkin) { s.Face.StrokeWidth = 99 },
		"name":       func(s *MascotSkin) { s.Name = strings.Repeat("a", 41) },
	}
	for name, mut := range cases {
		s := valid()
		mut(&s)
		if s.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestJSONWire(t *testing.T) {
	b, _ := json.Marshal(valid())
	for _, key := range []string{`"tooth_shape"`, `"screen_tone"`, `"tread_or_tooth"`, `"screen_center"`, `"stroke_width"`, `"tread_dot"`, `"antenna":false`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("wire JSON lacks %s: %s", key, b)
		}
	}
}
