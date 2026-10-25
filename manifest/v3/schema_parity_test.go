package v3

import (
	"bytes"
	"encoding/json"
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
// "<TipoGo>: go-only <tag>" (campo Go sin propiedad en el schema) o
// "<TipoGo>: schema-only <prop>" (propiedad del schema sin tag Go). El valor
// explica por que. El test falla tanto si aparece una diferencia nueva como
// si una entrada queda obsoleta (hay que borrarla al arreglar el schema).
var knownParityGaps = map[string]string{
	// BUG ABIERTO (auditoria B1): Model.Import existe en Go (types.go,
	// *ImportSpec, 2026-07-27) pero $defs.Model del schema tiene
	// additionalProperties:false y no declara "import": ningun manifest
	// puede usarlo (0 addons lo usan; la feature nunca llego). Se corrige
	// agregando "import" al schema en MD1/R2, no aqui; al hacerlo, borra
	// esta entrada (el test falla por obsoleta).
	"Manifest.models: go-only import": "BUG ABIERTO B1: Model.Import sin propiedad en el schema",

	// Handler es un solo tipo Go (union de todos los tipos de handler) que
	// el schema estrecha por sitio de uso. Estas son deuda conocida de
	// forma, no un bug de un campo: el schema de estos sitios solo admite
	// una parte de los campos de Handler.
	// Manifest.contributions.tools.handler: el schema de tool.handler solo
	// admite parte de Handler. Es deuda de FORMA, no un bug de un campo: Go
	// acepta estos campos pero validateHubActions
	// (manifest/v3/hub_primitives_validate.go:276-300) los verifica/rechaza
	// por tipo de handler. Se resuelve en MD3/R2 (estrechar Handler por sitio).
	"Manifest.contributions.tools.handler: go-only method":      "schema de tool.handler no admite este campo de Handler (validado por validateHubActions; MD3/R2)",
	"Manifest.contributions.tools.handler: go-only on_success":  "schema de tool.handler no admite este campo de Handler (validado por validateHubActions; MD3/R2)",
	"Manifest.contributions.tools.handler: go-only path":        "schema de tool.handler no admite este campo de Handler (validado por validateHubActions; MD3/R2)",
	"Manifest.contributions.tools.handler: go-only retries":     "schema de tool.handler no admite este campo de Handler (validado por validateHubActions; MD3/R2)",
	"Manifest.contributions.tools.handler: go-only send_record": "schema de tool.handler no admite este campo de Handler (validado por validateHubActions; MD3/R2)",
	"Manifest.contributions.tools.handler: go-only set":         "schema de tool.handler no admite este campo de Handler (validado por validateHubActions; MD3/R2)",
	"Manifest.contributions.tools.handler: go-only signing":     "schema de tool.handler no admite este campo de Handler (validado por validateHubActions; MD3/R2)",
	"Manifest.contributions.tools.handler: go-only timeout_ms":  "schema de tool.handler no admite este campo de Handler (validado por validateHubActions; MD3/R2)",
	"Manifest.contributions.tools.handler: go-only url_ref":     "schema de tool.handler no admite este campo de Handler (validado por validateHubActions; MD3/R2)",
	"Manifest.contributions.tools.handler: go-only url_setting": "schema de tool.handler no admite este campo de Handler (validado por validateHubActions; MD3/R2)",

	"Manifest.contributions.subscriptions.handler: go-only method":      "schema de subscription.handler no admite este campo de Handler",
	"Manifest.contributions.subscriptions.handler: go-only on_success":  "schema de subscription.handler no admite este campo de Handler",
	"Manifest.contributions.subscriptions.handler: go-only path":        "schema de subscription.handler no admite este campo de Handler",
	"Manifest.contributions.subscriptions.handler: go-only retries":     "schema de subscription.handler no admite este campo de Handler",
	"Manifest.contributions.subscriptions.handler: go-only send_record": "schema de subscription.handler no admite este campo de Handler",
	"Manifest.contributions.subscriptions.handler: go-only set":         "schema de subscription.handler no admite este campo de Handler",
	"Manifest.contributions.subscriptions.handler: go-only signing":     "schema de subscription.handler no admite este campo de Handler",
	"Manifest.contributions.subscriptions.handler: go-only timeout_ms":  "schema de subscription.handler no admite este campo de Handler",
	"Manifest.contributions.subscriptions.handler: go-only url_ref":     "schema de subscription.handler no admite este campo de Handler",
	"Manifest.contributions.subscriptions.handler: go-only url_setting": "schema de subscription.handler no admite este campo de Handler",
	"Manifest.provides_capabilities.handler: go-only capability":        "schema de provides_capabilities.handler no admite este campo de Handler",
	"Manifest.provides_capabilities.handler: go-only connector":         "schema de provides_capabilities.handler no admite este campo de Handler",
	"Manifest.provides_capabilities.handler: go-only export":            "schema de provides_capabilities.handler no admite este campo de Handler",
	"Manifest.provides_capabilities.handler: go-only input":             "schema de provides_capabilities.handler no admite este campo de Handler",
	"Manifest.provides_capabilities.handler: go-only method":            "schema de provides_capabilities.handler no admite este campo de Handler",
	"Manifest.provides_capabilities.handler: go-only on_success":        "schema de provides_capabilities.handler no admite este campo de Handler",
	"Manifest.provides_capabilities.handler: go-only path":              "schema de provides_capabilities.handler no admite este campo de Handler",
	"Manifest.provides_capabilities.handler: go-only retries":           "schema de provides_capabilities.handler no admite este campo de Handler",
	"Manifest.provides_capabilities.handler: go-only send_record":       "schema de provides_capabilities.handler no admite este campo de Handler",
	"Manifest.provides_capabilities.handler: go-only set":               "schema de provides_capabilities.handler no admite este campo de Handler",
	"Manifest.provides_capabilities.handler: go-only signing":           "schema de provides_capabilities.handler no admite este campo de Handler",
	"Manifest.provides_capabilities.handler: go-only timeout_ms":        "schema de provides_capabilities.handler no admite este campo de Handler",
	"Manifest.provides_capabilities.handler: go-only url":               "schema de provides_capabilities.handler no admite este campo de Handler",
	"Manifest.provides_capabilities.handler: go-only url_ref":           "schema de provides_capabilities.handler no admite este campo de Handler",
	"Manifest.provides_capabilities.handler: go-only url_setting":       "schema de provides_capabilities.handler no admite este campo de Handler",
}

func TestGoSchemaParity(t *testing.T) {
	var root map[string]any
	if err := json.Unmarshal(schemaBytes, &root); err != nil {
		t.Fatal(err)
	}
	w := &parityWalker{skipped: map[string]bool{}, root: root, seen: map[string]bool{}, found: map[string]bool{}, compared: map[string]bool{}}
	w.walk(reflect.TypeOf(Manifest{}), root, "Manifest")

	// Raiz extra: Branding vive en $defs.Preset.defaults.branding (el campo
	// Go es map[string]any, asi que el recorrido desde Manifest no lo ve).
	if br := dig(root, "$defs", "Preset", "properties", "defaults", "properties", "branding"); br != nil {
		w.walk(reflect.TypeOf(Branding{}), br, "Branding")
	} else {
		t.Error("el schema ya no tiene $defs.Preset.defaults.branding: actualiza la raiz extra Branding del test")
	}

	// Tipos con UnmarshalJSON propio: la forma del cable no la describen los
	// tags, asi que cada uno debe tener un recorrido explicito en wireForms.
	// Si aparece uno nuevo sin registrar, el test falla.
	var unwired []string
	for name := range w.skipped {
		if _, ok := wireForms[name]; !ok {
			unwired = append(unwired, name)
		}
	}
	sort.Strings(unwired)
	if len(unwired) > 0 {
		t.Errorf("tipos con UnmarshalJSON propio sin recorrido en wireForms (registralos en schema_parity_test.go): %v", unwired)
	}

	// Cobertura visible: $defs del schema nunca comparados contra un struct Go.
	defs, _ := root["$defs"].(map[string]any)
	var unvisited []string
	for name := range defs {
		if !w.compared[name] {
			unvisited = append(unvisited, name)
		}
	}
	sort.Strings(unvisited)
	t.Logf("cobertura: %d de %d $defs comparados contra un struct Go; sin comparar (%d): %v",
		len(defs)-len(unvisited), len(defs), len(unvisited), unvisited)
	t.Logf("tipos con UnmarshalJSON propio (recorrido via wireForms): %v", w.skipped)
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

var jsonUnmarshaler = reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()

func hasCustomJSON(t reflect.Type) bool {
	return t.Implements(jsonUnmarshaler) || reflect.PointerTo(t).Implements(jsonUnmarshaler)
}

func dig(n map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		next, ok := n[k].(map[string]any)
		if !ok {
			return nil
		}
		n = next
	}
	return n
}

// wireForms describe, por tipo Go con UnmarshalJSON propio, contra que partes
// del schema se compara. Recibe el nodo del schema ya resuelto del campo.
var wireForms map[string]func(w *parityWalker, sn map[string]any, path string)

// init evita el ciclo de inicializacion wireForms -> walk -> wireForms.
func init() {
	wireForms = map[string]func(w *parityWalker, sn map[string]any, path string){
		// options: array de FieldOption (estatico) u objeto DynamicOptions.
		"FieldOptions": func(w *parityWalker, sn map[string]any, path string) {
			for _, br := range w.branches(sn) {
				switch br["type"] {
				case "array":
					w.walk(reflect.TypeOf(FieldOption{}), br, path+"[]")
				case "object":
					w.walk(reflect.TypeOf(DynamicOptions{}), br, path)
				}
			}
		},
		// lines: boolean u objeto (documentFormLinesAlias = mismos campos sin el
		// UnmarshalJSON).
		// option_filter: una regla o una lista de reglas (OptionFilter es []OptionFilterRule).
		"OptionFilter": func(w *parityWalker, sn map[string]any, path string) {
			for _, br := range w.branches(sn) {
				switch br["type"] {
				case "array", "object":
					w.walk(reflect.TypeOf(OptionFilterRule{}), br, path)
				}
			}
		},
		"DocumentFormLines": func(w *parityWalker, sn map[string]any, path string) {
			for _, br := range w.branches(sn) {
				if br["type"] == "object" {
					w.walk(reflect.TypeOf(documentFormLinesAlias{}), br, path)
				}
			}
		},
	}
}

// branches devuelve las ramas oneOf/anyOf resueltas del nodo (o el propio nodo).
func (w *parityWalker) branches(n map[string]any) []map[string]any {
	n = w.resolve(n)
	for _, key := range []string{"oneOf", "anyOf"} {
		if arr, ok := n[key].([]any); ok {
			var out []map[string]any
			for _, e := range arr {
				if em, ok := e.(map[string]any); ok {
					r := w.resolve(em)
					if w.lastRef != "" {
						w.compared[w.lastRef] = true
					}
					out = append(out, r)
				}
			}
			return out
		}
	}
	return []map[string]any{n}
}

type parityWalker struct {
	compared map[string]bool // $defs comparados contra un struct Go
	lastRef  string          // ultimo $defs resuelto por resolve
	skipped  map[string]bool
	root     map[string]any
	seen     map[string]bool
	found    map[string]bool
}

func (w *parityWalker) resolve(n map[string]any) map[string]any {
	w.lastRef = ""
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
		w.lastRef = strings.TrimPrefix(ref, "#/$defs/")
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
	if w.lastRef != "" {
		w.compared[w.lastRef] = true // rama allOf/anyOf/oneOf nombrada (p. ej. anyOf[$ref, null])
	}
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
	// Detecta UnmarshalJSON propio en cada capa (tipo con nombre, puntero,
	// slice, map): OptionFilter es un slice con decodificador propio.
	for t := gt; ; t = t.Elem() {
		if t.Name() != "" && hasCustomJSON(t) && t.Kind() == reflect.Struct {
			break
		}
		if t.Name() != "" && t.PkgPath() == "github.com/asteby/metacore-kernel/manifest/v3" && hasCustomJSON(t) {
			w.skipped[t.Name()] = true // slice/map con decodificador propio
			if f, ok := wireForms[t.Name()]; ok {
				w.resolve(sn)
				if w.lastRef != "" {
					w.compared[w.lastRef] = true
				}
				f(w, sn, path)
				return
			}
			break
		}
		if k := t.Kind(); k != reflect.Ptr && k != reflect.Slice && k != reflect.Array && k != reflect.Map {
			break
		}
	}
	gt = unwrapGo(gt)
	if gt.Kind() != reflect.Struct {
		return
	}
	if hasCustomJSON(gt) {
		// Forma en el cable distinta de los tags del struct (p. ej.
		// FieldOptions: array o {source,...}); los tags no la describen.
		w.skipped[gt.Name()] = true
		w.resolve(sn)
		if w.lastRef != "" {
			w.compared[w.lastRef] = true
		}
		if f, ok := wireForms[gt.Name()]; ok {
			f(w, sn, path)
		}
		return
	}
	sn = w.child(sn)
	def := w.lastRef

	sprops := map[string]map[string]any{}
	w.props(sn, sprops, 0)
	if len(sprops) == 0 {
		// Nodo sin "properties" (p. ej. objeto libre): nada que comparar.
		return
	}
	// Guarda de ANCESTROS (no dedupe global): un par (tipo Go, forma del
	// nodo) ya presente en la rama actual no se vuelve a abrir, asi los
	// tipos recursivos terminan; pero el mismo par en ramas hermanas se
	// compara en cada sitio (si no, un Handler ya visto ocultaria las
	// diferencias de los demas sitios de uso).
	names := make([]string, 0, len(sprops))
	for k := range sprops {
		names = append(names, k)
	}
	sort.Strings(names)
	if def != "" {
		w.compared[def] = true
	}
	vk := gt.PkgPath() + "." + gt.Name() + "|" + strings.Join(names, ",")
	if w.seen[vk] {
		return
	}
	w.seen[vk] = true
	defer delete(w.seen, vk)

	// Se etiqueta por ruta: un mismo tipo Go (Handler) se reutiliza en
	// sitios con schemas distintos (action/tool/subscription/setup).
	label := path
	gprops := map[string]reflect.Type{}
	goTags(gt, gprops)
	tags := make([]string, 0, len(gprops))
	for k := range gprops {
		tags = append(tags, k)
	}
	sort.Strings(tags)

	for _, tag := range tags {
		if _, ok := sprops[tag]; !ok {
			w.found[label+": go-only "+tag] = true
		}
	}
	for _, prop := range names {
		if _, ok := gprops[prop]; !ok {
			w.found[label+": schema-only "+prop] = true
		}
	}
	for _, tag := range tags {
		if sp, ok := sprops[tag]; ok {
			w.walk(gprops[tag], sp, path+"."+tag)
		}
	}
}
