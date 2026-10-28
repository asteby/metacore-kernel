package brand

import (
	"bytes"
	"image"
	"image/png"
	"testing/fstest"
)

const svgNS = `xmlns="http://www.w3.org/2000/svg"`

func pngBytes(w, h int) []byte {
	var b bytes.Buffer
	_ = png.Encode(&b, image.NewRGBA(image.Rect(0, 0, w, h)))
	return b.Bytes()
}

func svg(viewBox string) []byte {
	return []byte(`<svg ` + svgNS + ` viewBox="` + viewBox + `"><path d="M0 0L1 1" fill="#00fcff"/></svg>`)
}

// vantoraPack is a vantora-like brand pack: heavy-ish static SVGs, a PNG
// favicon/maskable and a SMIL-animated mark (no script).
func vantoraPack() fstest.MapFS {
	return fstest.MapFS{
		"brand.json": {Data: []byte(`{
  "key":"vantora","name":"Vantora","short_name":"Vantora","description":"Plataforma de inteligencia",
  "names":{"en":"Vantora","es-MX":"Vantora"},
  "theme":{"primary":"#0033F0","accent":"#00FCFF","gradient":["#00fcff","#0033f0"],
           "background":"#fff","foreground":"#020846",
           "dark":{"background":"#020846","foreground":"#ffffff","primary":"#00fcff"},
           "theme_color":"#020846","background_color":"#020846"},
  "assistant":{"name":"Vanta","mascot":{"id":"vantora","tooth_shape":"hex-frame","antenna":false,"screen_tone":"dark",
     "colors":{"tire":"#051C35","tread_or_tooth":"#102E6B","accent":"#2160DE","glow":"#628FEA","screen":"#041324","screen_center":"#071A36"},
     "face":{"stroke":"#f4f8ff","stroke_width":6.5}}},
  "storage_namespace":"vantora",
  "assets":{"icon":"vantora-isotipo.svg","logo":"vantora-logo-fondo-claro.svg","logo_dark":"vantora-logo-fondo-oscuro.svg",
            "favicon":"favicon.png","maskable":"assets/maskable-512.png","animated":"animated.svg"}
}`)},
		"vantora-isotipo.svg":           {Data: svg("0 0 512 512")},
		"vantora-logo-fondo-claro.svg":  {Data: svg("0 0 1300 400")},
		"vantora-logo-fondo-oscuro.svg": {Data: svg("0 0 1300 400")},
		"favicon.png":                   {Data: pngBytes(48, 48)},
		"assets/maskable-512.png":       {Data: pngBytes(512, 512)},
		"animated.svg":                  {Data: []byte(`<svg ` + svgNS + ` width="200px" height="100"><rect width="10" height="10"><animate attributeName="opacity" from="0" to="1" dur="1s" repeatCount="indefinite"/></rect></svg>`)},
	}
}
