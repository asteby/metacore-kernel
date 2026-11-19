// Package colorx is the server-side mirror of the SDK contrast primitive
// (@asteby/metacore-theme/contrast): WCAG 2.x contrast math, readable-text
// selection and logo-tone sampling with no dependencies beyond the standard
// library.
//
// Use it where the decision must be made on the server (PDF, e-mail,
// thumbnails, hub projections such as brand_on_color / logo_tone computed once
// at logo upload) instead of re-sampling in every client.
//
// # Algorithm (identical to the SDK; see docs/colorx-contrast.md)
//
//   - Colors are gamma-encoded sRGB, channels 0..1. A translucent foreground
//     is composited (source-over, in gamma space like browsers) over its
//     background. A translucent background is flattened over white first.
//   - RelativeLuminance: 0.2126 R + 0.7152 G + 0.0722 B on linearised channels.
//   - ContrastRatio: (Lhi+0.05)/(Llo+0.05), range 1..21.
//   - OnColor: the candidate (default white / black) with the higher ratio;
//     when it is still below Min (default 4.5, WCAG AA normal text) its OKLCH
//     lightness is moved until it reaches Min.
//   - EnsureContrast: moves OKLCH lightness in 0.01 steps (keeping hue and
//     chroma) away from the background; returns an oklch() string.
//   - PickBacking: the candidate background with the best ratio against a
//     foreground (a white logo gets the darkest backing).
//   - ImageTone: mean colour of opaque pixels (alpha > 40/255) of a PNG, JPEG
//     or GIF, its luminance and a Tone. WebP and SVG are not supported (no
//     decoder in the kernel's dependency set): ErrUnsupportedImage.
//
// The conformance vectors live in testdata/contrast-vectors.json and are
// shared byte for byte with the SDK.
package colorx
