package colorx

import (
	"bytes"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math"
	"os"
	"strings"
	"testing"
)

const tol = 0.01

type vectors struct {
	OnColor []struct {
		Bg     string  `json:"bg"`
		Min    float64 `json:"min"`
		Expect string  `json:"expect"`
		Ratio  float64 `json:"ratio"`
	} `json:"onColor"`
	EnsureContrast []struct {
		Fg     string  `json:"fg"`
		Bg     string  `json:"bg"`
		Min    float64 `json:"min"`
		Expect string  `json:"expect"`
		Ratio  float64 `json:"ratio"`
	} `json:"ensureContrast"`
	PickBacking []struct {
		Fg         string   `json:"fg"`
		Candidates []string `json:"candidates"`
		Expect     string   `json:"expect"`
		Ratio      float64  `json:"ratio"`
	} `json:"pickBacking"`
}

func loadVectors(t *testing.T) vectors {
	t.Helper()
	raw, err := os.ReadFile("testdata/contrast-vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.OnColor) == 0 || len(v.EnsureContrast) == 0 || len(v.PickBacking) == 0 {
		t.Fatal("vectors file is missing a section")
	}
	return v
}

func ratioOf(t *testing.T, fg, bg string) float64 {
	t.Helper()
	r, err := Contrast(fg, bg)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestVectors(t *testing.T) {
	v := loadVectors(t)
	t.Run("onColor", func(t *testing.T) {
		for _, c := range v.OnColor {
			got, err := OnColor(c.Bg, OnOptions{Min: c.Min})
			if err != nil || got != c.Expect {
				t.Errorf("OnColor(%s,%v) = %q,%v; want %q", c.Bg, c.Min, got, err, c.Expect)
				continue
			}
			if r := ratioOf(t, got, c.Bg); math.Abs(r-c.Ratio) > tol || r < c.Min {
				t.Errorf("OnColor(%s) ratio %.3f; want %.3f (>= %v)", c.Bg, r, c.Ratio, c.Min)
			}
		}
	})
	t.Run("ensureContrast", func(t *testing.T) {
		for _, c := range v.EnsureContrast {
			got, err := EnsureContrast(c.Fg, c.Bg, c.Min)
			if err != nil || got != c.Expect {
				t.Errorf("EnsureContrast(%s,%s,%v) = %q,%v; want %q", c.Fg, c.Bg, c.Min, got, err, c.Expect)
				continue
			}
			if r := ratioOf(t, got, c.Bg); math.Abs(r-c.Ratio) > tol {
				t.Errorf("EnsureContrast(%s,%s) ratio %.3f; want %.3f", c.Fg, c.Bg, r, c.Ratio)
			}
		}
	})
	t.Run("pickBacking", func(t *testing.T) {
		for _, c := range v.PickBacking {
			got, r, err := PickBacking(c.Fg, c.Candidates)
			if err != nil || got != c.Expect || math.Abs(r-c.Ratio) > tol {
				t.Errorf("PickBacking(%s,%v) = %q,%.3f,%v; want %q,%.3f", c.Fg, c.Candidates, got, r, err, c.Expect, c.Ratio)
			}
		}
	})
}

func TestWCAGReferenceRatios(t *testing.T) {
	cases := []struct {
		fg, bg string
		want   float64
	}{
		{"#000000", "#ffffff", 21},
		{"#ffffff", "#ffffff", 1},
		{"#767676", "#ffffff", 4.54}, // smallest gray that passes AA on white
		{"#777777", "#ffffff", 4.48}, // one step below: fails AA
		{"#808080", "#ffffff", 3.95},
	}
	for _, c := range cases {
		if r := ratioOf(t, c.fg, c.bg); math.Abs(r-c.want) > tol {
			t.Errorf("ratio(%s,%s) = %.3f; want %.2f", c.fg, c.bg, r, c.want)
		}
	}
	if ratioOf(t, "#767676", "#fff") < MinText || ratioOf(t, "#777", "#fff") >= MinText {
		t.Error("AA boundary between #767676 and #777777 not respected")
	}
}

func TestParseColor(t *testing.T) {
	ok := []struct {
		in   string
		want RGBA
	}{
		{"#fff", RGBA{1, 1, 1, 1}},
		{"#FFF", RGBA{1, 1, 1, 1}},
		{"#0008", RGBA{0, 0, 0, 0x88 / 255.0}},
		{"#ff000080", RGBA{1, 0, 0, 0x80 / 255.0}},
		{"rgb(255, 0, 0)", RGBA{1, 0, 0, 1}},
		{"rgb(255 0 0 / 50%)", RGBA{1, 0, 0, 0.5}},
		{"rgba(0,0,255,0.25)", RGBA{0, 0, 1, 0.25}},
		{"rgb(100% 0% 0%)", RGBA{1, 0, 0, 1}},
		{"hsl(0 100% 50%)", RGBA{1, 0, 0, 1}},
		{"hsl(120, 100%, 50%)", RGBA{0, 1, 0, 1}},
		{"hsla(240deg 100% 50% / 0.5)", RGBA{0, 0, 1, 0.5}},
		{"oklch(1 0 0)", RGBA{1, 1, 1, 1}},
		{"oklch(0 0 0 / 0.3)", RGBA{0, 0, 0, 0.3}},
		{"white", RGBA{1, 1, 1, 1}},
		{" Black ", RGBA{0, 0, 0, 1}},
		{"transparent", RGBA{0, 0, 0, 0}},
	}
	for _, c := range ok {
		got, err := ParseColor(c.in)
		if err != nil {
			t.Errorf("ParseColor(%q): %v", c.in, err)
			continue
		}
		if math.Abs(got.R-c.want.R) > 1e-3 || math.Abs(got.G-c.want.G) > 1e-3 || math.Abs(got.B-c.want.B) > 1e-3 || math.Abs(got.A-c.want.A) > 1e-3 {
			t.Errorf("ParseColor(%q) = %+v; want %+v", c.in, got, c.want)
		}
	}
	for _, in := range []string{"", "#", "#12", "#12345", "#gggggg", "red", "var(--x)", "color-mix(in srgb, red, blue)", "rgb(1,2)", "rgb(a,b,c)", "hsl(x 1% 1%)", "oklch(0.5 x 1)", "rgb(1 2 3", "linear-gradient(red, blue)"} {
		if _, err := ParseColor(in); !errors.Is(err, ErrInvalidColor) {
			t.Errorf("ParseColor(%q) err = %v; want ErrInvalidColor", in, err)
		}
	}
}

func TestOklchRoundTrip(t *testing.T) {
	for _, hex := range []string{"#2f7fd6", "#f5c518", "#e11d48", "#101828"} {
		c, _ := ParseColor(hex)
		l, ch, h := RGBToOklch(c.R, c.G, c.B)
		r, g, b := OklchToRGB(l, ch, h)
		if math.Abs(r-c.R) > 1e-3 || math.Abs(g-c.G) > 1e-3 || math.Abs(b-c.B) > 1e-3 {
			t.Errorf("%s round trip = %v %v %v", hex, r, g, b)
		}
	}
}

func TestOnColorGuarantee(t *testing.T) {
	// With the default white/black candidates every opaque color gets >= 4.5.
	for r := 0; r < 256; r += 5 {
		for g := 0; g < 256; g += 5 {
			for b := 0; b < 256; b += 5 {
				bg := RGBA{float64(r) / 255, float64(g) / 255, float64(b) / 255, 1}.Hex()
				on, err := OnColor(bg, OnOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if got := ratioOf(t, on, bg); got < MinText {
					t.Fatalf("OnColor(%s) = %s ratio %.3f < 4.5", bg, on, got)
				}
			}
		}
	}
}

func TestOnColorCustomCandidatesAdjust(t *testing.T) {
	// Mid-gray candidates cannot reach 4.5 on a mid background: OKLCH adjust.
	got, err := OnColor("#808080", OnOptions{Light: "#999999", Dark: "#666666"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "oklch(") {
		t.Fatalf("expected adjusted oklch(), got %q", got)
	}
	if r := ratioOf(t, got, "#808080"); r < MinText {
		t.Errorf("adjusted ratio %.3f < 4.5", r)
	}
}

func TestOnColorTranslucentBgFlattensOverWhite(t *testing.T) {
	// 50% black over white is a light gray: dark text.
	got, err := OnColor("#00000080", OnOptions{})
	if err != nil || got != "#000000" {
		t.Errorf("got %q,%v; want #000000", got, err)
	}
}

func TestOnColorErrors(t *testing.T) {
	for _, o := range []OnOptions{{}, {Light: "nope"}, {Dark: "nope"}} {
		bg := "#fff"
		if o == (OnOptions{}) {
			bg = "nope"
		}
		if _, err := OnColor(bg, o); !errors.Is(err, ErrInvalidColor) {
			t.Errorf("OnColor(%q,%+v) err = %v", bg, o, err)
		}
	}
}

func TestEnsureContrastUnreachableReturnsExtreme(t *testing.T) {
	got, err := EnsureContrast("#808080", "#808080", 21)
	if err != nil || !strings.HasPrefix(got, "oklch(0.0000 ") && !strings.HasPrefix(got, "oklch(1.0000 ") {
		t.Errorf("got %q,%v; want an L=0 or L=1 extreme", got, err)
	}
	if _, err := EnsureContrast("x", "#fff", 4.5); !errors.Is(err, ErrInvalidColor) {
		t.Error("invalid fg must error")
	}
	if _, err := EnsureContrast("#000", "x", 4.5); !errors.Is(err, ErrInvalidColor) {
		t.Error("invalid bg must error")
	}
}

func TestEnsureContrastDirection(t *testing.T) {
	// Dark background: lightens. Light background: darkens.
	dark, _ := EnsureContrast("#0b3d91", "#101828", 4.5)
	light, _ := EnsureContrast("#f5c518", "#ffffff", 4.5)
	if ratioOf(t, dark, "#101828") < MinText || ratioOf(t, light, "#ffffff") < MinText {
		t.Errorf("ensure failed: %s %s", dark, light)
	}
}

func TestPickBackingEdge(t *testing.T) {
	if _, _, err := PickBacking("#fff", nil); !errors.Is(err, ErrInvalidColor) {
		t.Error("empty candidates must error")
	}
	if _, _, err := PickBacking("bad", []string{"#000"}); !errors.Is(err, ErrInvalidColor) {
		t.Error("invalid fg must error")
	}
	got, _, err := PickBacking("#fff", []string{"bad", "#000", "#222"})
	if err != nil || got != "#000" {
		t.Errorf("skip invalid candidates: got %q,%v", got, err)
	}
	// Ties: first wins.
	got, _, _ = PickBacking("#fff", []string{"#000000", "black"})
	if got != "#000000" {
		t.Errorf("tie must keep first, got %q", got)
	}
	// White logo never lands on a white backing.
	got, _, _ = PickBacking("#ffffff", []string{"#ffffff", "#2f7fd6"})
	if got != "#2f7fd6" {
		t.Errorf("white logo backing = %q", got)
	}
}

func TestAlphaForeground(t *testing.T) {
	// 50% white over black is mid gray (#808080): ratio ~5.3 vs black.
	fg, _ := ParseColor("#ffffff80")
	bg, _ := ParseColor("#000000")
	if r := ContrastRatio(fg, bg); r < 5 || r > 5.6 {
		t.Errorf("translucent fg ratio = %.3f", r)
	}
}

// ---- ImageTone -----------------------------------------------------------

func solid(w, h int, c color.NRGBA) *image.NRGBA {
	im := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			im.SetNRGBA(x, y, c)
		}
	}
	return im
}

func encPNG(t *testing.T, im image.Image) *bytes.Reader {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, im); err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(b.Bytes())
}

