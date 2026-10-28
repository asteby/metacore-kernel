package brand

import (
	"bytes"
	_ "embed" // brand.schema.json
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // registers the decoder used by DecodeConfig
	"image/png"
	"io"
	"io/fs"
	"path"
	"strconv"
	"strings"
)

// PackFile is the brand pack descriptor, brand.json at the root of the pack
// directory. Its JSON Schema is published as brand.schema.json (SchemaJSON).
// A pack is data, not code: a new product ships a folder, not Go.
type PackFile struct {
	Key              string            `json:"key"`
	Name             string            `json:"name"`
	ShortName        string            `json:"short_name,omitempty"`
	Description      string            `json:"description,omitempty"`
	Names            map[string]string `json:"names,omitempty"`
	Color            string            `json:"color,omitempty"`
	Theme            *Theme            `json:"theme,omitempty"`
	Assistant        *PackAssistant    `json:"assistant,omitempty"`
	StorageNamespace string            `json:"storage_namespace,omitempty"`
	Assets           PackAssets        `json:"assets"`
}

// PackAssistant is the assistant block of brand.json; avatar is a file name.
type PackAssistant struct {
	Name   string      `json:"name,omitempty"`
	Avatar string      `json:"avatar,omitempty"`
	Mascot *MascotSkin `json:"mascot,omitempty"`
}

// PackAssets maps each artwork slot to a file inside the pack.
type PackAssets struct {
	Icon         string `json:"icon"`
	Logo         string `json:"logo,omitempty"`
	LogoDark     string `json:"logo_dark,omitempty"`
	Favicon      string `json:"favicon,omitempty"`
	Maskable     string `json:"maskable,omitempty"`
	Monochrome   string `json:"monochrome,omitempty"`
	Animated     string `json:"animated,omitempty"`
	AnimatedDark string `json:"animated_dark,omitempty"`
	OG           string `json:"og,omitempty"`
}

// SchemaJSON is the JSON Schema of brand.json (PackFile), for editors and CI.
//
//go:embed brand.schema.json
var SchemaJSON []byte

// PackName is the descriptor file name inside a pack.
const PackName = "brand.json"

// maxPackJSON bounds brand.json itself.
const maxPackJSON = 64 << 10

// LoadFS reads a brand pack: brand.json plus the artwork files it references.
// Asset paths are flat file names inside the pack (or one level under
// assets/); "..", absolute paths, backslashes and deeper nesting are rejected.
// MIME comes from the extension and dimensions from the SVG viewBox/width/height
// or the PNG/JPEG header. Colors are normalized to lowercase #rrggbb and the
// result is validated, so a nil error means the Source is safe to serve.
func LoadFS(fsys fs.FS) (Source, error) {
	raw, err := readLimited(fsys, PackName, maxPackJSON)
	if err != nil {
		return Source{}, fmt.Errorf("brand: %w", err)
	}
	var p PackFile
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return Source{}, fmt.Errorf("brand: %s: %w", PackName, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return Source{}, fmt.Errorf("brand: %s: trailing content after the JSON document", PackName)
	}
	src := Source{
		Key: p.Key, Name: p.Name, ShortName: p.ShortName, Description: p.Description,
		Names: p.Names, Color: p.Color, StorageNamespace: p.StorageNamespace,
	}
	if p.Theme != nil {
		src.Theme = *p.Theme
	}
	var errs []error
	load := func(slot, name string, dst *File) {
		if name == "" {
			return
		}
		f, err := readAsset(fsys, name)
		if err != nil {
			errs = append(errs, fmt.Errorf("brand: assets.%s: %w", slot, err))
			return
		}
		*dst = f
	}
	a := p.Assets
	load("icon", a.Icon, &src.Icon)
	load("logo", a.Logo, &src.Logo)
	load("logo_dark", a.LogoDark, &src.LogoDark)
	load("favicon", a.Favicon, &src.Favicon)
	load("maskable", a.Maskable, &src.Maskable)
	load("monochrome", a.Monochrome, &src.Monochrome)
	load("animated", a.Animated, &src.Animated)
	load("animated_dark", a.AnimatedDark, &src.AnimatedDark)
	load("og", a.OG, &src.OG)
	if p.Assistant != nil {
		src.Assistant.Name, src.Assistant.Mascot = p.Assistant.Name, p.Assistant.Mascot
		load("assistant.avatar", p.Assistant.Avatar, &src.Assistant.Avatar)
	}
	if len(errs) > 0 {
		return Source{}, errors.Join(errs...)
	}
	src = src.Normalize()
	if src.Color == "" {
		src.Color = src.Theme.Primary
	}
	if err := src.Validate(); err != nil {
		return Source{}, err
	}
	return src, nil
}

