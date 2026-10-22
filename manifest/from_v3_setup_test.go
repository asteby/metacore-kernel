package manifest_test

import (
	"os"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

func parseSetupExample(t *testing.T) (*v3.Manifest, *manifest.Manifest) {
	t.Helper()
	raw, err := os.ReadFile("../docs/spec/v3/examples/addon-setup-example.json")
	if err != nil {
		t.Fatal(err)
	}
	m, err := v3.Parse(raw)
	if err != nil {
		t.Fatalf("v3.Parse: %v", err)
	}
	out := manifest.FromV3(m)
	return m, &out
}

func TestFromV3ProjectsSetupHealthAndSettingExtras(t *testing.T) {
	_, out := parseSetupExample(t)
	if len(out.Setup) != 1 || out.Setup[0].Trigger != "on_install" || len(out.Setup[0].Steps) != 7 {
		t.Fatalf("setup not projected: %+v", out.Setup)
	}
	if out.Health == nil || out.Health.Handler.Function != "HealthSummary" || out.Health.Permission != "mailer.health.read" {
		t.Fatalf("health not projected: %+v", out.Health)
	}
	pw := out.Settings[1]
	if !pw.WriteOnly || pw.VisibleWhen == nil || pw.VisibleWhen.Field != "dns_mode" || pw.VisibleWhen.Equals != "manual" {
		t.Fatalf("setting extras not projected: %+v", pw)
	}
	if cr := out.Connectors[0].Credentials[1]; !cr.WriteOnly {
		t.Fatalf("credential write_only not projected: %+v", cr)
	}
	if l := out.NativeService.Listeners; len(l) != 3 || l[2].Name != "imaps" {
		t.Fatalf("listeners not projected: %+v", l)
	}
}

// Wasm handlers of setup checks, records and the health probe join the export
// surface ("declared wasm = exists"), exactly like action handlers.
func TestFromV3SetupWasmHandlersJoinBackendExports(t *testing.T) {
	_, out := parseSetupExample(t)
	if out.Backend == nil {
		t.Fatal("expected a derived wasm backend")
	}
	have := map[string]bool{}
	for _, e := range out.Backend.Exports {
		have[e] = true
	}
	for _, want := range []string{"SendEmail", "ApplyDnsCloudflare", "ListRequiredDnsRecords", "VerifyDns", "HealthSummary"} {
		if !have[want] {
			t.Errorf("backend.exports lacks %q: %v", want, out.Backend.Exports)
		}
	}
	// native handlers are NOT wasm exports.
	if have["preflight"] {
		t.Error("native operation leaked into wasm exports")
	}
}

func TestLegacyValidateAcceptsSetupExample(t *testing.T) {
	_, out := parseSetupExample(t)
	if err := out.Validate("3.1.0"); err != nil {
		t.Fatalf("legacy validation rejected a v3-valid manifest: %v", err)
	}
}

// Dual validation: the legacy plane must reject what the v3 plane rejects.
func TestLegacyValidateRejectsBadSetup(t *testing.T) {
	cases := []struct {
		name string
		mut  func(m *manifest.Manifest)
		want string
	}{
		{"cycle", func(m *manifest.Manifest) { m.Setup[0].Steps[0].DependsOn = []string{"dns_check"} }, "cycle"},
		{"unknown dependency", func(m *manifest.Manifest) { m.Setup[0].Steps[1].DependsOn = []string{"ghost"} }, "unknown step"},
		{"unknown setting in form", func(m *manifest.Manifest) { m.Setup[0].Steps[1].Form.Settings = []string{"ghost"} }, "not a setting"},
		{"unknown connector", func(m *manifest.Manifest) { m.Setup[0].Steps[2].Form.Connector = "ghost" }, "not a connector"},
		{"unknown action", func(m *manifest.Manifest) { m.Setup[0].Steps[4].Action.Action = "ghost" }, "contributions.actions"},
		{"wasm export missing from declared backend.exports", func(m *manifest.Manifest) {
			m.Backend.Exports = []string{"SendEmail", "ApplyDnsCloudflare"}
		}, "backend.exports"},
		{"native handler without native service", func(m *manifest.Manifest) { m.NativeService = nil }, "native_service"},
		{"health bad every", func(m *manifest.Manifest) { m.Health.Every = "1s" }, "health.every"},
		{"visible_when unknown sibling", func(m *manifest.Manifest) { m.Settings[1].VisibleWhen.Field = "ghost" }, "not a sibling"},
		{"write_only with default", func(m *manifest.Manifest) { m.Settings[1].DefaultValue = "x" }, "write_only"},
		{"listener duplicate port", func(m *manifest.Manifest) { m.NativeService.Listeners[1].Port = 25 }, "duplicate"},
		{"wildcard egress on web port", func(m *manifest.Manifest) { m.NativeService.Network.Egress = []string{"*:443"} }, "egress"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, out := parseSetupExample(t)
			c.mut(out)
			err := out.Validate("3.1.0")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want error containing %q, got %v", c.want, err)
			}
		})
	}
}
