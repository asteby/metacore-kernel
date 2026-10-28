package preset

import "github.com/asteby/metacore-kernel/svgsafe"

// ValidateSVG guards SVG brand assets (assets/**.svg) of a preset bundle. It
// delegates to svgsafe.Validate, the kernel's single SVG validator. Hosts must
// ALSO serve preset assets with "Content-Security-Policy: sandbox" and
// "X-Content-Type-Options: nosniff": the allowlist is the first layer, not the
// only one.
func ValidateSVG(data []byte) error { return svgsafe.Validate(data) }
