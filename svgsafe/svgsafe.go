// Package svgsafe is the single SVG validator of the kernel: a strict
// allowlist for SVG assets that a host serves from its own origin (preset
// bundle assets, product brand assets). It lives in its own leaf package so
// bundle, preset and brand share one implementation instead of copies.
package svgsafe

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// Validate guards SVG brand assets (assets/**.svg) of a preset bundle or a
// product brand pack: the host serves them from its own origin, so bundle.Read validates them against a
// strict allowlist (ported from ops, asteby-hq/ops#1977). The file is not
// rewritten: anything outside the allowlist rejects the whole asset. Hosts
// must ALSO serve preset assets with "Content-Security-Policy: sandbox" and
// "X-Content-Type-Options: nosniff" — the allowlist is the first layer, not
// the only one.

var svgAllowedElements = map[string]bool{
	"svg": true, "g": true, "defs": true, "symbol": true, "use": true, "title": true, "desc": true, "metadata": true,
	"path": true, "rect": true, "circle": true, "ellipse": true, "line": true, "polyline": true, "polygon": true,
	"text": true, "tspan": true, "textpath": true,
	"lineargradient": true, "radialgradient": true, "stop": true, "pattern": true,
	"clippath": true, "mask": true, "marker": true,
	"filter": true, "fegaussianblur": true, "feoffset": true, "feblend": true, "fecolormatrix": true,
	"fecomposite": true, "feflood": true, "femerge": true, "femergenode": true, "fedropshadow": true,
	"animatetransform": true,                    // can only animate "transform" (checked below)
	"style":            true, "namedview": true, // sodipodi:namedview from Inkscape
}

// Attributes are allowed by lowercase local name. href/xlink:href are further
// restricted to in-document "#id" references; style values are scanned.
var svgAllowedAttrs = toSet(`
id class style xmlns version viewbox width height x y x1 y1 x2 y2 cx cy r rx ry d points transform
fill fill-opacity fill-rule stroke stroke-width stroke-opacity stroke-linecap stroke-linejoin stroke-miterlimit
stroke-dasharray stroke-dashoffset opacity color display visibility clip-path clip-rule mask filter
font-family font-size font-weight font-style text-anchor dominant-baseline letter-spacing
offset stop-color stop-opacity gradientunits gradienttransform spreadmethod fx fy fr
patternunits patterntransform patterncontentunits clippathunits maskunits maskcontentunits markerwidth markerheight
markerunits refx refy orient preserveaspectratio xml:space space role aria-label aria-hidden focusable
dx dy rotate textlength lengthadjust startoffset href
attributename from to dur repeatcount begin end calcmode keytimes keysplines additive accumulate
stddeviation in in2 result mode type values flood-color flood-opacity operator k1 k2 k3 k4
x-height enable-background overflow vector-effect paint-order mix-blend-mode isolation shape-rendering
`)

func toSet(s string) map[string]bool {
	m := map[string]bool{}
	for _, f := range strings.Fields(s) {
		m[f] = true
	}
	return m
}

var svgTransformTargets = map[string]bool{"transform": true, "gradienttransform": true, "patterntransform": true}

// Namespaces whose attributes are editor metadata and harmless (Inkscape etc.).
var svgIgnoredAttrSpaces = map[string]bool{
	"http://www.inkscape.org/namespaces/inkscape":        true,
	"http://sodipodi.sourceforge.net/DTD/sodipodi-0.dtd": true,
	"http://www.w3.org/2000/xmlns/":                      true,
	"xmlns":                                              true,
	"http://www.w3.org/XML/1998/namespace":               true,
}

var (
	svgDangerousStyle = regexp.MustCompile(`(?i)(@import|expression\s*\(|javascript:|behaviou?r\s*:|-moz-binding|url\s*\(\s*['"]?\s*[^#'"\s)])`)
	// ErrNotAllowed wraps every rejection of Validate (errors.Is).
	ErrNotAllowed = errors.New("svg not allowed")
	errSVG        = ErrNotAllowed
)

// Option tunes Validate.
type Option func(*config)

type config struct {
	animation bool
	maxBytes  int
}

// AllowAnimation additionally accepts declarative (SMIL) animation: <animate>
// and <set> whose attributeName is one of a short allowlist of presentation
// attributes (opacity, stop-color, offset, stroke-dashoffset, transform...).
// Scripts, foreignObject and external references stay rejected, and
// <animate>/<set> can never target href. CSS @keyframes inside <style> are
// always accepted (url() to external resources is not).
func AllowAnimation() Option { return func(c *config) { c.animation = true } }

