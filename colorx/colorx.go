package colorx

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// WCAG 2.x minimum ratios.
const (
	// MinText is 1.4.3 AA for normal text; the default target everywhere.
	MinText = 4.5
	// MinLargeText is 1.4.3 AA for large text.
	MinLargeText = 3.0
	// MinNonText is 1.4.11 (control boundaries, marks).
	MinNonText = 3.0
	// MinTextAAA is 1.4.6 AAA for normal text.
	MinTextAAA = 7.0
)

// ErrInvalidColor is returned for any color string ParseColor cannot read.
var ErrInvalidColor = errors.New("colorx: invalid color")

// RGBA is a gamma-encoded sRGB color; every channel is 0..1.
type RGBA struct{ R, G, B, A float64 }

func clamp01(n float64) float64 { return math.Min(1, math.Max(0, n)) }

// parseNum mirrors the SDK parseNumber: "50%" -> 0.5*percentScale.
func parseNum(tok string, percentScale float64) (float64, error) {
	t := strings.TrimSpace(tok)
	if strings.HasSuffix(t, "%") {
		v, err := strconv.ParseFloat(strings.TrimSuffix(t, "%"), 64)
		return v / 100 * percentScale, err
	}
	return strconv.ParseFloat(t, 64)
}

// parseAngle accepts a bare number or one suffixed with "deg".
func parseAngle(tok string) (float64, error) {
	return strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(tok), "deg"), 64)
}

// OklchToRGB converts OKLCH to gamma sRGB, clipping out-of-gamut channels
// (as browsers paint).
func OklchToRGB(l, c, hDeg float64) (r, g, b float64) {
	h := hDeg * math.Pi / 180
	a := c * math.Cos(h)
	bb := c * math.Sin(h)
	l_ := l + 0.3963377774*a + 0.2158037573*bb
	m_ := l - 0.1055613458*a - 0.0638541728*bb
	s_ := l - 0.0894841775*a - 1.291485548*bb
	L, M, S := l_*l_*l_, m_*m_*m_, s_*s_*s_
	lin := [3]float64{
		4.0767416621*L - 3.3077115913*M + 0.2309699292*S,
		-1.2684380046*L + 2.6097574011*M - 0.3413193965*S,
		-0.0041960863*L - 0.7034186147*M + 1.707614701*S,
	}
	var out [3]float64
	for i, v := range lin {
		x := clamp01(v)
		if x <= 0.0031308 {
			out[i] = 12.92 * x
		} else {
			out[i] = 1.055*math.Pow(x, 1/2.4) - 0.055
		}
	}
	return out[0], out[1], out[2]
}

func linearize(v float64) float64 {
	if v <= 0.04045 {
		return v / 12.92
	}
	return math.Pow((v+0.055)/1.055, 2.4)
}

// RGBToOklch converts gamma sRGB to OKLCH (l, c, h in degrees 0..360).
func RGBToOklch(r, g, b float64) (l, c, h float64) {
	lr, lg, lb := linearize(r), linearize(g), linearize(b)
	l1 := math.Cbrt(0.4122214708*lr + 0.5363325363*lg + 0.0514459929*lb)
	m1 := math.Cbrt(0.2119034982*lr + 0.6806995451*lg + 0.1073969566*lb)
	s1 := math.Cbrt(0.0883024619*lr + 0.2817188376*lg + 0.6299787005*lb)
	L := 0.2104542553*l1 + 0.793617785*m1 - 0.0040720468*s1
	A := 1.9779984951*l1 - 2.428592205*m1 + 0.4505937099*s1
	B := 0.0259040371*l1 + 0.7827717662*m1 - 0.808675766*s1
	C := math.Sqrt(A*A + B*B)
	H := math.Atan2(B, A) * 180 / math.Pi
	if H < 0 {
		H += 360
	}
	return L, C, H
}

func hslToRGB(h, s, l float64) (float64, float64, float64) {
	h = math.Mod(math.Mod(h, 360)+360, 360) / 360
	s, l = clamp01(s), clamp01(l)
	q := l + s - l*s
	if l < 0.5 {
		q = l * (1 + s)
	}
	p := 2*l - q
	hue := func(t float64) float64 {
		t = math.Mod(t+1, 1)
		switch {
		case t < 1.0/6:
			return p + (q-p)*6*t
		case t < 0.5:
			return q
		case t < 2.0/3:
			return p + (q-p)*(2.0/3-t)*6
		}
		return p
	}
	return hue(h + 1.0/3), hue(h), hue(h - 1.0/3)
}

