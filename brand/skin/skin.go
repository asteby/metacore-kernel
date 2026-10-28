// Package skin holds the declarative mascot skin shared by the product brand
// (brand.Assistant.Mascot) and the organization branding
// (manifest/v3 Branding.Mascot). It is a leaf package with no dependency on
// the rest of the kernel so both can import it without a cycle.
//
// The JSON shape mirrors the SDK MascotSkin type one to one (snake_case on the
// wire, camelCase in TypeScript).
package skin

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// Tooth shapes the SDK mascot knows how to draw.
const (
	ToothTireTread = "tire-tread"
	ToothGearTeeth = "gear-teeth"
	ToothHexFrame  = "hex-frame"
)

// Screen tones.
const (
	ScreenLight = "light"
	ScreenDark  = "dark"
)

// MascotSkin is a palette + tooth-shape configuration for the assistant
// mascot. Adding a vertical's mascot never needs component code, only data.
type MascotSkin struct {
	// ID is the stable skin id, ^[a-z][a-z0-9-]{1,31}$.
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	// ToothShape is tire-tread | gear-teeth | hex-frame.
	ToothShape string `json:"tooth_shape"`
	Antenna    bool   `json:"antenna"`
	// ScreenTone is light | dark.
	ScreenTone string       `json:"screen_tone"`
	Colors     MascotColors `json:"colors"`
	// Face optionally overrides the stroke of the face (a skin whose accent
	// disappears into its screen).
	Face *MascotFace `json:"face,omitempty"`
}

// MascotColors are the six palette entries, all #RGB or #RRGGBB.
type MascotColors struct {
	Tire         string `json:"tire"`
	TreadOrTooth string `json:"tread_or_tooth"`
	Accent       string `json:"accent"`
	Glow         string `json:"glow"`
	Screen       string `json:"screen"`
	ScreenCenter string `json:"screen_center"`
}

// MascotFace overrides how the face is drawn.
type MascotFace struct {
	Stroke      string  `json:"stroke,omitempty"`
	StrokeWidth float64 `json:"stroke_width,omitempty"`
	TreadDot    string  `json:"tread_dot,omitempty"`
}

var (
	idRe  = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)
	hexRe = regexp.MustCompile(`^#([0-9a-fA-F]{3}|[0-9a-fA-F]{6})$`)
)

// ValidID reports whether s is a valid skin id.
func ValidID(s string) bool { return idRe.MatchString(s) }

// ValidColor reports whether s is a #RGB or #RRGGBB color.
func ValidColor(s string) bool { return hexRe.MatchString(s) }

// Validate reports every problem of the skin (joined), or nil.
func (s MascotSkin) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf("mascot: "+format, a...)) }
	if !idRe.MatchString(s.ID) {
		add("id %q must match %s", s.ID, idRe)
	}
	if n := len([]rune(s.Name)); n > 40 || strings.IndexFunc(s.Name, unicode.IsControl) >= 0 {
		add("name must be at most 40 characters without control characters")
	}
	switch s.ToothShape {
	case ToothTireTread, ToothGearTeeth, ToothHexFrame:
	default:
		add("tooth_shape %q must be tire-tread, gear-teeth or hex-frame", s.ToothShape)
	}
	switch s.ScreenTone {
	case ScreenLight, ScreenDark:
	default:
		add("screen_tone %q must be light or dark", s.ScreenTone)
	}
	for name, v := range map[string]string{
		"colors.tire": s.Colors.Tire, "colors.tread_or_tooth": s.Colors.TreadOrTooth,
		"colors.accent": s.Colors.Accent, "colors.glow": s.Colors.Glow,
		"colors.screen": s.Colors.Screen, "colors.screen_center": s.Colors.ScreenCenter,
	} {
		if !hexRe.MatchString(v) {
			add("%s %q must be #RGB or #RRGGBB", name, v)
		}
	}
	if f := s.Face; f != nil {
		if f.Stroke != "" && !hexRe.MatchString(f.Stroke) {
			add("face.stroke %q must be #RGB or #RRGGBB", f.Stroke)
		}
		if f.TreadDot != "" && !hexRe.MatchString(f.TreadDot) {
			add("face.tread_dot %q must be #RGB or #RRGGBB", f.TreadDot)
		}
		if f.StrokeWidth < 0 || f.StrokeWidth > 20 {
			add("face.stroke_width %v must be between 0 and 20", f.StrokeWidth)
		}
	}
	return errors.Join(errs...)
}
