package security

import (
	"testing"

	"github.com/asteby/metacore-kernel/capability"
	"github.com/asteby/metacore-kernel/manifest"
)

func TestAICompleteGrant(t *testing.T) {
	c := Compile("tickets", []manifest.Capability{
		{Kind: "ai:complete", Target: "ticket_triage", Reason: "x"},
	})
	if err := c.CanCompleteAI("ticket_triage"); err != nil {
		t.Fatalf("granted feature refused: %v", err)
	}
	if err := c.CanCompleteAI("reply_draft"); err == nil {
		t.Fatal("ungranted feature must be refused")
	}
	if err := c.CanCompleteAI("ticket_*"); err == nil {
		t.Fatal("features are exact, never globs")
	}
	// ai:complete implies capability:query ai.complete (the wasm import).
	if err := c.CanQueryCapability(capability.AIComplete); err != nil {
		t.Fatalf("ai:complete must imply capability:query ai.complete: %v", err)
	}
	if err := c.CanQueryCapability(capability.GeoGeocode); err == nil {
		t.Fatal("ai:complete must not grant other capabilities")
	}
	if got := c.AIFeatures(); len(got) != 1 || got[0] != "ticket_triage" {
		t.Fatalf("AIFeatures = %v", got)
	}
	none := Compile("crm", nil)
	if none.CanQueryCapability(capability.AIComplete) == nil || none.CanCompleteAI("ticket_triage") == nil {
		t.Fatal("no grant, no AI")
	}
	var nilCaps *Capabilities
	if nilCaps.CanCompleteAI("x") == nil || nilCaps.AIFeatures() != nil {
		t.Fatal("nil policy denies")
	}
}