func TestImageTone(t *testing.T) {
	// Logo: an opaque square on a fully transparent canvas.
	logo := func(c color.NRGBA) *image.NRGBA {
		im := solid(100, 100, color.NRGBA{})
		for y := 30; y < 70; y++ {
			for x := 30; x < 70; x++ {
				im.SetNRGBA(x, y, c)
			}
		}
		return im
	}
	cases := []struct {
		name string
		im   image.Image
		tone Tone
		hex  string
	}{
		{"white logo on transparent", logo(color.NRGBA{255, 255, 255, 255}), ToneLight, "#ffffff"},
		{"black logo on transparent", logo(color.NRGBA{0, 0, 0, 255}), ToneDark, "#000000"},
		{"brand blue is mid", solid(8, 8, color.NRGBA{0x2f, 0x7f, 0xd6, 255}), ToneMid, "#2f7fd6"},
		{"faint pixels ignored", func() image.Image {
			im := logo(color.NRGBA{0, 0, 0, 255})
			for x := 0; x < 100; x++ {
				im.SetNRGBA(x, 0, color.NRGBA{255, 255, 255, 40}) // alpha == 40: not opaque
			}
			return im
		}(), ToneDark, "#000000"},
	}
	for _, c := range cases {
		got, err := ImageTone(encPNG(t, c.im))
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got.Tone != c.tone || got.Color != c.hex || got.Samples == 0 {
			t.Errorf("%s: got %+v; want tone %s color %s", c.name, got, c.tone, c.hex)
		}
	}
}

