package manifest_test

import (
	"os"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

func parseBlocksExample(t *testing.T) *manifest.Manifest {
	t.Helper()
	raw, err := os.ReadFile("../docs/spec/v3/examples/addon-connector-blocks-example.json")
	if err != nil {
		t.Fatal(err)
	}
	m, err := v3.Parse(raw)
	if err != nil {
		t.Fatalf("v3.Parse: %v", err)
	}
	out := manifest.FromV3(m)
	return &out
}

// The host projection carries the blocks verbatim (the host computes the
// webhook URL and runs the actions from them).
func TestFromV3ProjectsConnectorBlocks(t *testing.T) {
	out := parseBlocksExample(t)
	fl := out.Connectors[0].FormLayout
	if fl == nil || len(fl.Sections) != 2 || len(fl.Sections[1].Blocks) != 2 {
		t.Fatalf("blocks not projected: %+v", fl)
	}
	if b := fl.Sections[1].Blocks[0]; b.Kind != v3.FormBlockWebhookURL || b.Webhook != "events" {
		t.Fatalf("webhook block: %+v", b)
	}
}

// Action-block exports join backend.exports like a test_export, so the runtime
// whitelist lets the host invoke them.
func TestFromV3BlockExportsJoinBackendExports(t *testing.T) {
	out := parseBlocksExample(t)
	if out.Backend == nil {
		t.Fatal("expected a derived wasm backend")
	}
	have := map[string]bool{}
	for _, e := range out.Backend.Exports {
		have[e] = true
	}
	for _, want := range []string{"handle_connector_test", "handle_setup_test_order"} {
		if !have[want] {
			t.Errorf("backend.exports lacks %q: %v", want, out.Backend.Exports)
		}
	}
}

// Dual validation: the legacy plane accepts the example and rejects what the
// v3 plane rejects.
func TestLegacyValidateConnectorBlocks(t *testing.T) {
	out := parseBlocksExample(t)
	if err := out.Validate("3.1.0"); err != nil {
		t.Fatalf("legacy validation rejected a v3-valid manifest: %v", err)
	}
	bad := parseBlocksExample(t)
	bad.Connectors[0].FormLayout.Sections[1].Blocks[0].Webhook = "missing"
	if err := bad.Validate("3.1.0"); err == nil || !strings.Contains(err.Error(), `webhook "missing"`) {
		t.Fatalf("legacy validator accepted an unknown webhook: %v", err)
	}
	dev := parseBlocksExample(t)
	dev.EdgeDevices = []manifest.EdgeDeviceDef{{
		Key: "t", Capabilities: []string{"card_payment"}, Transport: "ws",
		FormLayout: &v3.FormLayout{Mode: "steps", Sections: []v3.FormSection{{
			Key: "p", Blocks: []v3.FormBlock{{Key: "x", Kind: v3.FormBlockInstructions, Description: "d"}},
		}}},
	}}
	if err := dev.Validate("3.1.0"); err == nil || !strings.Contains(err.Error(), "only supported in connectors") {
		t.Fatalf("legacy validator accepted blocks on an edge device: %v", err)
	}
}
