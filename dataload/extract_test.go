package dataload

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
)

// Las pruebas de la etapa extract usan un documento sintético (testdata/lista.*.json) con la
// misma forma que el de un consumidor real de refacciones; el motor no sabe nada de ese giro.

func loadLista(t *testing.T) (Profile, *Extractor) {
	t.Helper()
	pb, err := os.ReadFile("testdata/lista.profile.json")
	if err != nil {
		t.Fatal(err)
	}
	eb, err := os.ReadFile("testdata/lista.extract.json")
	if err != nil {
		t.Fatal(err)
	}
	prof, err := ParseProfile(pb)
	if err != nil {
		t.Fatal(err)
	}
	ex, err := ParseExtract(eb)
	if err != nil {
		t.Fatal(err)
	}
	return prof, ex
}

// describe corre solo la etapa extract sobre una descripción.
func describe(t *testing.T, desc, cat string) (map[string]any, Extraction) {
	t.Helper()
	prof, ex := loadLista(t)
	rec := map[string]any{"sku": "S1", "description_raw": strings.ToUpper(desc), "category_source": strings.ToUpper(cat)}
	x := ex.Apply(prof, map[string]any{}, rec)
	return rec, x
}

func attrs(rec map[string]any) map[string]any {
	a, _ := rec["attributes"].(map[string]any)
	return a
}

func tags(rec map[string]any) []string {
	var out []string
	if l, ok := rec["tags"].([]any); ok {
		for _, v := range l {
			out = append(out, v.(string))
		}
	}
	return out
}

func codes(x Extraction) []string { return x.Codes() }

func TestExtract_ReadsPieceVehicleEngineYearsAndWritesTagsAndCompatTable(t *testing.T) {
	rec, x := describe(t, "BALATA DISCO DELANTERO CERAMICA NISSAN ZETA 1.6 L4 2010 2012", "FRENOS")
	if x.Status != "ok" || len(x.Review) != 0 {
		t.Fatalf("debía quedar ok: %+v", x)
	}
	a := attrs(rec)
	want := map[string]any{
		"pieza": "BALATA DISCO", "detalle": "CERAMICA", "position": "delantera", "vehiculo_marca": "Nissan", "vehiculo_modelo": "ZETA",
		"motor": "1.6 L4", "anio_desde": 2010, "anio_hasta": 2012, "part_number": "S1", "compatibilidad": "Nissan|ZETA|2010-2012|1.6 L4|",
	}
	for k, v := range want {
		if a[k] != v {
			t.Errorf("attributes.%s = %#v, quería %#v", k, a[k], v)
		}
	}
	if !reflect.DeepEqual(tags(rec), []string{"fit:nissan/zeta/2010/1.6", "fit:nissan/zeta/2011/1.6", "fit:nissan/zeta/2012/1.6"}) {
		t.Fatalf("tags = %v", tags(rec))
	}
	if rec["category"] != "frenos" || rec["title"] != "BALATA DISCO DELANTERO CERAMICA NISSAN ZETA 1.6 L4 2010 2012" {
		t.Fatalf("categoría/título: %v / %v", rec["category"], rec["title"])
	}
	if _, kept := rec["description_raw"]; kept {
		t.Fatal("outputs.drop debía quitar el campo intermedio")
	}
}

func TestExtract_CaptureConsumeTitleRemoveFlagsAndLists(t *testing.T) {
	rec, x := describe(t, "ROTULA INFERIOR TOYOTA YARIS 1.5 L4 2007 2007 NUEVO C/2 SUSTITUYE A ABC123", "SUSPENSION Y DIRECCION")
	if x.Status != "ok" {
		t.Fatalf("%+v", x.Review)
	}
	a := attrs(rec)
	if a["nuevo"] != true || a["caja_piezas"] != 2 || a["equivalencias"] != "ABC123" || a["position"] != "inferior" {
		t.Fatalf("attrs = %v", a)
	}
	// NUEVO y SUSTITUYE se quitan del título; C/2 y los años se conservan.
	if rec["title"] != "ROTULA INFERIOR TOYOTA YARIS 1.5 L4 2007 2007 C/2" {
		t.Fatalf("title = %q", rec["title"])
	}
	// Un solo año: la tabla lo escribe sin rango.
	if a["compatibilidad"] != "Toyota|YARIS|2007|1.5 L4|" {
		t.Fatalf("compatibilidad = %v", a["compatibilidad"])
	}
}

