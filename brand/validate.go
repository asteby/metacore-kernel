package brand

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"github.com/asteby/metacore-kernel/brand/skin"
	"github.com/asteby/metacore-kernel/svgsafe"
)

// MaxAssetBytes is the largest single asset Validate accepts. Heavy SVG logos
// (a few hundred KB with gradients) pass; run svgo before shipping bigger ones.
const MaxAssetBytes = 512 << 10

// faviconSVGWarnBytes: a favicon this big as SVG is wasteful; Warnings reports it.
const faviconSVGWarnBytes = 32 << 10

var (
	keyRe    = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)
	localeRe = regexp.MustCompile(`^[A-Za-z]{2,3}(-[A-Za-z0-9]{2,8})*$`)
	pngMagic = []byte("\x89PNG\r\n\x1a\n")
)

// ErrInvalid wraps every problem reported by Source.Validate (errors.Is).
var ErrInvalid = errors.New("invalid brand")

// Check validates src; it is Source.Validate as a function for call sites that
// read better that way (a CI test of a brand pack, a host's boot sequence).
func Check(src Source) error { return src.Validate() }

// MustValidate panics when src is invalid. For a host's boot sequence, where a
// broken brand should stop the process instead of shipping a broken identity.
func MustValidate(src Source) {
	if err := src.Validate(); err != nil {
		panic(err)
	}
}

// Validate reports every problem of the product identity (joined, errors.Is
// ErrInvalid), or nil. NewHandler never calls it, so an existing host that
// serves a partial Source keeps working.
func (s Source) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf("brand: "+format, a...)) }

	if !keyRe.MatchString(s.Key) {
		add("key %q must match %s", s.Key, keyRe)
	}
	if !textOK(s.Name, 1, 60) {
		add("name must be 1-60 characters without control characters")
	}
	if !textOK(s.ShortName, 0, 60) {
		add("short_name must be at most 60 characters without control characters")
	}
	if !textOK(s.Description, 0, 500) {
		add("description must be at most 500 characters without control characters")
	}
	for loc, n := range s.Names {
		if !localeRe.MatchString(loc) {
			add("names: locale %q is not a BCP-47 tag", loc)
		}
		if !textOK(n, 1, 60) {
			add("names[%s] must be 1-60 characters without control characters", loc)
		}
	}
	if s.StorageNamespace != "" && !keyRe.MatchString(s.StorageNamespace) {
		add("storage_namespace %q must match %s", s.StorageNamespace, keyRe)
	}
	color := func(field, v string) {
		if v != "" && !skin.ValidColor(v) {
			add("%s %q must be #RRGGBB", field, v)
		}
	}
	color("color", s.Color)
	t := s.Theme
	color("theme.primary", t.Primary)
	color("theme.accent", t.Accent)
	color("theme.background", t.Background)
	color("theme.foreground", t.Foreground)
	color("theme.theme_color", t.ThemeColor)
	color("theme.background_color", t.BackgroundColor)
	if d := t.Dark; d != nil {
		color("theme.dark.primary", d.Primary)
		color("theme.dark.accent", d.Accent)
		color("theme.dark.background", d.Background)
		color("theme.dark.foreground", d.Foreground)
	}
	if n := len(t.Gradient); n != 0 && (n < 2 || n > 4) {
		add("theme.gradient must have 2-4 colors, got %d", n)
	}
	for i, c := range t.Gradient {
		if !skin.ValidColor(c) {
			add("theme.gradient[%d] %q must be #RRGGBB", i, c)
		}
	}
	if !textOK(s.Assistant.Name, 0, 40) {
		add("assistant.name must be at most 40 characters without control characters")
	}
	if m := s.Assistant.Mascot; m != nil {
		if err := m.Validate(); err != nil {
			errs = append(errs, err)
		}
	}

	if !s.Icon.present() {
		add("icon is required")
	}
	for _, a := range []struct {
		name  string
		f     File
		kinds []string // allowed mime types; first is the preferred one
		anim  bool
	}{
		{"icon", s.Icon, imageSVGPNG, false},
		{"logo", s.Logo, imageSVGPNG, false},
		{"og", s.OG, imageAny, false},
		{"logo_dark", s.LogoDark, imageSVGPNG, false},
		{"favicon", s.Favicon, imageFavicon, false},
		{"maskable", s.Maskable, []string{"image/png"}, false},
		{"monochrome", s.Monochrome, []string{"image/svg+xml"}, false},
		{"animated", s.Animated, []string{"image/svg+xml"}, true},
		{"animated_dark", s.AnimatedDark, []string{"image/svg+xml"}, true},
		{"assistant", s.Assistant.Avatar, imageSVGPNG, false},
	} {
		if err := checkFile(a.f, a.kinds, a.anim); err != nil {
			add("assets.%s: %v", a.name, err)
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrInvalid, errors.Join(errs...))
}

// Warnings lists non-fatal smells of an otherwise valid identity.
func (s Source) Warnings() []string {
	var w []string
	if s.Favicon.present() && strings.HasPrefix(s.Favicon.mime(), "image/svg") && len(s.Favicon.Bytes) > faviconSVGWarnBytes {
		w = append(w, fmt.Sprintf("favicon is an SVG of %d bytes (> %d): ship a 32/48 px PNG instead", len(s.Favicon.Bytes), faviconSVGWarnBytes))
	}
	if !s.Favicon.present() && len(s.Icon.Bytes) > faviconSVGWarnBytes {
		w = append(w, fmt.Sprintf("icon doubles as favicon and weighs %d bytes (> %d): add a small favicon", len(s.Icon.Bytes), faviconSVGWarnBytes))
	}
	if !s.Maskable.present() {
		w = append(w, "no maskable icon: installed PWAs will crop the icon on Android")
	}
	return w
}

var (
	imageSVGPNG  = []string{"image/svg+xml", "image/png"}
	imageFavicon = []string{"image/svg+xml", "image/png", "image/x-icon", "image/vnd.microsoft.icon"}
	imageAny     = []string{"image/svg+xml", "image/png", "image/jpeg", "image/webp"}
)

func checkFile(f File, allowed []string, animated bool) error {
	if !f.present() {
		return nil
	}
	if len(f.Bytes) > MaxAssetBytes {
		return fmt.Errorf("%d bytes exceeds the %d byte limit", len(f.Bytes), MaxAssetBytes)
	}
	mime := f.mime()
	ok := false
	for _, a := range allowed {
		if a == mime {
			ok = true
		}
	}
	if !ok {
		return fmt.Errorf("type %q is not accepted here (allowed: %s)", mime, strings.Join(allowed, ", "))
	}
	if f.Width < 0 || f.Height < 0 {
		return errors.New("negative dimensions")
	}
	switch mime {
	case "image/svg+xml":
		var opts []svgsafe.Option
		if animated {
			opts = append(opts, svgsafe.AllowAnimation())
		}
		return svgsafe.Validate(f.Bytes, opts...)
	case "image/png":
		if !bytes.HasPrefix(f.Bytes, pngMagic) {
			return errors.New("declared image/png but the bytes are not a PNG")
		}
	}
	return nil
}

func textOK(s string, min, max int) bool {
	n := len([]rune(s))
	return n >= min && n <= max && strings.IndexFunc(s, unicode.IsControl) < 0
}
