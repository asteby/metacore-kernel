package svgsafe

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

const ns = `xmlns="http://www.w3.org/2000/svg"`

func TestValidateAccepts(t *testing.T) {
	for name, svg := range map[string]string{
		"plain":            `<svg ` + ns + `><path d="M0 0" fill="#f00"/></svg>`,
		"xml prolog":       `<?xml version="1.0" encoding="UTF-8"?><svg ` + ns + `><g/></svg>`,
		"animateTransform": `<svg ` + ns + `><g><animateTransform attributeName="transform" type="rotate" from="0" to="360" dur="2s" repeatCount="indefinite"/></g></svg>`,
		"keyframes":        `<svg ` + ns + `><style>@keyframes a{from{opacity:0}to{opacity:1}} .x{animation:a 2s infinite}</style><rect class="x" width="1" height="1"/></svg>`,
		"local href":       `<svg ` + ns + `><defs><path id="p" d="M0 0"/></defs><use href="#p"/></svg>`,
	} {
		if err := Validate([]byte(svg)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// vantoraLike mimics a heavy brand logo: gradients, 132 stops, clipPath.
func vantoraLike() []byte {
	var b strings.Builder
	b.WriteString(`<svg ` + ns + ` viewBox="0 0 512 512"><defs><clipPath id="c"><rect width="512" height="512"/></clipPath>`)
	b.WriteString(`<radialGradient id="r" cx="50%" cy="50%" r="50%">`)
	for i := 0; i < 66; i++ {
		fmt.Fprintf(&b, `<stop offset="%d%%" stop-color="#00fcff" stop-opacity="0.%02d"/>`, i, i%100)
	}
	b.WriteString(`</radialGradient><linearGradient id="l" x1="0" y1="0" x2="1" y2="1">`)
	for i := 0; i < 66; i++ {
		fmt.Fprintf(&b, `<stop offset="%d%%" stop-color="#0033f0"/>`, i)
	}
	b.WriteString(`</linearGradient></defs><g clip-path="url(#c)"><path d="M0 0L512 512" fill="url(#r)" stroke="url(#l)"/></g></svg>`)
	return []byte(b.String())
}

func TestValidateHeavyStaticLogo(t *testing.T) {
	svg := vantoraLike()
	if got := strings.Count(string(svg), "<stop"); got != 132 {
		t.Fatalf("fixture has %d stops", got)
	}
	if err := Validate(svg); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRejects(t *testing.T) {
	for name, svg := range map[string]string{
		"script":             `<svg ` + ns + `><script>alert(1)</script></svg>`,
		"foreignObject":      `<svg ` + ns + `><foreignObject><div/></foreignObject></svg>`,
		"external href":      `<svg ` + ns + `><use href="https://evil/x.svg#a"/></svg>`,
		"js href":            `<svg ` + ns + `><use href="javascript:alert(1)"/></svg>`,
		"onload":             `<svg ` + ns + ` onload="alert(1)"/>`,
		"onclick":            `<svg ` + ns + `><rect onclick="x()"/></svg>`,
		"entity":             `<!DOCTYPE svg [<!ENTITY x "y">]><svg ` + ns + `/>`,
		"style import":       `<svg ` + ns + `><style>@import url(https://evil/x.css);</style></svg>`,
		"style url ext":      `<svg ` + ns + `><rect style="fill:url(https://evil/x)"/></svg>`,
		"not svg root":       `<html/>`,
		"empty":              ``,
		"animate":            `<svg ` + ns + `><rect><animate attributeName="opacity" from="0" to="1" dur="1s"/></rect></svg>`,
		"set":                `<svg ` + ns + `><rect><set attributeName="opacity" to="1"/></rect></svg>`,
		"animTransform href": `<svg ` + ns + `><g><animateTransform attributeName="href" to="x"/></g></svg>`,
	} {
		err := Validate([]byte(svg))
		if err == nil {
			t.Errorf("%s: accepted", name)
		} else if !errors.Is(err, ErrNotAllowed) {
			t.Errorf("%s: error does not wrap ErrNotAllowed: %v", name, err)
		}
	}
}

func TestAllowAnimation(t *testing.T) {
	ok := `<svg ` + ns + `><rect><animate attributeName="opacity" from="0" to="1" dur="1s" repeatCount="indefinite"/><set attributeName="stop-color" to="#fff"/></rect></svg>`
	if err := Validate([]byte(ok), AllowAnimation()); err != nil {
		t.Fatalf("SMIL opacity with AllowAnimation: %v", err)
	}
	for name, bad := range map[string]string{
		"animate href":    `<svg ` + ns + `><use href="#a"><animate attributeName="href" to="https://evil/x"/></use></svg>`,
		"animate xlink":   `<svg ` + ns + `><use><animate attributeName="xlink:href" to="x"/></use></svg>`,
		"animate onbegin": `<svg ` + ns + `><rect><animate attributeName="opacity" onbegin="x()"/></rect></svg>`,
		"script":          `<svg ` + ns + `><script>1</script></svg>`,
		"animate js val":  `<svg ` + ns + `><rect><animate attributeName="opacity" to="javascript:x()"/></rect></svg>`,
	} {
		if Validate([]byte(bad), AllowAnimation()) == nil {
			t.Errorf("%s: accepted with AllowAnimation", name)
		}
	}
}

func TestMaxBytes(t *testing.T) {
	svg := []byte(`<svg ` + ns + `><g/></svg>`)
	if err := Validate(svg, MaxBytes(len(svg))); err != nil {
		t.Fatal(err)
	}
	if err := Validate(svg, MaxBytes(len(svg)-1)); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("got %v", err)
	}
}

func FuzzValidate(f *testing.F) {
	f.Add([]byte(`<svg ` + ns + `><path d="M0 0"/></svg>`))
	f.Add([]byte(`<svg ` + ns + `><script>x</script></svg>`))
	f.Add(vantoraLike())
	f.Fuzz(func(t *testing.T, b []byte) {
		if Validate(b) == nil {
			// anything accepted must be script-free
			l := strings.ToLower(string(b))
			if strings.Contains(l, "<script") || strings.Contains(l, "<foreignobject") {
				t.Fatalf("accepted dangerous doc: %q", b)
			}
		}
		_ = Validate(b, AllowAnimation(), MaxBytes(1<<20))
	})
}
