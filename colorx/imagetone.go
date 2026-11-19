package colorx

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/draw"
	_ "image/gif"  // register decoder
	_ "image/jpeg" // register decoder
	_ "image/png"  // register decoder
	"io"
	"math"
)

// Tone classifies an image by its mean luminance.
type Tone string

const (
	ToneLight Tone = "light"
	ToneDark  Tone = "dark"
	ToneMid   Tone = "mid"
)

// Tone thresholds on mean relative luminance. A "light" image (white logo)
// needs a dark backing; a "dark" one a light backing; "mid" works on neither
// extreme and should be given a neutral backing.
const (
	LightMin = 0.6
	DarkMax  = 0.2
)

// Sampling limits. AlphaMin is the opacity threshold (alpha > 40/255).
const (
	AlphaMin      = 40
	MaxBytes      = 16 << 20 // bytes read from the reader
	MaxPixels     = 64 << 20 // w*h accepted before decoding (decompression bombs)
	MaxSampleSide = 256      // sample grid is at most ~256 px on its long side
)

var (
	// ErrUnsupportedImage: not PNG/JPEG/GIF (WebP and SVG have no decoder in
	// the kernel's dependency set), or corrupt/truncated data.
	ErrUnsupportedImage = errors.New("colorx: unsupported image format")
	// ErrImageTooLarge: bytes or dimensions exceed MaxBytes / MaxPixels.
	ErrImageTooLarge = errors.New("colorx: image too large")
	// ErrNoOpaquePixels: nothing to sample (fully transparent image).
	ErrNoOpaquePixels = errors.New("colorx: image has no opaque pixels")
)

// ToneResult is the outcome of ImageTone.
type ToneResult struct {
	Color     string  // mean color of the opaque pixels, #rrggbb
	Luminance float64 // relative luminance of Color, 6 decimals
	Tone      Tone
	Samples   int // opaque pixels averaged
}

// ClassifyLuminance maps a luminance to a Tone.
func ClassifyLuminance(l float64) Tone {
	switch {
	case l >= LightMin:
		return ToneLight
	case l <= DarkMax:
		return ToneDark
	}
	return ToneMid
}

// ImageTone decodes a PNG, JPEG or GIF and averages the gamma-sRGB channels
// of its opaque pixels (alpha > 40/255, un-premultiplied), sampling every Nth
// pixel so the grid is at most MaxSampleSide on its long side. It returns the
// mean color, its relative luminance and a Tone. Fully transparent images
// return ErrNoOpaquePixels; other formats, ErrUnsupportedImage.
func ImageTone(r io.Reader) (ToneResult, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxBytes+1))
	if err != nil {
		return ToneResult{}, err
	}
	if len(data) > MaxBytes {
		return ToneResult{}, ErrImageTooLarge
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return ToneResult{}, fmt.Errorf("%w: %v", ErrUnsupportedImage, err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > MaxPixels {
		return ToneResult{}, ErrImageTooLarge
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return ToneResult{}, fmt.Errorf("%w: %v", ErrUnsupportedImage, err)
	}
	b := img.Bounds()
	long := b.Dx()
	if b.Dy() > long {
		long = b.Dy()
	}
	step := (long + MaxSampleSide - 1) / MaxSampleSide
	if step < 1 {
		step = 1
	}
	// Normalise to NRGBA once so alpha handling is uniform (GIF palettes etc.).
	nr := image.NewNRGBA(b)
	draw.Draw(nr, b, img, b.Min, draw.Src)
	var sr, sg, sb float64
	n := 0
	for y := b.Min.Y; y < b.Max.Y; y += step {
		for x := b.Min.X; x < b.Max.X; x += step {
			p := nr.NRGBAAt(x, y)
			if p.A <= AlphaMin {
				continue
			}
			sr += float64(p.R)
			sg += float64(p.G)
			sb += float64(p.B)
			n++
		}
	}
	if n == 0 {
		return ToneResult{}, ErrNoOpaquePixels
	}
	f := float64(n) * 255
	mean := RGBA{sr / f, sg / f, sb / f, 1}
	lum := RelativeLuminance(mean)
	return ToneResult{Color: mean.Hex(), Luminance: math.Round(lum*1e6) / 1e6, Tone: ClassifyLuminance(lum), Samples: n}, nil
}