// MaxBytes rejects documents larger than n bytes (n <= 0 = no limit).
func MaxBytes(n int) Option { return func(c *config) { c.maxBytes = n } }

// animTargets are the attributes <animate>/<set> may drive under AllowAnimation.
var animTargets = toSet(`opacity fill-opacity stroke-opacity stop-color stop-opacity offset
stroke-dashoffset stroke-dasharray transform gradienttransform patterntransform`)

// Validate reports why an SVG is not safe to serve, or nil when it passes.
func Validate(data []byte, opts ...Option) error {
	var cfg config
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.maxBytes > 0 && len(data) > cfg.maxBytes {
		return fmt.Errorf("%w: %d bytes exceeds the %d byte limit", errSVG, len(data), cfg.maxBytes)
	}
	if bytes.Contains(bytes.ToLower(data), []byte("<!entity")) || bytes.Contains(bytes.ToLower(data), []byte("<!doctype")) && bytes.Contains(bytes.ToLower(data), []byte("[")) {
		return fmt.Errorf("%w: DTD/entity declarations", errSVG)
	}
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = true
	depth, sawRoot := 0, false
	var inStyle bool
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("%w: %v", errSVG, err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			name := strings.ToLower(t.Name.Local)
			if !sawRoot {
				if name != "svg" {
					return fmt.Errorf("%w: root is <%s>", errSVG, t.Name.Local)
				}
				sawRoot = true
			}
			isAnim := name == "animate" || name == "set"
			if !svgAllowedElements[name] && !(isAnim && cfg.animation) {
				return fmt.Errorf("%w: element <%s>", errSVG, t.Name.Local)
			}
			inStyle = name == "style"
			for _, a := range t.Attr {
				if err := checkSVGAttr(a); err != nil {
					return err
				}
				if name == "animatetransform" && strings.EqualFold(a.Name.Local, "attributeName") && !svgTransformTargets[strings.ToLower(strings.TrimSpace(a.Value))] {
					return fmt.Errorf("%w: animateTransform targeting %q", errSVG, a.Value)
				}
				if isAnim && strings.EqualFold(a.Name.Local, "attributeName") && !animTargets[strings.ToLower(strings.TrimSpace(a.Value))] {
					return fmt.Errorf("%w: <%s> targeting %q", errSVG, name, a.Value)
				}
			}
			depth++
		case xml.EndElement:
			depth--
			inStyle = false
		case xml.CharData:
			if inStyle && svgDangerousStyle.Match(t) {
				return fmt.Errorf("%w: unsafe <style> content", errSVG)
			}
		case xml.ProcInst:
			if !strings.EqualFold(t.Target, "xml") {
				return fmt.Errorf("%w: processing instruction <?%s?>", errSVG, t.Target)
			}
		case xml.Directive:
			return fmt.Errorf("%w: directive", errSVG)
		}
	}
	if !sawRoot {
		return fmt.Errorf("%w: empty", errSVG)
	}
	return nil
}

func checkSVGAttr(a xml.Attr) error {
	local := strings.ToLower(a.Name.Local)
	if strings.HasPrefix(local, "on") {
		return fmt.Errorf("%w: event handler %q", errSVG, a.Name.Local)
	}
	if svgIgnoredAttrSpaces[a.Name.Space] || a.Name.Space == "xmlns" || local == "xmlns" {
		return nil
	}
	if strings.HasPrefix(local, "data-") {
		return nil
	}
	val := strings.TrimSpace(a.Value)
	switch local {
	case "href":
		// href / xlink:href: only references into the same document.
		if !strings.HasPrefix(val, "#") {
			return fmt.Errorf("%w: external or scripted href %q", errSVG, svgTrunc(val))
		}
		return nil
	case "style":
		if svgDangerousStyle.MatchString(val) {
			return fmt.Errorf("%w: unsafe style attribute", errSVG)
		}
		return nil
	}
	if !svgAllowedAttrs[local] {
		return fmt.Errorf("%w: attribute %q", errSVG, a.Name.Local)
	}
	lv := strings.ToLower(val)
	if strings.Contains(lv, "javascript:") || strings.Contains(lv, "data:") || (strings.Contains(lv, "url(") && svgDangerousStyle.MatchString(val)) {
		return fmt.Errorf("%w: unsafe value in %q", errSVG, a.Name.Local)
	}
	return nil
}

func svgTrunc(s string) string {
	if len(s) > 40 {
		return s[:40] + "…"
	}
	return s
}
