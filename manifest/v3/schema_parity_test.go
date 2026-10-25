package v3

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// Paridad Go <-> JSON Schema del contrato v3 (MD0.8 de la auditoria de
// metadatos). Go (types.go) y el schema se escriben a mano por separado; un
// campo puede existir en un plano y no en el otro (caso real: Model.Import,
// que el schema con additionalProperties:false rechaza). Estos tests NO
// cambian el contrato: fijan el estado actual y fallan ante deriva nueva.

const (
	schemaCopyA = "schema/manifest-v3.schema.json"
	schemaCopyB = "../../docs/spec/v3/manifest-v3.schema.json"
)

// TestSchemaCopiesIdentical: las dos copias fisicas del schema deben ser
// byte a byte iguales (la embebida en el paquete y la publicada en docs/).
func TestSchemaCopiesIdentical(t *testing.T) {
	a, err := os.ReadFile(schemaCopyA)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(schemaCopyB)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("%s y %s difieren: edita una y copia a la otra (cp %s %s)", schemaCopyA, schemaCopyB, schemaCopyA, schemaCopyB)
	}
	if !bytes.Equal(a, schemaBytes) {
		t.Fatalf("el schema embebido no coincide con %s", schemaCopyA)
	}
}

// knownParityGaps es la allowlist inicial de diferencias conocidas. Clave:
// "<ruta>: go-only <tag>" (campo Go sin propiedad en el schema) o
// "<ruta>: schema-only <prop>" (propiedad del schema sin tag Go). El valor
// explica por que. El test falla tanto si aparece una diferencia nueva como
// si una entrada queda obsoleta (hay que borrarla al arreglar el schema).
var knownParityGaps = map[string]string{}

func TestGoSchemaParity(t *testing.T) {
	var root map[string]any
	if err := json.Unmarshal(schemaBytes, &root); err != nil {
		t.Fatal(err)
	}
	w := &parityWalker{root: root, seen: map[string]bool{}, found: map[string]bool{}}
	w.walk(reflect.TypeOf(Manifest{}), root, "Manifest")

	var fresh []string
	for k := range w.found {
		if _, ok := knownParityGaps[k]; !ok {
			fresh = append(fresh, k)
		}
	}
	var stale []string
	for k := range knownParityGaps {
		if !w.found[k] {
			stale = append(stale, k)
		}
	}
	sort.Strings(fresh)
	sort.Strings(stale)
	if len(fresh) > 0 {
		t.Errorf("deriva Go<->schema NUEVA (arregla el schema/types.go o, si es deuda conocida, anotala en knownParityGaps):\n  %s", strings.Join(fresh, "\n  "))
	}
	if len(stale) > 0 {
		t.Errorf("entradas obsoletas en knownParityGaps (ya hay paridad, borralas):\n  %s", strings.Join(stale, "\n  "))
	}
}

type parityWalker struct {
	root  map[string]any
	seen  map[string]bool
	found map[string]bool
}

func (w *parityWalker) resolve(n map[string]any) map[string]any {
	for i := 0; i < 8; i++ {
		ref, _ := n["$ref"].(string)
		if !strings.HasPrefix(ref, "#/$defs/") {
			return n
		}
		defs, _ := w.root["$defs"].(map[string]any)
		next, ok := defs[strings.TrimPrefix(ref, "#/$defs/")].(map[string]any)
		if !ok {
			return n
		}
		// $ref con hermanos: se fusionan (hermanos ganan).
		merged := map[string]any{}
		for k, v := range next {
			merged[k] = v
		}
		for k, v := range n {
			if k != "$ref" {
				merged[k] = v
			}
		}
		n = merged
	}
	return n
}

// props devuelve la union de propiedades del nodo, incluyendo las de
// allOf/anyOf/oneOf/if/then/else (el schema condiciona por kind).
func (w *parityWalker) props(n map[string]any, out map[string]map[string]any, depth int) {
	if depth > 8 {
		return
	}
	n = w.resolve(n)
	if p, ok := n["properties"].(map[string]any); ok {
		for k, v := range p {
			if vm, ok := v.(map[string]any); ok {
				if _, dup := out[k]; !dup {
					out[k] = vm
				}
			}
		}
	}
	for _, key := range []string{"allOf", "anyOf", "oneOf"} {
		if arr, ok := n[key].([]any); ok {
			for _, e := range arr {
				if em, ok := e.(map[string]any); ok {
					w.props(em, out, depth+1)
				}
			}
		}
	}
	for _, key := range []string{"if", "then", "else"} {
		if em, ok := n[key].(map[string]any); ok {
			w.props(em, out, depth+1)
		}
	}
}

func goTags(t reflect.Type, out map[string]reflect.Type) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name := strings.Split(tag, ",")[0]
		if f.Anonymous && name == "" {
			ft := f.Type
			if ft.Kind() == reflect.Ptr {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				goTags(ft, out)
			}
			continue
		}
		if name == "" {
			if !f.IsExported() {
				continue
			}
			name = f.Name
		}
		out[name] = f.Type
	}
}

func unwrapGo(t reflect.Type) reflect.Type {
	for {
		switch t.Kind() {
		case reflect.Ptr, reflect.Slice, reflect.Array, reflect.Map:
			t = t.Elem()
		default:
			return t
		}
	}
}

// child baja un nivel del schema: items / additionalProperties (objeto) / el
// propio nodo. Devuelve nil si no hay forma segura de seguir.
func (w *parityWalker) child(n map[string]any) map[string]any {
	n = w.resolve(n)
	if it, ok := n["items"].(map[string]any); ok {
		return w.child(it)
	}
	if ap, ok := n["additionalProperties"].(map[string]any); ok {
		return w.child(ap)
	}
	return n
}

func (w *parityWalker) walk(gt reflect.Type, sn map[string]any, path string) {
	gt = unwrapGo(gt)
	if gt.Kind() != reflect.Struct {
		return
	}
	sn = w.child(sn)
	key := gt.PkgPath() + "." + gt.Name() + "@" + path
	if gt.Name() != "" {
		key = gt.PkgPath() + "." + gt.Name() + "|" + fmt.Sprint(sn["$id"], sn["title"], len(sn))
	}
	if w.seen[key+"|"+path] {
		return
	}
	w.seen[key+"|"+path] = true

	sprops := map[string]map[string]any{}
	w.props(sn, sprops, 0)
	if len(sprops) == 0 {
		// Nodo sin "properties" (p. ej. objeto libre): nada que comparar.
		return
	}
	gprops := map[string]reflect.Type{}
	goTags(gt, gprops)

	for tag := range gprops {
		if _, ok := sprops[tag]; !ok {
			w.found[path+": go-only "+tag] = true
		}
	}
	for prop := range sprops {
		if _, ok := gprops[prop]; !ok {
			w.found[path+": schema-only "+prop] = true
		}
	}
	for tag, ft := range gprops {
		if sp, ok := sprops[tag]; ok {
			w.walk(ft, sp, path+"."+tag)
		}
	}
}
