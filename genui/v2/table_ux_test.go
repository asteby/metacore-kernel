package genuiv2

import (
	"fmt"
	"strings"
	"testing"
)

// Pruebas de la presentacion de tablas: max_height, density y las columnas con
// variant badge/icon_text (tones, labels, icons). Los vectores compartidos
// (testdata/conformance/*/table-*) fijan el contrato entre lenguajes.

func tuxCol(kv ...any) map[string]any {
	c := map[string]any{"key": "result", "label": "Resultado"}
	for i := 0; i+1 < len(kv); i += 2 {
		c[kv[i].(string)] = kv[i+1]
	}
	return c
}

func tuxTable(extra map[string]any, cols ...any) map[string]any {
	b := map[string]any{"type": "table", "columns": cols, "rows": []any{}}
	for k, v := range extra {
		b[k] = v
	}
	return b
}

func tuxMap(n int, val any) map[string]any {
	m := map[string]any{}
	for i := 0; i < n; i++ {
		m[fmt.Sprintf("v%d", i)] = val
	}
	return m
}

func TestTableUX(t *testing.T) {
	badge := func(kv ...any) map[string]any { return tuxCol(append([]any{"variant", "badge"}, kv...)...) }
	tests := []struct {
		name string
		doc  map[string]any
		want string
	}{
		{"tabla clasica sigue valida", pgDoc(nil, tuxTable(nil, tuxCol("format", "relative_date"))), ""},
		{"max_height sm", pgDoc(nil, tuxTable(map[string]any{"max_height": "sm"}, tuxCol())), ""},
		{"max_height md", pgDoc(nil, tuxTable(map[string]any{"max_height": "md"}, tuxCol())), ""},
		{"max_height lg", pgDoc(nil, tuxTable(map[string]any{"max_height": "lg"}, tuxCol())), ""},
		{"max_height fuera del enum", pgDoc(nil, tuxTable(map[string]any{"max_height": "xl"}, tuxCol())), CodeEnum},
		{"max_height con unidades", pgDoc(nil, tuxTable(map[string]any{"max_height": "24rem"}, tuxCol())), CodeEnum},
		{"max_height con source", pgDoc(map[string]any{"d": map[string]any{"q": "t:x"}}, map[string]any{"type": "table", "source": "data.d", "columns": []any{tuxCol()}, "max_height": "md"}), ""},
		{"density compact", pgDoc(nil, tuxTable(map[string]any{"density": "compact"}, tuxCol())), ""},
		{"density comfortable", pgDoc(nil, tuxTable(map[string]any{"density": "comfortable"}, tuxCol())), ""},
		{"density fuera del enum", pgDoc(nil, tuxTable(map[string]any{"density": "dense"}, tuxCol())), CodeEnum},

		{"variant text explicito", pgDoc(nil, tuxTable(nil, tuxCol("variant", "text"))), ""},
		{"variant badge sin mapas", pgDoc(nil, tuxTable(nil, badge())), ""},
		{"variant icon_text sin mapas", pgDoc(nil, tuxTable(nil, tuxCol("variant", "icon_text"))), ""},
		{"variant fuera del enum", pgDoc(nil, tuxTable(nil, tuxCol("variant", "pill"))), CodeEnum},
		{"feed en vivo completo", pgDoc(nil, tuxTable(map[string]any{"max_height": "md", "density": "compact"},
			tuxCol("key", "at", "label", "Hora", "format", "relative_date"),
			badge("tones", map[string]any{"ok": "success", "pending": "warning", "error": "danger"},
				"labels", map[string]any{"ok": map[string]any{"$t": "feed.ok"}, "pending": map[string]any{"$t": "feed.pending"}, "error": "Error"},
				"icons", map[string]any{"ok": "CheckCircle", "pending": "Clock", "error": "XCircle"}))), ""},
		{"icon_text con icons", pgDoc(nil, tuxTable(nil, tuxCol("variant", "icon_text", "icons", map[string]any{"ok": "Check"}))), ""},

		{"tones con variant text", pgDoc(nil, tuxTable(nil, tuxCol("tones", map[string]any{"ok": "success"}))), CodeInvalidBlock},
		{"labels con variant text", pgDoc(nil, tuxTable(nil, tuxCol("variant", "text", "labels", map[string]any{"ok": "OK"}))), CodeInvalidBlock},
		{"icons sin variant", pgDoc(nil, tuxTable(nil, tuxCol("icons", map[string]any{"ok": "Check"}))), CodeInvalidBlock},
		{"mapas vacios con variant text", pgDoc(nil, tuxTable(nil, tuxCol("tones", map[string]any{}))), CodeInvalidBlock},

		{"tono fuera del enum", pgDoc(nil, tuxTable(nil, badge("tones", map[string]any{"ok": "green"}))), CodeEnum},
		{"tono no cadena", pgDoc(nil, tuxTable(nil, badge("tones", map[string]any{"ok": 1.0}))), CodeEnum},
		{"tones no es objeto", pgDoc(nil, tuxTable(nil, badge("tones", []any{"success"}))), CodeType},
		{"tones con 32 entradas", pgDoc(nil, tuxTable(nil, badge("tones", tuxMap(32, "info")))), ""},
		{"tones con 33 entradas", pgDoc(nil, tuxTable(nil, badge("tones", tuxMap(33, "info")))), CodeLimitProperties},
		{"labels con 33 entradas", pgDoc(nil, tuxTable(nil, badge("labels", tuxMap(33, "x")))), CodeLimitProperties},
		{"icons con 33 entradas", pgDoc(nil, tuxTable(nil, badge("icons", tuxMap(33, "Check")))), CodeLimitProperties},
		{"clave de tones vacia", pgDoc(nil, tuxTable(nil, badge("tones", map[string]any{"": "info"}))), CodePattern},
		{"clave de tones de 64", pgDoc(nil, tuxTable(nil, badge("tones", map[string]any{strings.Repeat("a", 64): "info"}))), ""},
		{"clave de tones de 65", pgDoc(nil, tuxTable(nil, badge("tones", map[string]any{strings.Repeat("a", 65): "info"}))), CodePattern},
		{"clave de labels vacia", pgDoc(nil, tuxTable(nil, badge("labels", map[string]any{"": "x"}))), CodePattern},
		{"clave de icons vacia", pgDoc(nil, tuxTable(nil, badge("icons", map[string]any{"": "Check"}))), CodePattern},
		{"clave de tones con $", pgDoc(nil, tuxTable(nil, badge("tones", map[string]any{"$ref": "info"}))), CodeRefNotAllowed},
		{"clave de tones prototipo", pgDoc(nil, tuxTable(nil, badge("tones", map[string]any{"constructor": "info"}))), CodeForbiddenKey},
		{"clave de tones __proto__", pgDoc(nil, tuxTable(nil, badge("tones", map[string]any{"__proto__": "info"}))), CodeForbiddenKey},
		{"clave de labels prototype", pgDoc(nil, tuxTable(nil, badge("labels", map[string]any{"prototype": "x"}))), CodeForbiddenKey},

		{"label con $t y args", pgDoc(nil, tuxTable(nil, badge("labels", map[string]any{"ok": map[string]any{"$t": "feed.ok", "args": map[string]any{"n": 1.0}}}))), ""},
		{"label vinculacion no permitida", pgDoc(nil, tuxTable(nil, badge("labels", map[string]any{"ok": map[string]any{"$ref": "data.x.y"}}))), CodeRequired},
		{"label $t con proto", pgDoc(nil, tuxTable(nil, badge("labels", map[string]any{"ok": map[string]any{"$t": "a.__proto__.b"}}))), CodeForbiddenKey},
		{"label literal de 160", pgDoc(nil, tuxTable(nil, badge("labels", map[string]any{"ok": strings.Repeat("a", 160)}))), ""},
		{"label literal de 161", pgDoc(nil, tuxTable(nil, badge("labels", map[string]any{"ok": strings.Repeat("a", 161)}))), CodeLimitLength},
		{"label numerico", pgDoc(nil, tuxTable(nil, badge("labels", map[string]any{"ok": 3.0}))), CodeType},

		{"icono por nombre de host", pgDoc(nil, tuxTable(nil, badge("icons", map[string]any{"ok": "A" + strings.Repeat("b", 63)}))), ""},
		{"icono de 65", pgDoc(nil, tuxTable(nil, badge("icons", map[string]any{"ok": "A" + strings.Repeat("b", 64)}))), CodePattern},
		{"icono con guion", pgDoc(nil, tuxTable(nil, badge("icons", map[string]any{"ok": "check-circle"}))), CodePattern},
		{"icono prototipo", pgDoc(nil, tuxTable(nil, badge("icons", map[string]any{"ok": "constructor"}))), CodeForbiddenKey},
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
			if CodeOf(err) != tc.want {
				t.Fatalf("se esperaba %s, dio %v", tc.want, err)
			}
		})
	}
}
