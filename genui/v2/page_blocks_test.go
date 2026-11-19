package genuiv2

import (
	"strings"
	"testing"
)

// Pruebas de los bloques de pagina (tabs extendido, section, grid.areas,
// kpi_card, map.select). Los vectores compartidos (testdata/conformance) fijan
// el contrato entre lenguajes; aqui se ejercitan los limites y la
// retrocompatibilidad con documentos construidos en codigo.

func pgDoc(data map[string]any, blocks ...any) map[string]any {
	d := map[string]any{"version": Version, "blocks": blocks}
	if data != nil {
		d["data"] = data
	}
	return d
}

func pgBlock(typ string, kv ...any) map[string]any {
	b := map[string]any{"type": typ}
	for i := 0; i+1 < len(kv); i += 2 {
		b[kv[i].(string)] = kv[i+1]
	}
	return b
}

func pgText(area string) map[string]any {
	b := pgBlock("text", "text", "x")
	if area != "" {
		b["area"] = area
	}
	return b
}

func TestPageBlocks(t *testing.T) {
	tab := func(kv ...any) map[string]any {
		o := map[string]any{"key": "a", "label": "A"}
		for i := 0; i+1 < len(kv); i += 2 {
			o[kv[i].(string)] = kv[i+1]
		}
		return o
	}
	src := map[string]any{"p": map[string]any{"q": "t:x"}}

	tests := []struct {
		name string
		doc  map[string]any
		want string // "" = valido; si no, codigo estable
	}{
		{"section clasica sigue valida", pgDoc(nil, pgBlock("section", "title", "T", "blocks", []any{pgText("")})), ""},
		{"section custom", pgDoc(nil, pgBlock("section", "mode", "custom", "component", "hub_fleet.detail@1", "props", map[string]any{"a": true})), ""},
		{"section custom sin component", pgDoc(nil, pgBlock("section", "mode", "custom")), CodeRequired},
		{"section component y blocks", pgDoc(nil, pgBlock("section", "mode", "custom", "component", "a.b@1", "blocks", []any{pgText("")})), CodeInvalidBlock},
		{"section component sin mode custom", pgDoc(nil, pgBlock("section", "mode", "hybrid", "component", "a.b@1")), CodeInvalidBlock},
		{"section sin contenido", pgDoc(nil, pgBlock("section", "title", "T")), CodeRequired},
		{"section component mal nombrado", pgDoc(nil, pgBlock("section", "mode", "custom", "component", "A.b@1")), CodePattern},
		{"section component sin mayor", pgDoc(nil, pgBlock("section", "mode", "custom", "component", "a.b")), CodePattern},
		{"section component con proto", pgDoc(nil, pgBlock("section", "mode", "custom", "component", "a.prototype@1")), CodeForbiddenKey},

		{"tabs clasico", pgDoc(nil, pgBlock("tabs", "tabs", []any{tab("blocks", []any{pgText("")})})), ""},
		{"tabs opcion component", pgDoc(nil, pgBlock("tabs", "variant", "segmented", "tabs", []any{tab("icon", "map", "component", "a.b@2", "props", map[string]any{"n": 1.0})})), ""},
		{"tabs opcion blocks y component", pgDoc(nil, pgBlock("tabs", "tabs", []any{tab("blocks", []any{pgText("")}, "component", "a.b@1")})), CodeInvalidBlock},
		{"tabs opcion vacia", pgDoc(nil, pgBlock("tabs", "tabs", []any{tab()})), CodeRequired},
		{"tabs icono fuera del conjunto", pgDoc(nil, pgBlock("tabs", "tabs", []any{tab("icon", "x-y", "blocks", []any{pgText("")})})), CodePattern},
		{"tabs icono por nombre", pgDoc(nil, pgBlock("tabs", "tabs", []any{tab("icon", "MapPin", "blocks", []any{pgText("")})})), ""},
		{"tabs icono nombre de 64", pgDoc(nil, pgBlock("tabs", "tabs", []any{tab("icon", "A"+strings.Repeat("b", 63), "blocks", []any{pgText("")})})), ""},
		{"tabs icono nombre de 65", pgDoc(nil, pgBlock("tabs", "tabs", []any{tab("icon", "A"+strings.Repeat("b", 64), "blocks", []any{pgText("")})})), CodePattern},
		{"tabs icono vacio", pgDoc(nil, pgBlock("tabs", "tabs", []any{tab("icon", "", "blocks", []any{pgText("")})})), CodePattern},
		{"tabs icono con espacio", pgDoc(nil, pgBlock("tabs", "tabs", []any{tab("icon", "Map Pin", "blocks", []any{pgText("")})})), CodePattern},
		{"tabs icono prototipo", pgDoc(nil, pgBlock("tabs", "tabs", []any{tab("icon", "constructor", "blocks", []any{pgText("")})})), CodeForbiddenKey},
		{"tabs icono no cadena", pgDoc(nil, pgBlock("tabs", "tabs", []any{tab("icon", 5.0, "blocks", []any{pgText("")})})), CodeType},
		{"tabs claves duplicadas con component", pgDoc(nil, pgBlock("tabs", "tabs", []any{tab("component", "a.b@1"), tab("component", "a.b@1")})), CodeDuplicateID},

		{"grid clasico", pgDoc(nil, pgBlock("grid", "columns", 3.0, "blocks", []any{pgText("")})), ""},
		{"grid areas", pgDoc(nil, pgBlock("grid", "areas", []any{"a a", "b ."}, "columns", []any{"1fr", "minmax(120px, 1fr)"}, "rows", []any{"auto", "240px"}, "mobile", []any{"b", "a"},
			"blocks", []any{pgText("a"), pgText("b")})), ""},
		{"grid areas no rectangular", pgDoc(nil, pgBlock("grid", "areas", []any{"a b", "a"}, "blocks", []any{pgText("a"), pgText("b")})), CodeInvalidBlock},
		{"grid hijo sin area", pgDoc(nil, pgBlock("grid", "areas", []any{"a b"}, "blocks", []any{pgText("a"), pgText("")})), CodeInvalidBlock},
		{"grid area inexistente", pgDoc(nil, pgBlock("grid", "areas", []any{"a b"}, "blocks", []any{pgText("a"), pgText("c")})), CodeUnknownTarget},
		{"grid mobile inexistente", pgDoc(nil, pgBlock("grid", "areas", []any{"a"}, "mobile", []any{"z"}, "blocks", []any{pgText("a")})), CodeUnknownTarget},
		{"grid pista libre", pgDoc(nil, pgBlock("grid", "areas", []any{"a"}, "columns", []any{"calc(1fr)"}, "blocks", []any{pgText("a")})), CodePattern},
		{"grid columns lista sin areas", pgDoc(nil, pgBlock("grid", "columns", []any{"1fr"}, "blocks", []any{pgText("")})), CodeInvalidBlock},

		{"kpi_card", pgDoc(map[string]any{"k": map[string]any{"widget": "a.dashboard.k"}},
			pgBlock("kpi_card", "label", "L", "value", map[string]any{"$ref": "data.k.value"}, "trend", map[string]any{"$ref": "data.k.trend"},
				"cta", map[string]any{"do": "navigate", "route": map[string]any{"name": "page", "key": "p"}})), ""},
		{"kpi_card sin value", pgDoc(nil, pgBlock("kpi_card", "label", "L")), CodeRequired},
		{"kpi_card campo ajeno", pgDoc(nil, pgBlock("kpi_card", "label", "L", "value", 1.0, "spark", []any{1.0})), CodeFieldNotAllowed},

		{"map select", pgDoc(src, pgBlock("map", "summary", "m", "source", "data.p", "select", "state.sel")), ""},
		{"map select fuera de state", pgDoc(src, pgBlock("map", "summary", "m", "source", "data.p", "select", "record.sel")), CodePattern},
		{"map popup, precision_field y area_field", pgDoc(src, pgBlock("map", "summary", "m", "source", "data.p", "popup", []any{"a", "b"}, "precision_field", "prec", "area_field", "iso")), ""},
		{"map image_field y color_field", pgDoc(src, pgBlock("map", "summary", "m", "source", "data.p", "image_field", "logo", "color_field", "brand", "subtitle_field", "host")), ""},
		{"map subtitle_field sin source", pgDoc(nil, pgBlock("map", "summary", "m", "points", []any{map[string]any{"key": "a", "lat": 1.0, "lng": 2.0, "label": "A", "status": "ok"}}, "subtitle_field", "host")), CodeInvalidBlock},
		{"map image_field sin source", pgDoc(nil, pgBlock("map", "summary", "m", "points", []any{map[string]any{"key": "a", "lat": 1.0, "lng": 2.0, "label": "A", "status": "ok"}}, "image_field", "logo")), CodeInvalidBlock},
		{"map color_field sin source", pgDoc(nil, pgBlock("map", "summary", "m", "points", []any{map[string]any{"key": "a", "lat": 1.0, "lng": 2.0, "label": "A", "status": "ok"}}, "color_field", "brand")), CodeInvalidBlock},
		{"map image_field no es una clave", pgDoc(src, pgBlock("map", "summary", "m", "source", "data.p", "image_field", "not a key!")), CodePattern},
		{"map precision constante", pgDoc(src, pgBlock("map", "summary", "m", "source", "data.p", "precision", "city")), ""},
		{"map precision constante con puntos en linea", pgDoc(nil, pgBlock("map", "summary", "m", "points", []any{map[string]any{"key": "a", "lat": 1.0, "lng": 2.0, "label": "A", "status": "ok"}}, "precision", "country")), ""},
		{"map precision fuera del enum", pgDoc(src, pgBlock("map", "summary", "m", "source", "data.p", "precision", "street")), CodeEnum},
		{"map precision y precision_field", pgDoc(src, pgBlock("map", "summary", "m", "source", "data.p", "precision", "city", "precision_field", "prec")), CodeInvalidBlock},
		{"map popup sin source", pgDoc(nil, pgBlock("map", "summary", "m", "points", []any{map[string]any{"key": "a", "lat": 1.0, "lng": 2.0, "label": "A", "status": "ok"}}, "popup", []any{"a"})), CodeInvalidBlock},
		{"map area_field sin source", pgDoc(nil, pgBlock("map", "summary", "m", "points", []any{map[string]any{"key": "a", "lat": 1.0, "lng": 2.0, "label": "A", "status": "ok"}}, "area_field", "iso")), CodeInvalidBlock},
		{"map precision_field sin source", pgDoc(nil, pgBlock("map", "summary", "m", "points", []any{map[string]any{"key": "a", "lat": 1.0, "lng": 2.0, "label": "A", "status": "ok"}}, "precision_field", "prec")), CodeInvalidBlock},
		{"map popup de 8 columnas", pgDoc(src, pgBlock("map", "summary", "m", "source", "data.p", "popup", []any{"a", "b", "c", "d", "e", "f", "g", "h"})), ""},
		{"map popup de 9 columnas", pgDoc(src, pgBlock("map", "summary", "m", "source", "data.p", "popup", []any{"a", "b", "c", "d", "e", "f", "g", "h", "i"})), CodeLimitItems},
		{"map popup con columna repetida", pgDoc(src, pgBlock("map", "summary", "m", "source", "data.p", "popup", []any{"a", "a"})), ""},
		{"map popup con clave invalida", pgDoc(src, pgBlock("map", "summary", "m", "source", "data.p", "popup", []any{"a b"})), CodePattern},
		{"map popup con proto", pgDoc(src, pgBlock("map", "summary", "m", "source", "data.p", "popup", []any{"a.prototype"})), CodeForbiddenKey},

		{"bloque desconocido sigue desconocido", pgDoc(nil, pgBlock("section", "mode", "hybrid", "blocks", []any{pgBlock("ranking")})), CodeUnknownBlock},
		{"area mal formada en cualquier bloque", pgDoc(nil, pgText("Bad")), CodePattern},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.doc, Options{})
			if tc.want == "" {
				if err != nil {
					t.Fatalf("se esperaba valido, error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("se esperaba %s y el documento paso", tc.want)
			}
			if got := CodeOf(err); got != tc.want {
				t.Fatalf("codigo %s, se esperaba %s (%v)", got, tc.want, err)
			}
		})
	}
}

