package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// countingPerms counts permission checks, to prove work is skipped.
type countingPerms struct{ n *int }

func (c countingPerms) Can(_, _ uuid.UUID, _, _ string) bool { *c.n++; return true }

func newPromptCopilot(t *testing.T, clock ClockZone, mut func(*CopilotConfig)) *CopilotService {
	t.Helper()
	cfg := CopilotConfig{
		Orchestrator: NewAgentOrchestrator(OrchestratorConfig{}),
		Brand:        fakeBrand{Personality{Name: "Aby", SystemPrompt: "Eres Aby.", PlatformName: "Pitsline", Addendum: "Habla de llantas."}},
		Clock:        clock,
	}
	if mut != nil {
		mut(&cfg)
	}
	svc, err := NewCopilotService(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

var mx = time.FixedZone("America/Mexico_City", -6*3600)

func TestTodayClauseIsDayPrecise(t *testing.T) {
	got := TodayClause(time.Date(2026, 10, 12, 9, 30, 0, 0, mx), false)
	want := "[Contexto del turno] Hoy es lunes 2026-10-12 (zona America/Mexico_City). "
	if !strings.HasPrefix(got, want) || strings.Contains(got, "09:30") || !strings.Contains(got, "NUNCA inventes la fecha") {
		t.Fatalf("got %q", got)
	}
	if later := TodayClause(time.Date(2026, 10, 12, 23, 59, 0, 0, mx), false); later != got {
		t.Fatal("the clause must only change once a day")
	}
	if withTime := TodayClause(time.Date(2026, 10, 12, 9, 30, 0, 0, mx), true); !strings.Contains(withTime, "lunes 2026-10-12, 09:30") {
		t.Fatalf("time of day: %q", withTime)
	}
}

func TestCopilotSystemPromptIsStableAndClockMovesToTurnContext(t *testing.T) {
	org, user := uuid.New(), uuid.New()
	a := newPromptCopilot(t, fixedClock{time.Date(2026, 10, 12, 9, 30, 0, 0, mx)}, nil)
	b := newPromptCopilot(t, fixedClock{time.Date(2026, 10, 13, 18, 1, 0, 0, mx)}, nil)
	pa, _ := a.SystemPrompt(org, user, false)
	pb, _ := b.SystemPrompt(org, user, false)
	if pa != pb {
		t.Fatalf("system prompt must not depend on the clock:\n%s\n---\n%s", pa, pb)
	}
	if strings.Contains(pa, "2026") || strings.Contains(pa, "Fecha y hora") {
		t.Fatalf("clock in system prompt:\n%s", pa)
	}
	if tc := a.TurnContext(org, user); !strings.Contains(tc, "lunes 2026-10-12") || strings.Contains(tc, "09:30") {
		t.Fatalf("turn context %q", tc)
	}

	legacy := newPromptCopilot(t, fixedClock{time.Date(2026, 10, 12, 9, 30, 0, 0, mx)}, func(c *CopilotConfig) { c.ClockInSystemPrompt = true })
	if p, _ := legacy.SystemPrompt(org, user, false); !strings.Contains(p, "2026-10-12 09:30") {
		t.Fatalf("legacy clock opt-in:\n%s", p)
	}
	if legacy.TurnContext(org, user) != "" {
		t.Fatal("legacy clock must not be sent twice")
	}
}

func TestCopilotCapabilitiesClauseIsOptIn(t *testing.T) {
	checks := 0
	crud, err := NewCRUDService(CRUDConfig{Catalog: testCatalog(), Permissions: countingPerms{&checks}})
	if err != nil {
		t.Fatal(err)
	}
	caps := &CapabilityRegistry{CRUD: crud}
	org, user := uuid.New(), uuid.New()

	off := newPromptCopilot(t, nil, func(c *CopilotConfig) { c.Capabilities = caps })
	if p, _ := off.SystemPrompt(org, user, false); strings.Contains(p, "REGISTRO AUTORITATIVO") {
		t.Fatal("capabilities must be off by default")
	}
	if checks != 0 {
		t.Fatalf("capabilities computed although not injected (%d permission checks)", checks)
	}

	on := newPromptCopilot(t, nil, func(c *CopilotConfig) { c.Capabilities = caps; c.InjectCapabilities = true })
	if p, _ := on.SystemPrompt(org, user, false); !strings.Contains(p, "REGISTRO AUTORITATIVO") || checks == 0 {
		t.Fatal("InjectCapabilities must restore the clause")
	}
}

func TestCopilotTurnLimits(t *testing.T) {
	cases := []struct {
		cfgResult, cfgHistory   int
		wantResult, wantHistory int
	}{
		{0, 0, DefaultCopilotMaxToolResultBytes, DefaultCopilotHistoryTokenBudget},
		{2000, 900, 2000, 900},
		{-1, -1, 0, 0}, // defer to the orchestrator / whole history
	}
	for _, c := range cases {
		svc := newPromptCopilot(t, nil, func(cfg *CopilotConfig) {
			cfg.MaxToolResultBytes, cfg.HistoryTokenBudget = c.cfgResult, c.cfgHistory
		})
		r, h := svc.turnLimits()
		if r != c.wantResult || h != c.wantHistory {
			t.Errorf("cfg (%d,%d): got (%d,%d)", c.cfgResult, c.cfgHistory, r, h)
		}
	}
}
