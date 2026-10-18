package dataload

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/i18n"
	"github.com/asteby/metacore-kernel/importer"
)

// codeConstants lee del paquete las constantes de tipo Code (nombre Go -> clave).
func codeConstants(t *testing.T) map[string]string {
	t.Helper()
	files, _ := filepath.Glob("*.go")
	out := map[string]string{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok || g.Tok != token.CONST {
				continue
			}
			for _, s := range g.Specs {
				vs := s.(*ast.ValueSpec)
				if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "Code" {
					continue
				}
				for i, n := range vs.Names {
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok {
						t.Fatalf("%s no es un literal", n.Name)
					}
					v, _ := strconv.Unquote(lit.Value)
					out[n.Name] = v
				}
			}
		}
	}
	return out
}

func loadLocale(t *testing.T, lang string) map[string]string {
	t.Helper()
	b, err := os.ReadFile("locales/" + lang + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

var placeholderRE = regexp.MustCompile(`\{([a-z][a-zA-Z]*)\}`)

func placeholders(s string) []string {
	var out []string
	for _, m := range placeholderRE.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	sort.Strings(out)
	return out
}

// Punto 6: cada código tiene texto en cada idioma, con los mismos parámetros, y
// el catálogo no trae claves huérfanas.
func TestMessages_CatalogCoversEveryCodeInEveryLanguage(t *testing.T) {
	codes := codeConstants(t)
	keys := map[string]bool{}
	for name, key := range codes {
		if !strings.HasPrefix(key, "dataload.") {
			t.Errorf("%s = %q: los códigos van con el prefijo dataload.", name, key)
		}
		if keys[key] {
			t.Errorf("código repetido %q", key)
		}
		keys[key] = true
	}
	// El catálogo también traduce los problemas de fila que produce el importador.
	for _, c := range importer.IssueCodes() {
		keys[c] = true
	}
	es, en := loadLocale(t, "es"), loadLocale(t, "en")
	for _, cat := range []map[string]string{es, en} {
		for k := range cat {
			if !keys[k] {
				t.Errorf("clave huérfana en el catálogo: %q", k)
			}
		}
	}
	for key := range keys {
		e, ok1 := es[key]
		n, ok2 := en[key]
		if !ok1 || !ok2 {
			t.Errorf("%q sin texto (es=%v en=%v)", key, ok1, ok2)
			continue
		}
		if pe, pn := placeholders(e), placeholders(n); strings.Join(pe, ",") != strings.Join(pn, ",") {
			t.Errorf("%q: parámetros distintos es=%v en=%v", key, pe, pn)
		}
	}
	if Messages().Len() == 0 {
		t.Fatal("catálogo embebido vacío")
	}
}

// Punto 6: el motor no escribe textos fijos: ni errors.New/fmt.Errorf ni
// literales en español fuera de las pruebas y del catálogo.
func TestMessages_NoFixedTextInEngine(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	spanish := regexp.MustCompile(`[áéíóúñÁÉÍÓÚÑ¿¡]`)
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0) // sin comentarios
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
					if id, ok := sel.X.(*ast.Ident); ok && ((id.Name == "errors" && sel.Sel.Name == "New") || (id.Name == "fmt" && sel.Sel.Name == "Errorf")) {
						t.Errorf("%s: %s.%s; usa newErr/wrapErr con un Code", fset.Position(x.Pos()), id.Name, sel.Sel.Name)
					}
				}
			case *ast.BasicLit:
				if x.Kind == token.STRING && spanish.MatchString(x.Value) {
					t.Errorf("%s: texto fijo %s; va en locales/*.json", fset.Position(x.Pos()), x.Value)
				}
			}
			return true
		})
	}
}

