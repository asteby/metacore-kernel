package brand

import (
	"bytes"
	"encoding/xml"
	"errors"
	"image/png"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Asset validation shared by the brand pack and any host that accepts
// uploaded artwork. Same rules as the hub (BRAND-PACK-CONTRACT §1): type by
// magic bytes, PNG must decode and end in IEND, SVG is walked token by token
// and rejected on anything that can run script or load external content.

var pngMagic = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}

var pngIEND = []byte{0, 0, 0, 0, 'I', 'E', 'N', 'D', 0xae, 0x42, 0x60, 0x82}

// SniffType returns TypePNG or TypeSVG from the content, or "" when neither.
func SniffType(data []byte) string {
	if bytes.HasPrefix(data, pngMagic) {
		return TypePNG
	}
	head := bytes.TrimLeft(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), " \t\r\n")
	if bytes.HasPrefix(head, []byte("<?xml")) || bytes.HasPrefix(head, []byte("<svg")) || bytes.HasPrefix(head, []byte("<!--")) {
		return TypeSVG
	}
	return ""
}

// ValidateAsset checks data is a safe PNG or SVG and, when declared is not
// empty, that it matches the sniffed type. Returns the sniffed type.
func ValidateAsset(declared string, data []byte) (string, error) {
	typ := SniffType(data)
	if typ == "" {
		return "", errors.New("only SVG or PNG are accepted")
	}
	if d := strings.ToLower(strings.TrimSpace(declared)); d != "" && d != typ {
		return "", errors.New("declared type " + d + " does not match content (" + typ + ")")
	}
	var err error
	switch typ {
	case TypePNG:
		err = ValidatePNG(data)
	case TypeSVG:
		err = ValidateSVG(data)
	}
	if err != nil {
		return "", err
	}
	return typ, nil
}

// ValidatePNG requires a decodable header, 1..MaxImageDim px per side and
// the file to end with the IEND chunk (nothing appended after it).
func ValidatePNG(data []byte) error {
	cfg, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return errors.New("invalid PNG: " + err.Error())
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > MaxImageDim || cfg.Height > MaxImageDim {
		return errors.New("PNG dimensions out of range (1.." + strconv.Itoa(MaxImageDim) + ")")
	}
	if !bytes.HasSuffix(data, pngIEND) {
		return errors.New("PNG must end with the IEND chunk")
	}
	return nil
}

// svgDeniedElements run code, embed HTML or load external documents.
var svgDeniedElements = map[string]bool{
	"script": true, "foreignobject": true, "iframe": true, "embed": true,
	"object": true, "audio": true, "video": true, "handler": true, "listener": true,
}

// ValidateSVG walks the XML token by token. It rejects DOCTYPE/ENTITY,
// processing instructions other than <?xml?>, denied elements (with or
// without prefix), on* attributes, href/src that are not #fragments,
// javascript:/vbscript: anywhere in a value, external url()/@import in
// styles, and <set>/<animate> targeting href or on*. Root must be <svg>.
func ValidateSVG(data []byte) error {
	if !utf8.Valid(data) {
		return errors.New("SVG must be UTF-8")
	}
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = true
	dec.Entity = map[string]string{}
	var (
		depth    int
		rootSeen bool
		inStyle  int
	)
	for {
		tok, err := dec.RawToken()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return errors.New("invalid SVG XML: " + err.Error())
		}
		switch t := tok.(type) {
		case xml.Directive:
			return errors.New("SVG must not contain DOCTYPE or ENTITY declarations")
		case xml.ProcInst:
			if !strings.EqualFold(t.Target, "xml") {
				return errors.New("SVG must not contain processing instructions")
			}
		case xml.StartElement:
			name := strings.ToLower(t.Name.Local)
			if depth == 0 {
				if rootSeen {
					return errors.New("SVG must have a single root element")
				}
				if name != "svg" {
					return errors.New("root element must be <svg>")
				}
				rootSeen = true
			}
			if svgDeniedElements[name] {
				return errors.New("SVG must not contain <" + t.Name.Local + ">")
			}
			if name == "style" {
				inStyle++
			}
			for _, a := range t.Attr {
				if err := checkSVGAttr(a); err != nil {
					return err
				}
			}
			depth++
		case xml.EndElement:
			depth--
			if strings.EqualFold(t.Name.Local, "style") && inStyle > 0 {
				inStyle--
			}
		case xml.CharData:
			if inStyle > 0 {
				if err := checkSVGStyle(string(t)); err != nil {
					return err
				}
			} else if depth == 0 && len(bytes.TrimSpace(t)) > 0 {
				return errors.New("SVG must not contain text outside the root element")
			}
		}
	}
	if !rootSeen {
		return errors.New("SVG has no <svg> root")
	}
	if depth != 0 {
		return errors.New("SVG is not well-formed")
	}
	return nil
}

func checkSVGAttr(a xml.Attr) error {
	local := strings.ToLower(a.Name.Local)
	val := a.Value
	compact := strings.ToLower(strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return -1
		}
		return r
	}, val))
	if strings.HasPrefix(local, "on") {
		return errors.New("SVG must not contain event handler attributes (" + a.Name.Local + ")")
	}
	if strings.Contains(compact, "javascript:") || strings.Contains(compact, "vbscript:") {
		return errors.New("SVG must not contain javascript: URLs")
	}
	if local == "href" || local == "src" || strings.HasSuffix(local, ":href") {
		if !strings.HasPrefix(strings.TrimSpace(val), "#") {
			return errors.New("SVG links must be internal fragments (#id)")
		}
	}
	if local == "attributename" {
		target := strings.ToLower(strings.TrimSpace(val))
		if strings.HasSuffix(target, "href") || strings.HasPrefix(target, "on") {
			return errors.New("SVG animation must not target " + val)
		}
	}
	if local == "style" || strings.Contains(compact, "url(") {
		return checkSVGStyle(val)
	}
	return nil
}

var svgURLRe = regexp.MustCompile(`(?i)url\(\s*(['"]?)\s*([^)'"]*)`)

func checkSVGStyle(css string) error {
	low := strings.ToLower(css)
	if strings.Contains(low, "@import") {
		return errors.New("SVG styles must not use @import")
	}
	if strings.Contains(strings.ReplaceAll(low, " ", ""), "javascript:") || strings.Contains(low, "expression(") {
		return errors.New("SVG styles must not contain script")
	}
	for _, m := range svgURLRe.FindAllStringSubmatch(css, -1) {
		if !strings.HasPrefix(strings.TrimSpace(m[2]), "#") {
			return errors.New("SVG styles must only reference internal fragments (url(#id))")
		}
	}
	return nil
}
