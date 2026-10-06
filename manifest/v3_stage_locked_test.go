package manifest_test

import (
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// Una etapa `locked` (póliza contabilizada) pasa el schema estricto v3 y llega
// al modelo tipado; un tipo inválido se rechaza.
func TestParse_StageLocked(t *testing.T) {
	src := strings.Replace(pipelineManifestJSON,
		`"order": 3, "is_final": true }`, `"order": 3, "is_final": true, "locked": true }`, 1)
	m, err := v3.Parse([]byte(src))
	if err != nil {
		t.Fatalf("v3.Parse rejected stages[].locked: %v", err)
	}
	st := m.Models[0].Stages
	if !st[3].Locked || st[0].Locked {
		t.Fatalf("locked not carried: %+v", st)
	}
	bad := strings.Replace(pipelineManifestJSON,
		`"order": 3, "is_final": true }`, `"order": 3, "is_final": true, "locked": "yes" }`, 1)
	if _, err := v3.Parse([]byte(bad)); err == nil {
		t.Fatal("a non-boolean locked must be rejected")
	}
}

// FromV3 lleva `locked` al StageDef que el runtime (dynamic.StageMachine) lee.
func TestFromV3_StageLocked(t *testing.T) {
	src := strings.Replace(pipelineManifestJSON,
		`"order": 3, "is_final": true }`, `"order": 3, "is_final": true, "locked": true }`, 1)
	m, err := v3.Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	host := manifest.FromV3(m)
	for _, def := range host.ModelDefinitions {
		if len(def.Stages) == 4 {
			if !def.Stages[3].Locked || def.Stages[2].Locked {
				t.Fatalf("host stages = %+v", def.Stages)
			}
			return
		}
	}
	t.Fatal("stage machine not projected")
}
