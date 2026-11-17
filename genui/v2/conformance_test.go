package genuiv2

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// Shared vectors with @asteby/metacore-genui (conformance/README.md).
type vector struct {
	Description string `json:"description"`
	Input       any    `json:"input"`
	Options     struct {
		AllowExternalLinks bool `json:"allowExternalLinks"`
	} `json:"options"`
	Expect struct {
		OK     bool   `json:"ok"`
		Code   string `json:"code"`
		Output any    `json:"output"`
	} `json:"expect"`
}

func loadVectors(t *testing.T, kind string) map[string]vector {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("testdata", "conformance", kind, "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("sin vectores en %s", kind)
	}
	out := map[string]vector{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var v vector
		if err := dec.Decode(&v); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		out[strings.TrimSuffix(filepath.Base(f), ".json")] = v
	}
	return out
}

func names(m map[string]vector) []string {
	n := make([]string, 0, len(m))
	for k := range m {
		n = append(n, k)
	}
	sort.Strings(n)
	return n
}

// canon normalises numbers so json.Number and float64 compare equal.
func canon(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func codeOf(t *testing.T, err error) string {
	t.Helper()
	e, ok := err.(*Error)
	if !ok {
		t.Fatalf("error sin codigo estable: %v", err)
	}
	return e.Code
}

func TestConformanceValid(t *testing.T) {
	for name, v := range loadVectors(t, "valid") {
		t.Run(name, func(t *testing.T) {
			o := Options{AllowExternalLinks: v.Options.AllowExternalLinks}
			if _, err := Parse(v.Input, o); err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if _, err := ParseAny(v.Input, o); err != nil {
				t.Fatalf("ParseAny: %v", err)
			}
		})
	}
}

func TestConformanceInvalid(t *testing.T) {
	for name, v := range loadVectors(t, "invalid") {
		t.Run(name, func(t *testing.T) {
			o := Options{AllowExternalLinks: v.Options.AllowExternalLinks}
			_, err := Parse(v.Input, o)
			if err == nil {
				t.Fatalf("se esperaba %s y el documento paso", v.Expect.Code)
			}
			if got := codeOf(t, err); got != v.Expect.Code {
				t.Fatalf("codigo %s, se esperaba %s (%v)", got, v.Expect.Code, err)
			}
			if _, err := ParseAny(v.Input, o); err == nil {
				t.Fatal("ParseAny acepto un documento invalido")
			}
		})
	}
}

func TestConformanceUpgrade(t *testing.T) {
	for name, v := range loadVectors(t, "upgrade") {
		t.Run(name, func(t *testing.T) {
			o := Options{AllowExternalLinks: v.Options.AllowExternalLinks}
			r, err := ParseAny(v.Input, o)
			if !v.Expect.OK {
				if err == nil {
					t.Fatalf("se esperaba %s", v.Expect.Code)
				}
				if got := codeOf(t, err); got != v.Expect.Code {
					t.Fatalf("codigo %s, se esperaba %s (%v)", got, v.Expect.Code, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseAny: %v", err)
			}
			if r.Source != VersionV1 {
				t.Fatalf("source %s", r.Source)
			}
			if !reflect.DeepEqual(canon(t, r.Doc), canon(t, v.Expect.Output)) {
				t.Fatalf("salida distinta:\n got %v\nwant %v", canon(t, r.Doc), canon(t, v.Expect.Output))
			}
			if _, err := Parse(r.Doc, o); err != nil {
				t.Fatalf("el v2 resultante no valida: %v", err)
			}
		})
	}
}

func TestConformanceDowngrade(t *testing.T) {
	for name, v := range loadVectors(t, "downgrade") {
		t.Run(name, func(t *testing.T) {
			in, _ := v.Input.(map[string]any)
			var caps *Caps
			if c, ok := in["caps"]; ok {
				raw, _ := json.Marshal(c)
				caps = &Caps{}
				if err := json.Unmarshal(raw, caps); err != nil {
					t.Fatal(err)
				}
			}
			spec := in["spec"]
			var doc Doc
			if m, ok := spec.(map[string]any); ok {
				doc = m
			}
			r, err := Downgrade(doc, caps)
			if !v.Expect.OK {
				if err == nil {
					t.Fatalf("se esperaba %s", v.Expect.Code)
				}
				if got := codeOf(t, err); got != v.Expect.Code {
					t.Fatalf("codigo %s, se esperaba %s (%v)", got, v.Expect.Code, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Downgrade: %v", err)
			}
			if !reflect.DeepEqual(canon(t, r.Spec), canon(t, v.Expect.Output)) {
				t.Fatalf("salida distinta:\n got %v\nwant %v", canon(t, r.Spec), canon(t, v.Expect.Output))
			}
		})
	}
}

// El codigo de error no depende del orden de las claves: se recorren ordenadas por bytes UTF-8.
func TestInvalidIsDeterministic(t *testing.T) {
	for name, v := range loadVectors(t, "invalid") {
		if _, isStr := v.Input.(string); isStr {
			continue
		}
		for i := 0; i < 5; i++ {
			_, err := Parse(v.Input, Options{AllowExternalLinks: v.Options.AllowExternalLinks})
			if err == nil || codeOf(t, err) != v.Expect.Code {
				t.Fatalf("%s: codigo inestable (%v)", name, err)
			}
		}
	}
}

// Cada codigo estable tiene al menos un vector invalid, upgrade o downgrade.
func TestEveryCodeHasAVector(t *testing.T) {
	covered := map[string]bool{}
	for _, kind := range []string{"invalid", "upgrade", "downgrade"} {
		for _, v := range loadVectors(t, kind) {
			if v.Expect.Code != "" {
				covered[v.Expect.Code] = true
			}
		}
	}
	for _, c := range AllCodes() {
		if !covered[c] {
			t.Errorf("el codigo %s no tiene vector", c)
		}
	}
}

// Los archivos del esquema y de los vectores deben coincidir con el lock que escribe scripts/sync-genui-vectors.sh.
func TestVectorsMatchLock(t *testing.T) {
	var files []string
	for _, root := range []string{"schema", filepath.Join("testdata", "conformance")} {
		_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() && strings.HasSuffix(p, ".json") {
				files = append(files, filepath.ToSlash(p))
			}
			return nil
		})
	}
	sort.Strings(files)
	var lines strings.Builder
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(raw)
		lines.WriteString(hex.EncodeToString(sum[:]) + "  " + f + "\n")
	}
	total := sha256.Sum256([]byte(lines.String()))
	lock, err := os.ReadFile(filepath.Join("testdata", "conformance.lock"))
	if err != nil {
		t.Fatal(err)
	}
	want := hex.EncodeToString(total[:])
	if !strings.Contains(string(lock), "content_sha256 "+want) {
		t.Fatalf("los vectores/esquema no coinciden con conformance.lock (calculado %s). Corre scripts/sync-genui-vectors.sh desde el SDK; no edites estos archivos a mano.\n%s", want, lock)
	}
}

func TestLimitsAndHelpers(t *testing.T) {
	if MaxBytes != 128*1024 || MaxDepth != 6 || MaxBlocks != 200 {
		t.Fatal("limites del contrato cambiados")
	}
	if len(SchemaJSON()) == 0 {
		t.Fatal("esquema vacio")
	}
	if CodeOf(nil) != CodeInvalidDocument {
		t.Fatal("CodeOf(nil)")
	}
}

func TestParseInputForms(t *testing.T) {
	text := `{"version":"genui/v2","blocks":[{"type":"text","text":"hola <b>"}]}`
	for _, in := range []any{text, []byte(text), map[string]any{"version": "genui/v2", "blocks": []any{map[string]any{"type": "text", "text": "hola <b>"}}}} {
		if _, err := Parse(in, Options{}); err != nil {
			t.Fatalf("%T: %v", in, err)
		}
	}
	type typed struct {
		Version string `json:"version"`
		Blocks  []any  `json:"blocks"`
	}
	if _, err := Parse(typed{Version: "genui/v2", Blocks: []any{map[string]any{"type": "text", "text": "x"}}}, Options{}); err != nil {
		t.Fatalf("struct: %v", err)
	}
	if _, err := Parse(`{"version":"genui/v2","blocks":[`, Options{}); CodeOf(err) != CodeInvalidJSON {
		t.Fatalf("json roto: %v", err)
	}
	if _, err := Parse(`{"version":"genui/v2","blocks":[{"type":"text","text":"x"}]} {}`, Options{}); CodeOf(err) != CodeInvalidJSON {
		t.Fatalf("datos tras el documento: %v", err)
	}
}