// ParseColor reads #rgb, #rgba, #rrggbb, #rrggbbaa, rgb()/rgba(), hsl()/hsla(),
// oklch(), and the keywords white, black and transparent. Anything else
// (var(), color-mix(), gradients, named colors) yields ErrInvalidColor.
func ParseColor(input string) (RGBA, error) {
	bad := fmt.Errorf("%w: %q", ErrInvalidColor, input)
	s := strings.ToLower(strings.TrimSpace(input))
	switch s {
	case "transparent":
		return RGBA{0, 0, 0, 0}, nil
	case "white":
		return RGBA{1, 1, 1, 1}, nil
	case "black":
		return RGBA{0, 0, 0, 1}, nil
	}
	if strings.HasPrefix(s, "#") {
		hex := s[1:]
		if len(hex) == 3 || len(hex) == 4 {
			var sb strings.Builder
			for _, ch := range hex {
				sb.WriteRune(ch)
				sb.WriteRune(ch)
			}
			hex = sb.String()
		}
		if len(hex) != 6 && len(hex) != 8 {
			return RGBA{}, bad
		}
		var v [4]float64
		v[3] = 1
		for i := 0; i < len(hex)/2; i++ {
			n, err := strconv.ParseUint(hex[i*2:i*2+2], 16, 8)
			if err != nil {
				return RGBA{}, bad
			}
			v[i] = float64(n) / 255
		}
		return RGBA{v[0], v[1], v[2], v[3]}, nil
	}
	open := strings.IndexByte(s, '(')
	if open < 0 || !strings.HasSuffix(s, ")") {
		return RGBA{}, bad
	}
	name, inner := s[:open], s[open+1:len(s)-1]
	if name != "oklch" && name != "rgb" && name != "rgba" && name != "hsl" && name != "hsla" {
		return RGBA{}, bad
	}
	body, alphaPart, hasSlash := strings.Cut(inner, "/")
	parts := strings.Fields(strings.ReplaceAll(body, ",", " "))
	if len(parts) < 3 {
		return RGBA{}, bad
	}
	alpha := 1.0
	var err error
	switch {
	case hasSlash:
		alpha, err = parseNum(alphaPart, 1)
	case len(parts) == 4 && name != "oklch":
		alpha, err = parseNum(parts[3], 1)
	}
	if err != nil || math.IsNaN(alpha) {
		return RGBA{}, bad
	}
	alpha = clamp01(alpha)
	var x, y, z float64
	switch name {
	case "oklch":
		l, e1 := parseNum(parts[0], 1)
		c, e2 := parseNum(parts[1], 0.4)
		h, e3 := parseAngle(parts[2])
		if e1 != nil || e2 != nil || e3 != nil {
			return RGBA{}, bad
		}
		x, y, z = OklchToRGB(l, c, h)
	case "hsl", "hsla":
		h, e1 := parseAngle(parts[0])
		sat, e2 := parseNum(parts[1], 1)
		lig, e3 := parseNum(parts[2], 1)
		if e1 != nil || e2 != nil || e3 != nil {
			return RGBA{}, bad
		}
		x, y, z = hslToRGB(h, sat, lig)
	default:
		ch := func(t string) (float64, error) {
			if strings.HasSuffix(t, "%") {
				v, e := strconv.ParseFloat(strings.TrimSuffix(t, "%"), 64)
				return clamp01(v / 100), e
			}
			v, e := strconv.ParseFloat(t, 64)
			return clamp01(v / 255), e
		}
		var e1, e2, e3 error
		x, e1 = ch(parts[0])
		y, e2 = ch(parts[1])
		z, e3 = ch(parts[2])
		if e1 != nil || e2 != nil || e3 != nil {
			return RGBA{}, bad
		}
	}
	for _, v := range []float64{x, y, z} {
		if math.IsNaN(v) {
			return RGBA{}, bad
		}
	}
	return RGBA{x, y, z, alpha}, nil
}

// Hex returns #rrggbb (alpha dropped).
func (c RGBA) Hex() string {
	h := func(v float64) int { return int(math.Round(clamp01(v) * 255)) }
	return fmt.Sprintf("#%02x%02x%02x", h(c.R), h(c.G), h(c.B))
}

// Composite paints top over bottom (source-over, in gamma sRGB).
func Composite(top, bottom RGBA) RGBA {
	a := top.A + bottom.A*(1-top.A)
	if a == 0 {
		return RGBA{}
	}
	mix := func(x, y float64) float64 { return (x*top.A + y*bottom.A*(1-top.A)) / a }
	return RGBA{mix(top.R, bottom.R), mix(top.G, bottom.G), mix(top.B, bottom.B), a}
}

// RelativeLuminance is the WCAG relative luminance (0..1) of c; alpha is ignored.
func RelativeLuminance(c RGBA) float64 {
	return 0.2126*linearize(c.R) + 0.7152*linearize(c.G) + 0.0722*linearize(c.B)
}