// Los limites no cambian con los bloques de pagina: profundidad 6 y 200 bloques.
func TestPageBlocksKeepLimits(t *testing.T) {
	nest := func(sections int) map[string]any {
		var b any = pgText("")
		for i := 0; i < sections; i++ {
			b = pgBlock("section", "mode", "declarative", "blocks", []any{b})
		}
		return pgDoc(nil, b)
	}
	if _, err := Parse(nest(5), Options{}); err != nil { // 5 sections + 1 text = profundidad 6
		t.Fatalf("profundidad 6 debe valer: %v", err)
	}
	if _, err := Parse(nest(6), Options{}); CodeOf(err) != CodeLimitDepth {
		t.Fatalf("profundidad 7 debe dar %s, dio %v", CodeLimitDepth, err)
	}

	// 8 grids de 24 kpi_card = 8 + 192 = 200 bloques exactos.
	grids := func(extra int) map[string]any {
		var rows []any
		for g := 0; g < 8; g++ {
			var kids []any
			for i := 0; i < 24; i++ {
				kids = append(kids, pgBlock("kpi_card", "label", "L", "value", 1.0))
			}
			rows = append(rows, pgBlock("grid", "columns", 2.0, "blocks", kids))
		}
		for i := 0; i < extra; i++ {
			rows = append(rows, pgText(""))
		}
		return pgDoc(nil, rows...)
	}
	if _, err := Parse(grids(0), Options{}); err != nil {
		t.Fatalf("200 bloques deben valer: %v", err)
	}
	if _, err := Parse(grids(1), Options{}); CodeOf(err) != CodeLimitBlocks {
		t.Fatalf("201 bloques deben dar %s, dio %v", CodeLimitBlocks, err)
	}
}

// Una opcion de tabs custom no tiene hijos que degradar: Downgrade no debe romperse.
func TestDowngradeTabsWithComponent(t *testing.T) {
	d := pgDoc(nil, pgBlock("tabs", "tabs", []any{
		map[string]any{"key": "a", "label": "A", "component": "a.b@1"},
		map[string]any{"key": "b", "label": "B", "blocks": []any{pgText("")}},
	}))
	r, err := Downgrade(d, &Caps{Versions: []string{Version}})
	if err != nil {
		t.Fatalf("Downgrade: %v", err)
	}
	if _, err := Parse(r.Spec, Options{}); err != nil {
		t.Fatalf("el resultado no valida: %v", err)
	}
}
