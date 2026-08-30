package manifest_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// agentCapabilitiesManifestJSON mirrors the real shape packages/pos ships:
// one "guide" capability whose walkthrough the host copilot (Aby) drives,
// including a step that auto-advances on a DOM click.
const agentCapabilitiesManifestJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "pos", "name": "Punto de Venta", "version": "0.24.2" },
  "compatibility": { "requires": [{ "key": "kernel", "version": ">=3.0.0 <4.0.0" }] },
  "contributions": {
    "agent_capabilities": [
      {
        "id": "pos.guide.create-sale",
        "title": "Crear una venta",
        "description": "Guía al usuario por el flujo completo de una venta.",
        "kind": "guide",
        "risk": "none",
        "permission": "pos.sale.write",
        "route": "/addons/pos/terminal",
        "guide": {
          "id": "pos.create-sale",
          "title": "Crear una venta",
          "intents": ["como creo una venta", "nueva venta"],
          "steps": [
            { "id": "open-terminal", "route": "/addons/pos/terminal", "target": "pos.product-search", "title": "Elige los productos", "description": "Busca y agrega productos." },
            { "id": "checkout", "route": "/addons/pos/terminal", "target": "pos.checkout", "title": "Continúa al cobro", "description": "Abre el cobro.", "advanceOn": { "event": "click" } }
          ]
        }
      }
    ]
  }
}`

// TestFromV3_AgentCapabilities_Mapped pins the wire contract the frontend
// guidance engine depends on: contributions.agent_capabilities[] must survive
// FromV3 onto the legacy Manifest (what /metacore/manifests serves) and
// marshal back under the "agent_capabilities" key with the v3 field names
// intact. Before this mapping existed the DB row carried the guides but the
// served manifest silently dropped them, so Aby never offered the walkthrough.
func TestFromV3_AgentCapabilities_Mapped(t *testing.T) {
	m, err := v3.Parse([]byte(agentCapabilitiesManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse rejected agent_capabilities manifest: %v", err)
	}
	out := manifest.FromV3(m)

	if len(out.AgentCapabilities) != 1 {
		t.Fatalf("mapped agent_capabilities = %+v, want 1", out.AgentCapabilities)
	}
	cap := out.AgentCapabilities[0]
	if cap.ID != "pos.guide.create-sale" || cap.Kind != "guide" || cap.Risk != "none" {
		t.Fatalf("mapped capability = %+v", cap)
	}
	if cap.Guide == nil || len(cap.Guide.Steps) != 2 {
		t.Fatalf("mapped guide = %+v, want 2 steps", cap.Guide)
	}
	if len(cap.Guide.Intents) != 2 || cap.Guide.Intents[0] != "como creo una venta" {
		t.Fatalf("mapped intents = %+v", cap.Guide.Intents)
	}
	step := cap.Guide.Steps[1]
	if step.Target != "pos.checkout" || step.AdvanceOn == nil || step.AdvanceOn.Event != "click" {
		t.Fatalf("mapped advanceOn step = %+v", step)
	}

	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal legacy manifest: %v", err)
	}
	for _, want := range []string{`"agent_capabilities"`, `"pos.guide.create-sale"`, `"advanceOn":{"event":"click"}`, `"intents":["como creo una venta","nueva venta"]`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("served manifest JSON missing %s:\n%s", want, raw)
		}
	}
}

// TestFromV3_AgentCapabilities_NilContributions guards the nil-safe mapping:
// a manifest with no contributions block (the common case) must map to an
// empty AgentCapabilities without panicking, and must omit the key on the wire.
func TestFromV3_AgentCapabilities_NilContributions(t *testing.T) {
	m, err := v3.Parse([]byte(runtimeManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse: %v", err)
	}
	if m.Contributions != nil {
		t.Fatalf("fixture unexpectedly has contributions; test needs a nil block")
	}
	out := manifest.FromV3(m)
	if out.AgentCapabilities != nil {
		t.Fatalf("agent_capabilities = %+v, want nil for a manifest without contributions", out.AgentCapabilities)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), `"agent_capabilities"`) {
		t.Fatalf("agent_capabilities must be omitted on the wire when empty:\n%s", raw)
	}
}
