package bundle

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// MD0.8 (c): "mismo veredicto" entre el validador v3 y el camino que usan los
// hosts (bundle.Read -> v3.Parse -> manifest.FromV3 -> Manifest.Validate
// legacy). Los tres deben aceptar o rechazar los mismos fixtures; una
// divergencia nueva falla el test. NO cambia el contrato: solo lo observa.

// verdictKernel es la version de kernel con la que se valida el manifest
// legacy proyectado (3.1.0 es lo que exige hub_fleet_v230; el resto acepta cualquier 3.x).
const verdictKernel = "3.1.0"

type verdict struct {
	v3     error // v3.ParseWithOptions estricto
	bundle error // bundle.Read estricto (dual-read del manifest.json)
	legacy error // FromV3(...).Validate; nil si v3 ya rechazo
}

func judge(t *testing.T, raw []byte) verdict {
	t.Helper()
	var v verdict
	m, _, err := v3.ParseWithOptions(raw, v3.Options{Strict: true})
	v.v3 = err
	if err == nil {
		legacy := manifest.FromV3(m)
		v.legacy = legacy.Validate(verdictKernel)
	}
	_, v.bundle = Read(packManifest(t, raw), 1<<26)
	return v
}

func ok(err error) bool { return err == nil }

// knownVerdictGaps: fixtures (o mutaciones) cuyo veredicto difiere entre
// planos. Clave: "<caso>"; valor: por que. Una entrada que ya no diverge
// tambien falla (hay que borrarla).
var knownVerdictGaps = map[string]string{}

// Nota: preset/testdata/pitsline_preset.json no entra aqui: sus *_file
// necesitan los defaults/** dentro del bundle (los cubren preset_files_test.go
// y el paquete preset); empaquetar solo manifest.json lo haria fallar por el
// motivo equivocado.
func fixtureCases(t *testing.T) map[string][]byte {
	t.Helper()
	paths := []string{
		"../docs/spec/v3/examples/addon-example.json",
		"../docs/spec/v3/examples/addon-ui-example.json",
		"../docs/spec/v3/examples/addon-setup-example.json",
		"../docs/spec/v3/examples/addon-connector-blocks-example.json",
		"../docs/spec/v3/examples/preset-example.json",
		"../manifest/v3/testdata/hub_fleet_v230.json",
		"testdata/inventory_v3_manifest.json",
	}
	out := map[string][]byte{}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("fixture %s: %v", p, err)
		}
		out[filepath.Base(p)] = b
	}
	return out
}