func TestExtract_AmbiguousEquivalenceIsReviewedNotGuessed(t *testing.T) {
	rec, x := describe(t, "ROTULA INFERIOR TOYOTA YARIS 1.5 L4 2007 2012 SUSTITUYE A ABC123 A DEF456", "SUSPENSION Y DIRECCION")
	if x.Status != "revisar" || !reflect.DeepEqual(codes(x), []string{"equivalencia_ambigua"}) {
		t.Fatalf("%+v", x)
	}
	if attrs(rec)["equivalencias"] != "ABC123,DEF456" {
		t.Fatalf("las equivalencias se listan para revisarlas: %v", attrs(rec)["equivalencias"])
	}
}

func TestExtract_UnknownEngineKeepsCompatTableButNoFitTags(t *testing.T) {
	rec, x := describe(t, "AMORTIGUADOR TRASERO MAZDA 3 - - 2008 2013", "SUSPENSION Y DIRECCION")
	if x.Status != "revisar" || !reflect.DeepEqual(codes(x), []string{"motor_desconocido"}) {
		t.Fatalf("%+v", x.Review)
	}
	if len(tags(rec)) != 0 {
		t.Fatalf("sin cilindrada no se inventan etiquetas: %v", tags(rec))
	}
	if attrs(rec)["compatibilidad"] != "Mazda|3|2008-2013||" {
		t.Fatalf("compatibilidad = %v", attrs(rec)["compatibilidad"])
	}
	// "-" es relleno del origen: no entra al título.
	if rec["title"] != "AMORTIGUADOR TRASERO MAZDA 3 2008 2013" {
		t.Fatalf("title = %q", rec["title"])
	}
}

