package v3

import (
	"strings"
	"testing"
)

const aiCaps = `"capabilities": [
        {"kind": "ai:complete", "target": "ticket_triage", "reason": "Clasificar tickets entrantes"},
        {"kind": "ai:complete", "target": "reply_draft", "reason": "Borrador de respuesta"}
      ]`

func TestAIDeclarationParses(t *testing.T) {
	m, err := Parse(capabilityManifest("tickets", aiCaps+`,
      "ai": {"monthly_sparks_cap": 300, "features": [
        {"key": "ticket_triage", "max_tier": "fast", "max_tokens": 600, "monthly_sparks_cap": 120, "personal_data": true}
      ]}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if m.AI == nil || m.AI.MonthlySparksCap != 300 || len(m.AI.Features) != 1 || !m.AI.Features[0].PersonalData {
		t.Fatalf("ai block not parsed: %+v", m.AI)
	}
	if g := m.AIGrants(); len(g) != 2 || g[0].Feature != "ticket_triage" || g[0].Purpose == "" {
		t.Fatalf("grants: %+v", g)
	}
	// The ai block is optional: grants alone are valid (host defaults).
	if _, err := Parse(capabilityManifest("tickets", aiCaps)); err != nil {
		t.Fatalf("grants without ai block: %v", err)
	}
}

func TestAIDeclarationRejects(t *testing.T) {
	cases := map[string]struct{ extra, want string }{
		"wildcard feature":      {`"capabilities": [{"kind": "ai:complete", "target": "*", "reason": "x"}]`, "no wildcards"},
		"missing purpose":       {`"capabilities": [{"kind": "ai:complete", "target": "ticket_triage"}]`, "reason is required"},
		"limit without grant":   {aiCaps + `, "ai": {"features": [{"key": "lead_score"}]}`, "no ai:complete capability"},
		"unknown tier (schema)": {aiCaps + `, "ai": {"features": [{"key": "ticket_triage", "max_tier": "ultra"}]}`, "max_tier"},
		"negative cap (schema)": {aiCaps + `, "ai": {"monthly_sparks_cap": -1}`, "monthly_sparks_cap"},
		"feature above addon":   {aiCaps + `, "ai": {"monthly_sparks_cap": 10, "features": [{"key": "ticket_triage", "monthly_sparks_cap": 20}]}`, "exceeds"},
		"unknown ai field":      {aiCaps + `, "ai": {"model": "gpt"}`, "model"},
		"addon provides ai":     {`"provides_capabilities": [{"key": "ai.complete", "handler": {"type": "wasm", "function": "complete"}}]`, "implemented by the host"},
	}
	for name, tc := range cases {
		_, err := Parse(capabilityManifest("tickets", tc.extra))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want error containing %q, got %v", name, tc.want, err)
		}
	}
}