// mutate aplica una edicion al JSON del inventory para fabricar casos
// negativos con la misma forma que un manifest real.
func mutate(t *testing.T, raw []byte, f func(m map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	f(m)
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestVerdictParity_V3VsBundleLegacy(t *testing.T) {
	cases := fixtureCases(t)
	inv := cases["inventory_v3_manifest.json"]
	cases["neg:sin-metadata.key"] = mutate(t, inv, func(m map[string]any) {
		delete(m["metadata"].(map[string]any), "key")
	})
	cases["neg:propiedad-desconocida-en-raiz"] = mutate(t, inv, func(m map[string]any) {
		m["campo_inexistente"] = true
	})
	cases["neg:modelo-sin-columnas"] = mutate(t, inv, func(m map[string]any) {
		ms := m["models"].([]any)
		ms[0].(map[string]any)["columns"] = []any{}
	})
	cases["neg:kind-invalido"] = mutate(t, inv, func(m map[string]any) {
		m["kind"] = "Nope"
	})

	seen := map[string]bool{}
	for name, raw := range cases {
		name, raw := name, raw
		t.Run(name, func(t *testing.T) {
			v := judge(t, raw)
			// v3 y bundle comparten el mismo parser: deben coincidir siempre.
			v3OK, bundleOK := ok(v.v3), ok(v.bundle)
			// Tras v3 aceptado, el legacy proyectado debe aceptar tambien.
			diverges := v3OK != bundleOK || (v3OK && !ok(v.legacy))
			if diverges {
				seen[name] = true
				if _, known := knownVerdictGaps[name]; !known {
					t.Errorf("veredicto distinto: v3=%v bundle=%v legacy=%v", v.v3, v.bundle, v.legacy)
				}
			}
			if strings.HasPrefix(name, "neg:") && v3OK {
				t.Errorf("el caso negativo %s fue aceptado por v3", name)
			}
			if !strings.HasPrefix(name, "neg:") && !v3OK {
				t.Errorf("el fixture %s debe ser valido en v3: %v", name, v.v3)
			}
		})
	}
	for name := range knownVerdictGaps {
		if !seen[name] {
			t.Errorf("entrada obsoleta en knownVerdictGaps (ya no diverge): %s", name)
		}
	}
}

// Reglas «gemelas»: el validador legacy (manifest.Manifest.Validate) y v3
// (validateDoc) implementan por separado las mismas reglas que el schema NO
// expresa (expr de formulas, prefijo de hook, placeholder de folio). Estos
// casos PASAN el schema y fallan solo en esas reglas; se compara en ambas
// direcciones sin pasar por el parser estricto de v3:
//   - v3 -> legacy: si v3 rechaza por una regla Go, json.Unmarshal directo a
//     v3.Manifest + FromV3 + Validate tambien debe rechazar.
//   - legacy -> v3: si el legacy rechaza, v3 tambien (y no por el schema).
func TestVerdictParity_TwinRules(t *testing.T) {
	cases := fixtureCases(t)
	inv := cases["inventory_v3_manifest.json"]
	onModel := func(key string, f func(md map[string]any)) []byte {
		return mutate(t, inv, func(m map[string]any) {
			for _, e := range m["models"].([]any) {
				if md := e.(map[string]any); md["key"] == key {
					f(md)
				}
			}
		})
	}
	twins := map[string][]byte{
		"formula-expr-columna-inexistente": onModel("Product", func(md map[string]any) {
			md["formulas"] = []any{map[string]any{"target": "unit_price", "expr": "columna_que_no_existe * 2"}}
		}),
		"formula-expr-sintaxis-invalida": onModel("Product", func(md map[string]any) {
			md["formulas"] = []any{map[string]any{"target": "unit_price", "expr": "unit_price * ("}}
		}),
		// El prefijo de hook (wasm|webhook|compiled) ya lo fija el schema por
		// patron, asi que no es un caso gemelo; lo que el schema no expresa
		// son las referencias cruzadas del hook.
		"hook-to-etapa-inexistente": onModel("Transfer", func(md map[string]any) {
			md["stage_field"] = "status"
			md["stages"] = []any{map[string]any{"key": "draft"}, map[string]any{"key": "done"}}
			md["on_transition"] = []any{map[string]any{"from": "*", "to": "no_existe", "do": "wasm:subir"}}
		}),
		"hook-set-columna-inexistente": onModel("Transfer", func(md map[string]any) {
			md["stage_field"] = "status"
			md["stages"] = []any{map[string]any{"key": "draft"}, map[string]any{"key": "done"}}
			md["on_transition"] = []any{map[string]any{"from": "*", "to": "done", "set": map[string]any{"columna_fantasma": "x"}}}
		}),
		"folio-sin-placeholder": onModel("Product", func(md map[string]any) {
			md["sequences"] = []any{map[string]any{"key": "folio", "format": "INV-"}}
		}),
		"folio-con-dos-placeholders": onModel("Product", func(md map[string]any) {
			md["sequences"] = []any{map[string]any{"key": "folio", "format": "{seq}-{seq:06}"}}
		}),
	}
	for name, raw := range twins {
		name, raw := name, raw
		t.Run(name, func(t *testing.T) {
			_, _, v3Err := v3.ParseWithOptions(raw, v3.Options{Strict: true})
			if v3Err == nil {
				t.Fatalf("v3 acepto el caso gemelo %s: no ejercita ninguna regla", name)
			}
			if strings.Contains(v3Err.Error(), "schema validation failed") || strings.Contains(v3Err.Error(), "decode") {
				t.Fatalf("el caso %s debe PASAR el schema y fallar en una regla Go, pero: %v", name, v3Err)
			}
			// Camino directo: sin schema ni reglas de v3, solo decode + proyeccion + legacy.
			var m v3.Manifest
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatalf("decode directo: %v", err)
			}
			lm := manifest.FromV3(&m)
			legacyErr := lm.Validate(verdictKernel)
			if legacyErr == nil {
				t.Errorf("v3 rechaza (%v) pero el legacy acepta la proyeccion: regla gemela ausente en manifest.Validate", v3Err)
			}
			// Direccion contraria: el legacy rechazo, v3 tambien (ya comprobado arriba);
			// y el control: el inventory sin mutar pasa por los dos.
			if _, _, err := v3.ParseWithOptions(inv, v3.Options{Strict: true}); err != nil {
				t.Fatalf("control: %v", err)
			}
			var ctl v3.Manifest
			if err := json.Unmarshal(inv, &ctl); err != nil {
				t.Fatal(err)
			}
			lc := manifest.FromV3(&ctl)
			if err := lc.Validate(verdictKernel); err != nil {
				t.Fatalf("control legacy: %v", err)
			}
		})
	}
}