func TestExtract_EntityGuards_SuspectModelAmbiguousBrandAndAllowList(t *testing.T) {
	cases := []struct {
		desc string
		want []string
		cmp  bool // ¿escribe compatibilidad?
	}{
		{"BIELETA DELANTERA FORD RANGER EXCEPTO 4X4 2.3 L4 2005 2010", []string{"modelo_sospechoso"}, false},
		{"BIELETA DELANTERA FORD UNO DOS TRES CUATRO CINCO 2.3 L4 2005 2010", []string{"modelo_sospechoso"}, false}, // más de 4 palabras
		{"BIELETA DELANTERA FORD MAZDA 3 2.3 L4 2005 2010", []string{"marca_ambigua"}, false},                       // otra marca dentro del modelo
		{"BIELETA DELANTERA DODGE RAM 1500 5.7 V8 2005 2010", nil, true},                                            // permitida por allowIn
		{"BIELETA DELANTERA FORD 2.3 L4 2005 2010", []string{"vehiculo_no_reconocido"}, false},                      // marca sin modelo: no hay vehículo
		{"BIELETA DELANTERA FORD FOCUS 2.0 L4 2012 2005", []string{"anios_invalidos"}, false},                       // años invertidos
		{"BIELETA DELANTERA FORD FOCUS 2.0 L4 1800 1810", []string{"anios_invalidos"}, false},                       // fuera de rango
		{"BIELETA DELANTERA FORD FOCUS 2.0 L4", []string{"anios_no_reconocidos"}, false},
		{"BIELETA DELANTERA FORD FOCUS 2012 2014", []string{"motor_no_reconocido"}, false},
	}
	for _, c := range cases {
		rec, x := describe(t, c.desc, "SUSPENSION Y DIRECCION")
		got := codes(x)
		if !reflect.DeepEqual(got, c.want) && !(len(got) == 0 && len(c.want) == 0) {
			// un caso puede sumar códigos derivados; exigimos que incluya los esperados
			for _, w := range c.want {
				if !contains(got, w) {
					t.Errorf("%q: códigos %v, faltó %s", c.desc, got, w)
				}
			}
		}
		_, hasCompat := attrs(rec)["compatibilidad"]
		if hasCompat != c.cmp {
			t.Errorf("%q: compatibilidad=%v, quería %v (%v)", c.desc, hasCompat, c.cmp, attrs(rec))
		}
		if !c.cmp && (len(tags(rec)) != 0 || attrs(rec)["vehiculo_marca"] != nil) {
			t.Errorf("%q: con duda no debe quedar vehículo ni etiquetas: %v %v", c.desc, tags(rec), attrs(rec))
		}
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func TestExtract_TokensOrderPrefixAndUnknownPiece(t *testing.T) {
	rec, _ := describe(t, "AMORTIGUADOR DER/DEL 4X4 HONDA CIVIC 1.8 L4 2012 2014", "SUSPENSION Y DIRECCION")
	a := attrs(rec)
	if a["position"] != "delantera/derecha" || a["traccion"] != "4X4" || a["pieza"] != "AMORTIGUADOR" {
		t.Fatalf("attrs = %v", a)
	}
	rec, x := describe(t, "PIEZA RARA HONDA CIVIC 1.8 L4 2012 2014", "FRENOS")
	if attrs(rec)["pieza"] != "PIEZA RARA" || !contains(codes(x), "pieza_sin_catalogo") || x.Status != "ok" {
		t.Fatalf("una pieza fuera del vocabulario solo informa: %v %+v", attrs(rec), x.Review)
	}
}

func TestExtract_UniversalAndOptionalVehicleAndCategoryMap(t *testing.T) {
	rec, x := describe(t, "FILTRO ACEITE UNIVERSAL", "AFINACION")
	if x.Status != "ok" || !contains(codes(x), "universal") || !reflect.DeepEqual(tags(rec), []string{"fit:universal"}) || rec["category"] != "filtros" {
		t.Fatalf("universal: %v %+v tags=%v cat=%v", attrs(rec), x.Review, tags(rec), rec["category"])
	}
	rec, x = describe(t, "ACEITE MOTOR 10W40 SINTETICO", "QUIMICOS")
	if x.Status != "ok" || !reflect.DeepEqual(codes(x), []string{"categoria_sin_mapa", "sin_vehiculo_opcional"}) {
		t.Fatalf("categoría sin mapa y vehículo opcional solo informan: %+v", x.Review)
	}
	if _, has := rec["category"]; has {
		t.Fatalf("sin equivalencia no se escribe categoría: %v", rec["category"])
	}
	_, x = describe(t, "GATO HIDRAULICO 2 TON", "FRENOS")
	if x.Status != "revisar" || !contains(codes(x), "vehiculo_no_reconocido") {
		t.Fatalf("sin vehículo ni universal ni categoría opcional se revisa: %+v", x.Review)
	}
}

func TestExtract_RowChecks_SumGreaterStockRawAndRange(t *testing.T) {
	prof, ex := loadLista(t)
	rec := map[string]any{
		"sku": "S", "description_raw": "BUJE DEL NISSAN ZETA 1.6 L4 2010 2012", "category_source": "FRENOS",
		"price": 1000.0, "price_before_tax": 800.0, "tax_amount": 100.0, "cost": 1200.0,
		"stock": map[string]any{"cdmx": 2.5, "edomex": -1.0},
	}
	raw := map[string]any{"CDMX": "2.5", "EDO MEX": "#n/d"}
	x := ex.Apply(prof, raw, rec)
	want := []string{"costo_mayor_que_precio", "precio_incoherente", "stock_fraccionario", "stock_nd", "stock_negativo"}
	if !reflect.DeepEqual(codes(x), want) {
		t.Fatalf("codes = %v, quería %v", codes(x), want)
	}
	// Dentro de la tolerancia (2 centavos) la suma cuadra.
	rec["price"], rec["price_before_tax"], rec["tax_amount"], rec["cost"] = 1059.32, 913.21, 146.12, 10.0
	rec["stock"] = map[string]any{"cdmx": 2.0}
	if x := ex.Apply(prof, map[string]any{}, rec); x.Status != "ok" {
		t.Fatalf("tolerancia: %+v", x.Review)
	}
}

func TestExtract_DatasetCheckFlagsEveryRowOfARepeatedValue(t *testing.T) {
	_, ex := loadLista(t)
	rows := []RowFacts{{Record: map[string]any{"sku": "A"}}, {Record: map[string]any{"sku": "B"}}, {Record: map[string]any{"sku": "A"}}, {Record: nil}}
	got := ex.CheckDataset(rows)
	if len(got) != 2 || got[0].Index != 0 || got[1].Index != 2 || got[0].Item.Code != "sku_duplicado" || got[0].Item.Severity != SeverityRevisar {
		t.Fatalf("got = %+v", got)
	}
}

func TestExtract_ParseRejectsBadDocuments(t *testing.T) {
	good, _ := os.ReadFile("testdata/lista.extract.json")
	mutate := func(from, to string) []byte {
		if !strings.Contains(string(good), from) {
			t.Fatalf("la mutación no aplica: %q", from)
		}
		return []byte(strings.Replace(string(good), from, to, 1))
	}
	cases := map[string]struct {
		doc  []byte
		frag string
	}{
		"campo desconocido":      {mutate(`"schema":`, `"numbers": {}, "schema":`), "unknown field"},
		"esquema":                {mutate("extract/v1", "extract/v9"), "no soportado"},
		"regex inválida":         {mutate(`(?:^|\\s)(?<flag>NUEVO)(?:\\s|$)`, `(?:^|\\s)(?<flag>NUEVO(?:\\s|$)`), "patrón"},
		"lookahead no es RE2":    {mutate(`(?<flag>NUEVO)`, `(?<flag>NUEVO)(?=X)`), "patrón"},
		"grupo inexistente":      {mutate(`"group": "codes"`, `"group": "nope"`), "no tiene el grupo"},
		"código sin severidad":   {mutate(`"code": "motor_desconocido"`, `"code": "inventado"`), "inventado"},
		"vocab inexistente":      {mutate(`"vocab": "vehicleBrands"`, `"vocab": "nada"`), "inexistente"},
		"plantilla sin cerrar":   {mutate(`"template": "{anio_desde|span(anio_hasta)}"`, `"template": "{anio_desde"`), "sin cerrar"},
		"filtro desconocido":     {mutate(`{motor_cilindros}`, `{motor_cilindros|magia}`), "filtro"},
		"tipo de regla":          {mutate(`"kind": "tokens", "from": "pieza", "dictionary": "positions"`, `"kind": "adivina", "from": "pieza", "dictionary": "positions"`), "tipo de regla"},
		"tipo de check":          {mutate(`"type": "greater"`, `"type": "mayor"`), "desconocido"},
		"limpieza desconocida":   {mutate(`"normalize": ["upper"`, `"normalize": ["mayusculas"`), "regla de limpieza"},
		"código en dos niveles":  {mutate(`"info": ["pieza_sin_catalogo"`, `"info": ["motor_desconocido", "pieza_sin_catalogo"`), "revisar e info"},
		"id de regla repetido":   {mutate(`"id": "nuevo"`, `"id": "caja"`), "repetido"},
		"tokens sin diccionario": {mutate(`"dictionary": "drivetrain"`, `"dictionary": "pieceTypes"`), "diccionario"},
	}
	for name, c := range cases {
		_, err := ParseExtract(c.doc)
		if err == nil || !strings.Contains(err.Error(), c.frag) {
			t.Errorf("%s: err = %v, debía mencionar %q", name, err, c.frag)
		}
	}
	if _, err := ParseExtract(good); err != nil {
		t.Fatalf("el documento bueno debe cargar: %v", err)
	}
}

func TestExtract_MustMatchProfileVersion(t *testing.T) {
	prof, ex := loadLista(t)
	if err := ex.CheckProfile(prof); err != nil {
		t.Fatal(err)
	}
	prof.Version = "2"
	if err := ex.CheckProfile(prof); err == nil {
		t.Fatal("un documento de otra versión del perfil no debe aceptarse")
	}
	_, err := BuildPlan(context.Background(), PlanRequest{Org: "o", Profile: prof, Source: NewRowsSource("x", nil), Target: NewMemoryTarget(), Extract: ex})
	if err == nil {
		t.Fatal("BuildPlan debe exigir el mismo perfil")
	}
}

func TestExtract_HashIsCanonicalAndChangesWithTheDocument(t *testing.T) {
	_, ex := loadLista(t)
	eb, _ := os.ReadFile("testdata/lista.extract.json")
	// Mismo contenido con otro espaciado y sin cambios semánticos: misma huella.
	again, err := ParseExtract([]byte(strings.ReplaceAll(string(eb), "  ", " ")))
	if err != nil || again.Hash() != ex.Hash() {
		t.Fatalf("la huella no debe depender del formato: %v", err)
	}
	changed, err := ParseExtract([]byte(strings.Replace(string(eb), `"maxTokens": 4`, `"maxTokens": 5`, 1)))
	if err != nil || changed.Hash() == ex.Hash() {
		t.Fatalf("un cambio de regla debe cambiar la huella: %v", err)
	}
}