// ContrastRatio is the WCAG contrast ratio (1..21). A translucent fg is
// composited over bg; a translucent bg is flattened over white first.
func ContrastRatio(fg, bg RGBA) float64 {
	bg = opaque(bg)
	l1 := RelativeLuminance(Composite(fg, bg))
	l2 := RelativeLuminance(bg)
	return (math.Max(l1, l2) + 0.05) / (math.Min(l1, l2) + 0.05)
}

func opaque(c RGBA) RGBA {
	if c.A >= 1 {
		return c
	}
	return Composite(c, RGBA{1, 1, 1, 1})
}

// Contrast parses both colors and returns their ContrastRatio.
func Contrast(fg, bg string) (float64, error) {
	f, err := ParseColor(fg)
	if err != nil {
		return 0, err
	}
	b, err := ParseColor(bg)
	if err != nil {
		return 0, err
	}
	return ContrastRatio(f, b), nil
}

// OnOptions configures OnColor. Zero values select the defaults.
type OnOptions struct {
	Light string  // candidate for dark backgrounds; default "#ffffff"
	Dark  string  // candidate for light backgrounds; default "#000000"
	Min   float64 // target ratio; default MinText (4.5)
}

// OnColor returns the readable text color for bg: the Light or Dark candidate
// with the higher contrast (Light wins ties). If even the best candidate is
// below Min, its OKLCH lightness is adjusted with EnsureContrast (the result
// is then an oklch() string). With the default white/black candidates the
// result is always >= 4.58:1 on any opaque background.
func OnColor(bg string, opts OnOptions) (string, error) {
	if opts.Light == "" {
		opts.Light = "#ffffff"
	}
	if opts.Dark == "" {
		opts.Dark = "#000000"
	}
	if opts.Min == 0 {
		opts.Min = MinText
	}
	b, err := ParseColor(bg)
	if err != nil {
		return "", err
	}
	light, err := ParseColor(opts.Light)
	if err != nil {
		return "", err
	}
	dark, err := ParseColor(opts.Dark)
	if err != nil {
		return "", err
	}
	best, bestRatio := opts.Light, ContrastRatio(light, b)
	if r := ContrastRatio(dark, b); r > bestRatio {
		best, bestRatio = opts.Dark, r
	}
	if bestRatio >= opts.Min {
		return best, nil
	}
	return EnsureContrast(best, bg, opts.Min)
}

// EnsureContrast moves the OKLCH lightness of color (keeping hue and chroma,
// 0.01 per step, up to 100 steps) away from bg until it reaches min (0 means
// MinText). It returns an oklch() string "oklch(L C H)" with 4/4/2 decimals;
// when min is unreachable it returns the closest extreme (L 0 or 1). A
// foreground that already passes is returned re-encoded as oklch().
func EnsureContrast(color, bg string, min float64) (string, error) {
	if min == 0 {
		min = MinText
	}
	c, err := ParseColor(color)
	if err != nil {
		return "", err
	}
	b, err := ParseColor(bg)
	if err != nil {
		return "", err
	}
	l0, ch, hue := RGBToOklch(c.R, c.G, c.B)
	fmtL := func(l float64) string { return fmt.Sprintf("oklch(%.4f %.4f %.2f)", l, ch, hue) }
	ratio := func(l float64) float64 {
		p, _ := ParseColor(fmtL(l)) // always valid: built above
		return ContrastRatio(p, b)
	}
	if ratio(l0) >= min {
		return fmtL(l0), nil
	}
	lighten := RelativeLuminance(opaque(b)) < 0.18
	for i := 1; i <= 100; i++ {
		var l float64
		if lighten {
			l = math.Min(1, l0+float64(i)*0.01)
		} else {
			l = math.Max(0, l0-float64(i)*0.01)
		}
		if ratio(l) >= min || l == 0 || l == 1 {
			return fmtL(l), nil
		}
	}
	if lighten {
		return fmtL(1), nil
	}
	return fmtL(0), nil
}

// PickBacking returns the candidate background that contrasts most with fg
// (first wins ties) and that ratio. Candidates that do not parse are skipped;
// if none parses (or the list is empty) it returns ErrInvalidColor. Typical
// use: a white logo and the brand palette as candidates.
func PickBacking(fg string, candidates []string) (string, float64, error) {
	f, err := ParseColor(fg)
	if err != nil {
		return "", 0, err
	}
	best, bestRatio := "", -1.0
	for _, cand := range candidates {
		b, err := ParseColor(cand)
		if err != nil {
			continue
		}
		if r := ContrastRatio(f, b); r > bestRatio {
			best, bestRatio = cand, r
		}
	}
	if best == "" {
		return "", 0, fmt.Errorf("%w: no usable backing candidate", ErrInvalidColor)
	}
	return best, bestRatio, nil
}