func TestImageToneTransparent(t *testing.T) {
	_, err := ImageTone(encPNG(t, solid(10, 10, color.NRGBA{255, 255, 255, 0})))
	if !errors.Is(err, ErrNoOpaquePixels) {
		t.Errorf("err = %v; want ErrNoOpaquePixels", err)
	}
}

func TestImageToneFormats(t *testing.T) {
	white := solid(20, 20, color.NRGBA{255, 255, 255, 255})
	var j, g bytes.Buffer
	if err := jpeg.Encode(&j, white, nil); err != nil {
		t.Fatal(err)
	}
	if err := gif.Encode(&g, white, nil); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"jpeg": j.Bytes(), "gif": g.Bytes()} {
		got, err := ImageTone(bytes.NewReader(data))
		if err != nil || got.Tone != ToneLight {
			t.Errorf("%s: %+v, %v", name, got, err)
		}
	}
}

func TestImageToneUnsupported(t *testing.T) {
	for name, data := range map[string]string{
		"svg":       `<svg xmlns="http://www.w3.org/2000/svg"><rect width="1" height="1"/></svg>`,
		"webp":      "RIFF\x00\x00\x00\x00WEBPVP8 ",
		"empty":     "",
		"truncated": "\x89PNG\r\n\x1a\n\x00\x00",
	} {
		if _, err := ImageTone(strings.NewReader(data)); !errors.Is(err, ErrUnsupportedImage) {
			t.Errorf("%s: err = %v; want ErrUnsupportedImage", name, err)
		}
	}
}