func TestMessages_ErrorsAreLocalizableAndMatchSentinels(t *testing.T) {
	err := wrapErr(errors.New("disk full"), CodeApplyBatch, "from", 1, "to", 200)
	if got := err.Error(); got != "dataload: aplicar lote 1-200: disk full" {
		t.Fatalf("es por omisión = %q", got)
	}
	en := i18n.WithLanguage(context.Background(), "en-US")
	if got := Localize(en, err); got != "dataload: applying batch 1-200: disk full" {
		t.Fatalf("en = %q", got)
	}
	if ErrorCode(err) != CodeApplyBatch {
		t.Fatalf("code = %q", ErrorCode(err))
	}
	// Los centinelas se comparan por código aunque el error traiga parámetros.
	nf := newErr(CodeReviewNotFound, "entity", "item", "key", "x")
	if !errors.Is(nf, ErrReviewNotFound) || errors.Is(nf, ErrReviewStale) {
		t.Fatal("errors.Is por código")
	}
	// Un detalle anidado se traduce en el mismo idioma, también tras pasar por JSON.
	bad := detailsErr([]Message{msg(CodeProfileFieldProblem, "target", "sku", "detail", msg(CodeDeriveNoCols))}, CodeProfileInvalid, "name", "p")
	if got := Localize(en, bad); got != `profile "p": "sku": derive without cols` {
		t.Fatalf("anidado = %q", got)
	}
	var round Message
	b, _ := json.Marshal(bad.(*Error).Details[0])
	_ = json.Unmarshal(b, &round)
	if got := round.Text("en"); got != `"sku": derive without cols` {
		t.Fatalf("tras JSON = %q", got)
	}
}

// Punto 6: lo que llega a ops (corrida, problemas de fila, motivos) trae código.
func TestMessages_RunIssuesAndReviewItemsCarryCodes(t *testing.T) {
	ctx := context.Background()
	tgt := NewMemoryTarget()
	src := NewCSVSource("dup.csv", []byte("ID,Nombre\n1,A\n1,B\n,C\n"))
	plan := mustPlan(t, PlanRequest{Org: "org", Profile: catalogProfile(), Source: src, Target: tgt})
	if is := plan.Rows[1].Issues[0]; is.Code != string(CodeRowDuplicate) || is.Params["first"] != 1 || is.Message != "identidad repetida; se usa la fila 1" {
		t.Fatalf("duplicada: %+v", is)
	}
	if is := plan.Rows[2].Issues[0]; is.Code != importer.IssueRequired || is.Params["column"] != "ID" {
		t.Fatalf("columna requerida sin código: %+v", is)
	}
	if got := renderCode(Messages(), "en", Code(plan.Rows[2].Issues[0].Code), plan.Rows[2].Issues[0].Params); got != "'ID' missing" {
		t.Fatalf("en = %q", got)
	}

	r := &Runner{Ledger: NewMemoryLedger(), Target: tgt}
	defer r.Close()
	run, err := r.Start(ctx, RunRequest{Org: "org", Profile: catalogProfile(), Source: src, FailOnInvalid: true})
	if err != nil {
		t.Fatal(err)
	}
	run, _ = r.Wait(ctx, "org", run.ID)
	if run.State != RunFailed || run.ErrorCode != CodeRunInvalidRows || run.ErrorParams["invalid"] != 1 || run.ErrorParams["duplicates"] != 1 {
		t.Fatalf("corrida: %+v", run)
	}

	_, ex := loadLista(t)
	got := ex.CheckDataset([]RowFacts{{Record: map[string]any{"sku": "A"}}, {Record: map[string]any{"sku": "A"}}})
	if len(got) != 2 || got[0].Item.DetailCode != CodeReviewRepeated || got[0].Item.Detail != "valor repetido en el archivo" {
		t.Fatalf("motivo: %+v", got)
	}
	en := i18n.WithLanguage(ctx, "en")
	if s := got[0].Item.Localize(en); s != "value repeated in the file" {
		t.Fatalf("motivo en = %q", s)
	}
	if s := LocalizeIssue(en, plan.Rows[1].Issues[0]); s != "repeated identity; row 1 is used" {
		t.Fatalf("problema en = %q", s)
	}
}