// Normalize returns a copy with every color as lowercase #rrggbb (#rgb is
// expanded). Values that are not colors are left as they are for Validate to
// report.
func (s Source) Normalize() Source {
	s.Color = normColor(s.Color)
	t := s.Theme
	t.Primary, t.Accent = normColor(t.Primary), normColor(t.Accent)
	t.Background, t.Foreground = normColor(t.Background), normColor(t.Foreground)
	t.ThemeColor, t.BackgroundColor = normColor(t.ThemeColor), normColor(t.BackgroundColor)
	if len(t.Gradient) > 0 {
		g := make([]string, len(t.Gradient))
		for i, c := range t.Gradient {
			g[i] = normColor(c)
		}
		t.Gradient = g
	}
	if t.Dark != nil {
		d := *t.Dark
		d.Primary, d.Accent = normColor(d.Primary), normColor(d.Accent)
		d.Background, d.Foreground = normColor(d.Background), normColor(d.Foreground)
		t.Dark = &d
	}
	s.Theme = t
	return s
}

func normColor(c string) string {
	if len(c) == 0 || c[0] != '#' {
		return c
	}
	c = strings.ToLower(c)
	if len(c) == 4 {
		c = "#" + strings.Repeat(string(c[1]), 2) + strings.Repeat(string(c[2]), 2) + strings.Repeat(string(c[3]), 2)
	}
	return c
}

// Overlay holds the fields an operator overrides on top of a base identity
// (typically per instance). It has the shape of Source; empty fields mean
// "keep the base".
type Overlay = Source

// Merge returns base with every non-empty field of over applied on top, field
// by field: files replace the whole file, Names merge per locale, Theme merges
// per color (Gradient replaces as a whole) and the Assistant per field.
func Merge(base Source, over Overlay) Source {
	out := base
	str := func(dst *string, v string) {
		if v != "" {
			*dst = v
		}
	}
	file := func(dst *File, v File) {
		if v.present() {
			*dst = v
		}
	}
	str(&out.Key, over.Key)
	str(&out.Name, over.Name)
	str(&out.Color, over.Color)
	str(&out.ShortName, over.ShortName)
	str(&out.Description, over.Description)
	str(&out.StorageNamespace, over.StorageNamespace)
	file(&out.Icon, over.Icon)
	file(&out.Logo, over.Logo)
	file(&out.OG, over.OG)
	file(&out.LogoDark, over.LogoDark)
	file(&out.Favicon, over.Favicon)
	file(&out.Maskable, over.Maskable)
	file(&out.Monochrome, over.Monochrome)
	file(&out.Animated, over.Animated)
	file(&out.AnimatedDark, over.AnimatedDark)

	if len(base.Names) > 0 || len(over.Names) > 0 {
		out.Names = make(map[string]string, len(base.Names)+len(over.Names))
		for k, v := range base.Names {
			out.Names[k] = v
		}
		for k, v := range over.Names {
			if v != "" {
				out.Names[k] = v
			}
		}
	}

	t, o := &out.Theme, over.Theme
	str(&t.Primary, o.Primary)
	str(&t.Accent, o.Accent)
	str(&t.Background, o.Background)
	str(&t.Foreground, o.Foreground)
	str(&t.ThemeColor, o.ThemeColor)
	str(&t.BackgroundColor, o.BackgroundColor)
	if len(o.Gradient) > 0 {
		t.Gradient = append([]string(nil), o.Gradient...)
	} else {
		t.Gradient = append([]string(nil), base.Theme.Gradient...)
	}
	if base.Theme.Dark != nil || o.Dark != nil {
		var d ThemeColors
		if base.Theme.Dark != nil {
			d = *base.Theme.Dark
		}
		if o.Dark != nil {
			str(&d.Primary, o.Dark.Primary)
			str(&d.Accent, o.Dark.Accent)
			str(&d.Background, o.Dark.Background)
			str(&d.Foreground, o.Dark.Foreground)
		}
		t.Dark = &d
	}

	str(&out.Assistant.Name, over.Assistant.Name)
	file(&out.Assistant.Avatar, over.Assistant.Avatar)
	if over.Assistant.Mascot != nil {
		out.Assistant.Mascot = over.Assistant.Mascot
	}
	return out
}