func TestImageToneTooLarge(t *testing.T) {
	// Header-only PNG claiming 100000x100000: must be refused before decoding.
	var b bytes.Buffer
	b.WriteString("\x89PNG\r\n\x1a\n")
	ihdr := []byte{0, 0, 0, 13, 'I', 'H', 'D', 'R', 0, 1, 0x86, 0xa0, 0, 1, 0x86, 0xa0, 8, 6, 0, 0, 0}
	b.Write(ihdr)
	b.Write([]byte{0, 0, 0, 0}) // CRC is not checked by DecodeConfig
	_, err := ImageTone(&b)
	if !errors.Is(err, ErrImageTooLarge) && !errors.Is(err, ErrUnsupportedImage) {
		t.Errorf("err = %v", err)
	}
}

func TestImageToneSamplesLargeImageSparsely(t *testing.T) {
	got, err := ImageTone(encPNG(t, solid(1024, 1024, color.NRGBA{255, 255, 255, 255})))
	if err != nil {
		t.Fatal(err)
	}
	if got.Samples > (MaxSampleSide+1)*(MaxSampleSide+1) {
		t.Errorf("sampled %d pixels; grid must be bounded", got.Samples)
	}
}

func TestClassifyLuminance(t *testing.T) {
	for l, want := range map[float64]Tone{0: ToneDark, DarkMax: ToneDark, 0.3: ToneMid, LightMin: ToneLight, 1: ToneLight} {
		if got := ClassifyLuminance(l); got != want {
			t.Errorf("ClassifyLuminance(%v) = %s; want %s", l, got, want)
		}
	}
}
