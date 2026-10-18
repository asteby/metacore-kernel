package dataload

import (
	"strings"
	"testing"
)

func TestIsGTIN(t *testing.T) {
	for s, want := range map[string]bool{
		"742033312414": true, "742033312339": true, "684611315595": true,
		"742033312415": false, "FS00150GL": false, "": false, "12345": false, "4006381333931": true,
	} {
		if got := IsGTIN(s); got != want {
			t.Errorf("IsGTIN(%q)=%v, quería %v", s, got, want)
		}
	}
}

func deriveProfile() Profile {
	return Profile{
		Name: "d", Version: "1", Entity: "row", Key: []string{"id"},
		Fields: []Field{
			{Target: "id", From: []string{"ID"}},
			{Target: "sku", From: []string{"SKU"}},
			{Target: "price", Type: "number", Derive: &Derive{Op: DeriveFirst, Cols: []string{"Precio rebajado", "Precio normal"}}},
			{Target: "compare_at", Type: "number", Derive: &Derive{Op: DeriveFirst, Cols: []string{"Precio normal"}, When: "Precio rebajado"}},
			{Target: "barcode", Derive: &Derive{Op: DeriveGTIN, Cols: []string{"GTIN", "SKU"}}},
			{Target: "parent", Derive: &Derive{Op: DeriveExtract, Cols: []string{"Superior"}, Pattern: `^id:(\d+)$`}},
			{Target: "tags", From: []string{"Etiquetas"}, Split: ","},
		},
	}
}

func TestDerive_FirstGTINExtractAndSplit(t *testing.T) {
	p := deriveProfile()
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	spec := p.importSpec()
	rec, _, _, issues := p.buildRecord(spec, map[string]any{
		"ID": "5", "SKU": "742033312414", "Precio normal": "195", "Precio rebajado": "175", "Superior": "id:9487", "Etiquetas": " creatine , ares ,",
	}, nil, 0)
	if len(issues) > 0 {
		t.Fatal(issues)
	}
	if rec["price"] != 175.0 || rec["compare_at"] != 195.0 || rec["barcode"] != "742033312414" || rec["parent"] != "9487" || rec["sku"] != "742033312414" {
		t.Fatalf("registro inesperado: %v", rec)
	}
	if tags, _ := rec["tags"].([]any); len(tags) != 2 || tags[0] != "creatine" || tags[1] != "ares" {
		t.Fatalf("tags = %v", rec["tags"])
	}
	// Sin precio rebajado: price = normal y no hay compare_at. SKU no GTIN: sin barcode.
	rec, _, _, _ = p.buildRecord(spec, map[string]any{"ID": "6", "SKU": "FS00150GL", "Precio normal": "250", "Superior": ""}, nil, 1)
	if rec["price"] != 250.0 || rec["compare_at"] != nil || rec["barcode"] != nil || rec["parent"] != nil {
		t.Fatalf("registro inesperado: %v", rec)
	}
	if _, has := rec["tags"]; has {
		t.Fatalf("tags vacías deben quedar ausentes: %v", rec)
	}
}

func TestDerive_ValidationAndHash(t *testing.T) {
	p := deriveProfile()
	p.Fields[2].From = []string{"X"}
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "con derive no se usa from") {
		t.Fatalf("got %v", err)
	}
	p = deriveProfile()
	p.Fields[5].Derive.Pattern = `^id:\d+$`
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "grupo de captura") {
		t.Fatalf("got %v", err)
	}
	p = deriveProfile()
	p.Fields[4].Derive.Op = "magia"
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "magia") {
		t.Fatalf("got %v", err)
	}
}

func TestOverrides_CleanAddAndUnknownTarget(t *testing.T) {
	p := Profile{Name: "c", Entity: "e", Key: []string{"id"}, Fields: []Field{{Target: "id", From: []string{"ID"}}, {Target: "d", From: []string{"D"}, Clean: []string{CleanTrim}}}}
	r, err := p.Resolve(&Overrides{CleanAdd: map[string][]string{"d": {"drop_lines:(?i)^aviso"}}})
	if err != nil {
		t.Fatal(err)
	}
	f, _ := r.field("d")
	if len(f.Clean) != 2 || len(p.Fields[1].Clean) != 1 {
		t.Fatalf("cleanAdd mal aplicado o mutó el perfil base: %v / %v", f.Clean, p.Fields[1].Clean)
	}
	if _, err := p.Resolve(&Overrides{CleanAdd: map[string][]string{"nada": {CleanTrim}}}); err == nil {
		t.Fatal("cleanAdd de un campo inexistente debía fallar")
	}
	if _, err := p.Resolve(&Overrides{CleanAdd: map[string][]string{"d": {"drop_lines:("}}}); err == nil {
		t.Fatal("regex inválida debía fallar")
	}
}

func TestClean_DropLinesSanitizeAndParagraphs(t *testing.T) {
	rules := []string{CleanUnescapeNewline, "drop_lines:(?i)este producto no es un medicamento", "drop_lines:(?i)^\\s*(<[^>]+>)*\\s*im[aá]genes con fines", CleanSanitizeHTML, CleanCollapseBlankLines, CleanAutoParagraphs}
	in := "Proteína aislada\n\\n<ul>\n\\n \t<li>30 porciones</li>\n\\n \t<li>0 carbs</li>\n\\n</ul>\n\\nEste producto no es un medicamento y es responsabilidad de quien lo usa.\n\\n\n\\nImágenes con fines exclusivos para venta."
	want := "<p>Proteína aislada</p>\n<ul>\n<li>30 porciones</li>\n<li>0 carbs</li>\n</ul>"
	if got := applyClean(rules, in); got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	// Solo avisos: queda vacío (sin descripción).
	if got := applyClean(rules, "<hr />\n\\n\n\\nEste producto no es un medicamento.\n\\n\n\\nImágenes con fines exclusivos para venta."); got != "" {
		t.Fatalf("debía quedar vacío, got %q", got)
	}
	// Lista con el aviso dentro de un <li>.
	if got := applyClean(rules, "<ol>\n\\n \t<li>Omega-3</li>\n\\n \t<li>Este producto no es un medicamento.</li>\n\\n</ol>"); got != "<ol>\n<li>Omega-3</li>\n</ol>" {
		t.Fatalf("got %q", got)
	}
}

func TestSanitizeHTML_Security(t *testing.T) {
	cases := map[string]string{
		`<script>alert(1)</script>hola`:                           "hola",
		`<p onclick="x()" style="a:b">t</p>`:                      "<p>t</p>",
		`<a href="javascript:alert(1)">x</a>`:                     "<a>x</a>",
		`<a href="https://ok.mx/p?a=1" target=_blank>x</a>`:       `<a href="https://ok.mx/p?a=1">x</a>`,
		`<img src=x onerror=alert(1)>texto`:                       "texto",
		`<!-- c -->a <b>b</b> < c`:                                "a <b>b</b> &lt; c",
		`<style>p{}</style><iframe src="x"></iframe><div>d</div>`: "d",
	}
	for in, want := range cases {
		if got := sanitizeHTML(in); got != want {
			t.Errorf("sanitizeHTML(%q) = %q, quería %q", in, got, want)
		}
	}
}