func readLimited(fsys fs.FS, name string, max int64) ([]byte, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s exceeds %d bytes", name, max)
	}
	return b, nil
}

// packPath validates a pack-relative asset path: flat, or one level under assets/.
func packPath(name string) error {
	switch {
	case name == "":
		return errors.New("empty path")
	case strings.ContainsAny(name, "\\\x00"):
		return fmt.Errorf("path %q contains a backslash or NUL", name)
	case strings.HasPrefix(name, "/"):
		return fmt.Errorf("path %q is absolute", name)
	case strings.Contains(name, ".."):
		return fmt.Errorf("path %q contains \"..\"", name)
	case !fs.ValidPath(name) || path.Clean(name) != name:
		return fmt.Errorf("path %q is not clean", name)
	}
	switch strings.Count(name, "/") {
	case 0:
	case 1:
		if !strings.HasPrefix(name, "assets/") {
			return fmt.Errorf("path %q: only a flat name or assets/<name> is allowed", name)
		}
	default:
		return fmt.Errorf("path %q is nested too deep", name)
	}
	return nil
}

var extTypes = map[string]string{
	".svg": "image/svg+xml", ".png": "image/png", ".ico": "image/x-icon",
	".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".webp": "image/webp",
}

func readAsset(fsys fs.FS, name string) (File, error) {
	if err := packPath(name); err != nil {
		return File{}, err
	}
	mime, ok := extTypes[strings.ToLower(path.Ext(name))]
	if !ok {
		return File{}, fmt.Errorf("%q: extension not supported (svg, png, ico, jpg, webp)", name)
	}
	b, err := readLimited(fsys, name, MaxAssetBytes)
	if err != nil {
		return File{}, err
	}
	w, h := dimensions(mime, b)
	return File{Bytes: b, Type: mime, Width: w, Height: h}, nil
}

// dimensions returns best-effort pixel dimensions (0, 0 when unknown).
func dimensions(mime string, b []byte) (int, int) {
	switch mime {
	case "image/png":
		if cfg, err := png.DecodeConfig(bytes.NewReader(b)); err == nil {
			return cfg.Width, cfg.Height
		}
	case "image/jpeg":
		if cfg, _, err := image.DecodeConfig(bytes.NewReader(b)); err == nil {
			return cfg.Width, cfg.Height
		}
	case "image/svg+xml":
		return svgSize(b)
	}
	return 0, 0
}

// svgSize reads the root viewBox, else width/height, rounded to pixels.
func svgSize(b []byte) (int, int) {
	dec := xml.NewDecoder(bytes.NewReader(b))
	for {
		tok, err := dec.Token()
		if err != nil {
			return 0, 0
		}
		el, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		var vb, w, h string
		for _, a := range el.Attr {
			switch strings.ToLower(a.Name.Local) {
			case "viewbox":
				vb = a.Value
			case "width":
				w = a.Value
			case "height":
				h = a.Value
			}
		}
		if f := strings.FieldsFunc(vb, func(r rune) bool { return r == ' ' || r == ',' }); len(f) == 4 {
			vw, e1 := strconv.ParseFloat(f[2], 64)
			vh, e2 := strconv.ParseFloat(f[3], 64)
			if e1 == nil && e2 == nil && vw > 0 && vh > 0 {
				return int(vw + 0.5), int(vh + 0.5)
			}
		}
		return px(w), px(h)
	}
}

func px(s string) int {
	s = strings.TrimSuffix(strings.TrimSpace(s), "px")
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f <= 0 {
		return 0
	}
	return int(f + 0.5)
}
