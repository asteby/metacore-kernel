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
