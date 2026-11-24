package manifest_test

import (
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/capability"
	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

func aiManifest() manifest.Manifest {
	return manifest.Manifest{
		Key: "tickets", Name: "Tickets", Version: "1.0.0", Kernel: ">=2.0.0 <3.0.0",
		Capabilities: []manifest.Capability{
			{Kind: "ai:complete", Target: "ticket_triage", Reason: "Clasificar tickets"},
			{Kind: "ai:complete", Target: "reply_draft", Reason: "Borrador"},
		},
		AI: &capability.AISpec{MonthlySparksCap: 300, Features: []capability.AIFeatureSpec{
			{Key: "ticket_triage", MaxTier: "fast", MaxTokens: 600, MonthlySparksCap: 120, PersonalData: true},
		}},
	}
}

func TestAIManifestValidateAndPolicy(t *testing.T) {
	m := aiManifest()
	if err := m.Validate("2.0.0"); err != nil {
		t.Fatalf("valid ai manifest rejected: %v", err)
	}
	p := m.AIPolicy()
	f, ok := p.Feature("ticket_triage")
	if p.Addon != "tickets" || p.MonthlySparksCap != 300 || !ok || f.MaxTier != "fast" || f.MaxTokens != 600 || f.MonthlySparksCap != 120 || !f.PersonalData || f.Purpose != "Clasificar tickets" {
		t.Fatalf("policy: %+v", p)
	}
	if f, ok := p.Feature("reply_draft"); !ok || f.MaxTier != "" || f.MonthlySparksCap != 0 {
		t.Fatalf("a feature without limits keeps zero limits (host defaults): %+v", f)
	}
	if got := p.FeatureKeys(); strings.Join(got, ",") != "reply_draft,ticket_triage" {
		t.Fatalf("feature keys %v", got)
	}
}

func TestAIManifestValidateRejects(t *testing.T) {
	m := aiManifest()
	m.Capabilities[0].Reason = ""
	if err := m.Validate("2.0.0"); err == nil || !strings.Contains(err.Error(), "reason is required") {
		t.Fatalf("missing purpose must fail, got %v", err)
	}
	m = aiManifest()
	m.AI.Features = append(m.AI.Features, capability.AIFeatureSpec{Key: "lead_score"})
	if err := m.Validate("2.0.0"); err == nil || !strings.Contains(err.Error(), "no ai:complete capability") {
		t.Fatalf("limit without grant must fail, got %v", err)
	}
	m = aiManifest()
	m.ProvidesCapabilities = []manifest.CapabilityProviderDef{{Key: capability.AIComplete}}
	if err := m.Validate("2.0.0"); err == nil || !strings.Contains(err.Error(), "implemented by the host") {
		t.Fatalf("providing ai.complete must fail, got %v", err)
	}
}

func TestAIFromV3(t *testing.T) {
	raw := []byte(`{
      "apiVersion": "asteby.com/v3", "kind": "Addon",
      "metadata": {"key": "tickets", "name": "Tickets", "version": "1.0.0"},
      "compatibility": {"requires": [{"key": "kernel", "version": ">=3.0.0 <4.0.0"}]},
      "capabilities": [{"kind": "ai:complete", "target": "ticket_triage", "reason": "Clasificar"}],
      "ai": {"monthly_sparks_cap": 50, "features": [{"key": "ticket_triage", "max_tier": "fast"}]}
    }`)
	vm, err := v3.Parse(raw)
	if err != nil {
		t.Fatalf("v3.Parse: %v", err)
	}
	m := manifest.FromV3(vm)
	if m.AI == nil || m.AI.MonthlySparksCap != 50 || m.AI.Features[0].MaxTier != "fast" {
		t.Fatalf("ai block not projected: %+v", m.AI)
	}
	if p := m.AIPolicy(); len(p.Features) != 1 || p.Features["ticket_triage"].Purpose != "Clasificar" {
		t.Fatalf("policy from v3: %+v", p)
	}
}
