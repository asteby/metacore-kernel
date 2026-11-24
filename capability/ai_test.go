package capability_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/capability"
	"github.com/asteby/metacore-kernel/capability/aitest"
	"github.com/google/uuid"
)

var (
	aiOrg = uuid.MustParse("11111111-1111-1111-1111-111111111111")
	aiNow = time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
)

func ticketsPolicy() capability.AIPolicy {
	return capability.NewAIPolicy("tickets",
		[]capability.AIGrant{{Feature: "ticket_triage", Purpose: "clasificar tickets"}, {Feature: "reply_draft", Purpose: "borrador"}},
		&capability.AISpec{MonthlySparksCap: 100, Features: []capability.AIFeatureSpec{
			{Key: "ticket_triage", MaxTier: "fast", MaxTokens: 600, MonthlySparksCap: 40, PersonalData: true},
		}})
}

func newGate(p *aitest.Provider, m *aitest.Meter) *capability.AIGate {
	g := &capability.AIGate{
		Provider: p, Policies: aitest.Policies{"tickets": ticketsPolicy()},
		Defaults: capability.AIDefaults{Tier: "balanced", MaxTokens: 1000},
		Now:      func() time.Time { return aiNow },
	}
	if m != nil {
		g.Meter = m
	}
	return g
}

func triage(prompt string) capability.AICall {
	return capability.AICall{Org: aiOrg, Addon: "tickets", Request: capability.AIRequest{Feature: "ticket_triage", Prompt: prompt}}
}

func TestAICompleteContractRegistered(t *testing.T) {
	c, ok := capability.Lookup(capability.AIComplete)
	if !ok || !c.IsQuery() || c.Fanout() || !c.HostImplemented || c.Idempotent || c.Timeout != capability.AITimeout {
		t.Fatalf("ai.complete must be a host-implemented, non-fanout query: %+v", c)
	}
	if errs := c.CheckInput(map[string]any{"feature": "ticket_triage", "prompt": "hola", "tier": "fast",
		"response_format": map[string]any{"type": "json_object"}, "max_tokens": 100}); len(errs) != 0 {
		t.Fatalf("valid input rejected: %v", errs)
	}
	if errs := c.CheckInput(map[string]any{"feature": "Bad-Key", "tier": "ultra"}); len(errs) < 2 {
		t.Fatalf("bad feature and tier must fail: %v", errs)
	}
	out, _ := json.Marshal(aitest.OK("hola", 3))
	if errs := c.ValidateOutput(out); len(errs) != 0 {
		t.Fatalf("an AIResult must validate as the contract output: %v", errs)
	}
	out, _ = json.Marshal(capability.AIDegraded(capability.AIStatusInsufficientPulse, "sin pulse"))
	if errs := c.ValidateOutput(out); len(errs) != 0 {
		t.Fatalf("a degraded AIResult must validate: %v", errs)
	}
}

func TestAIRequestValidate(t *testing.T) {
	temp := 3.0
	cases := []struct {
		name string
		req  capability.AIRequest
		want string
	}{
		{"no text", capability.AIRequest{Feature: "x"}, "messages or prompt"},
		{"both", capability.AIRequest{Feature: "x", Prompt: "a", Messages: []capability.AIMessage{{Role: "user", Content: "b"}}}, "not both"},
		{"bad role", capability.AIRequest{Feature: "x", Messages: []capability.AIMessage{{Role: "tool", Content: "b"}}}, "role"},
		{"no user turn", capability.AIRequest{Feature: "x", Messages: []capability.AIMessage{{Role: "system", Content: "b"}}}, "user turn"},
		{"schema without type", capability.AIRequest{Feature: "x", Prompt: "a", ResponseFormat: &capability.AIResponseFormat{Type: "json_object", Schema: json.RawMessage(`{}`)}}, "only allowed"},
		{"json_schema without schema", capability.AIRequest{Feature: "x", Prompt: "a", ResponseFormat: &capability.AIResponseFormat{Type: "json_schema"}}, "JSON Schema"},
		{"temperature", capability.AIRequest{Feature: "x", Prompt: "a", Temperature: &temp}, "temperature"},
		{"long key", capability.AIRequest{Feature: "x", Prompt: "a", IdempotencyKey: strings.Repeat("k", 129)}, "idempotency_key"},
		{"feature", capability.AIRequest{Feature: "x.y", Prompt: "a"}, "feature"},
	}
	for _, tc := range cases {
		errs := strings.Join(tc.req.Validate(), "; ")
		if !strings.Contains(errs, tc.want) {
			t.Errorf("%s: want %q in %q", tc.name, tc.want, errs)
		}
	}
	ok := capability.AIRequest{Feature: "x", System: "s", Messages: []capability.AIMessage{{Role: "user", Content: "hola"}},
		ResponseFormat: &capability.AIResponseFormat{Type: "json_schema", Schema: json.RawMessage(`{"type":"object"}`)}}
	if errs := ok.Validate(); len(errs) != 0 {
		t.Fatalf("valid request rejected: %v", errs)
	}
	if conv := ok.Conversation(); len(conv) != 2 || conv[0].Role != "system" {
		t.Fatalf("system must go first: %+v", conv)
	}
}

func TestClampAITier(t *testing.T) {
	for _, tc := range [][3]string{{"heavy", "fast", "fast"}, {"fast", "heavy", "fast"}, {"", "fast", ""}, {"heavy", "", "heavy"}} {
		if got := capability.ClampAITier(tc[0], tc[1]); got != tc[2] {
			t.Errorf("ClampAITier(%q,%q) = %q, want %q", tc[0], tc[1], got, tc[2])
		}
	}
	if capability.LowerCap(0, 5) != 5 || capability.LowerCap(7, 0) != 7 || capability.LowerCap(7, 5) != 5 {
		t.Fatal("LowerCap must pick the stricter non-zero cap")
	}
}

func TestValidateAIDeclaration(t *testing.T) {
	grants := []capability.AIGrant{{Feature: "ticket_triage", Purpose: "x"}}
	if errs := capability.ValidateAIDeclaration(grants, &capability.AISpec{MonthlySparksCap: 10,
		Features: []capability.AIFeatureSpec{{Key: "ticket_triage", MaxTier: "fast", MonthlySparksCap: 5}}}); len(errs) != 0 {
		t.Fatalf("valid declaration rejected: %v", errs)
	}
	errs := strings.Join(capability.ValidateAIDeclaration(
		[]capability.AIGrant{{Feature: "*", Purpose: "x"}, {Feature: "a", Purpose: " "}, {Feature: "a", Purpose: "y"}},
		&capability.AISpec{MonthlySparksCap: 10, Features: []capability.AIFeatureSpec{
			{Key: "zz"}, {Key: "a", MaxTier: "ultra", MaxTokens: -1, MonthlySparksCap: 11}, {Key: "a"},
		}}), "\n")
	for _, want := range []string{"no wildcards", "reason is required", "granted twice", "no ai:complete capability", "max_tier", "max_tokens", "exceeds", "duplicated"} {
		if !strings.Contains(errs, want) {
			t.Errorf("want %q in:\n%s", want, errs)
		}
	}
}

func TestAIGateAppliesFeatureLimitsAndPolicy(t *testing.T) {
	p := aitest.NewProvider(aitest.OK("urgente", 2))
	m := aitest.NewMeter()
	g := newGate(p, m)
	call := triage("cliente molesto")
	call.Request.Tier, call.Request.MaxTokens = "heavy", 5000
	res, err := g.Complete(context.Background(), call)
	if err != nil || !res.OK() || res.Content != "urgente" || res.BilledTo != "org" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	got := p.Last()
	if got.Request.Tier != "fast" || got.Request.MaxTokens != 600 || !got.PersonalData || got.Org != aiOrg || got.Addon != "tickets" {
		t.Fatalf("limits not applied: %+v", got)
	}
	if len(m.Records) != 1 || m.Records[0].CostSparks != 2 || m.Records[0].Period != "2026-11" || m.Records[0].Feature != "ticket_triage" {
		t.Fatalf("usage not recorded: %+v", m.Records)
	}
	// A feature without its own limits takes the host defaults.
	res, err = g.Complete(context.Background(), capability.AICall{Org: aiOrg, Addon: "tickets", Request: capability.AIRequest{Feature: "reply_draft", Prompt: "x"}})
	if err != nil || !res.OK() || p.Last().Request.Tier != "balanced" || p.Last().Request.MaxTokens != 1000 {
		t.Fatalf("defaults not applied: %+v err=%v", p.Last().Request, err)
	}
}

func TestAIGateForbiddenAndInvalidAreErrors(t *testing.T) {
	p := aitest.NewProvider()
	g := newGate(p, nil)
	_, err := g.Complete(context.Background(), capability.AICall{Org: aiOrg, Addon: "tickets", Request: capability.AIRequest{Feature: "crm_score", Prompt: "x"}})
	if !errors.Is(err, capability.ErrAIForbidden) {
		t.Fatalf("undeclared feature must be forbidden, got %v", err)
	}
	_, err = g.Complete(context.Background(), capability.AICall{Org: aiOrg, Addon: "crm", Request: capability.AIRequest{Feature: "ticket_triage", Prompt: "x"}})
	if !errors.Is(err, capability.ErrAIForbidden) {
		t.Fatalf("uninstalled addon must be forbidden, got %v", err)
	}
	_, err = g.Complete(context.Background(), capability.AICall{Org: aiOrg, Addon: "", Request: capability.AIRequest{Feature: "ticket_triage", Prompt: "x"}})
	if !errors.Is(err, capability.ErrAIForbidden) {
		t.Fatalf("a call without addon must be forbidden, got %v", err)
	}
	_, err = g.Complete(context.Background(), capability.AICall{Org: aiOrg, Addon: "tickets", Request: capability.AIRequest{Feature: "ticket_triage"}})
	if !errors.Is(err, capability.ErrInvalidInput) {
		t.Fatalf("invalid request must be ErrInvalidInput, got %v", err)
	}
	if p.CallCount() != 0 {
		t.Fatal("nothing may reach the provider")
	}
}

func TestAIGateQuotaExceededIsAResult(t *testing.T) {
	p := aitest.NewProvider()
	m := aitest.NewMeter()
	m.Add(aiOrg, "tickets", "ticket_triage", "2026-11", 40)
	g := newGate(p, m)
	res, err := g.Complete(context.Background(), triage("x"))
	if err != nil || res.Status != capability.AIStatusQuotaExceeded || p.CallCount() != 0 {
		t.Fatalf("feature cap must cut before the provider: res=%+v err=%v calls=%d", res, err, p.CallCount())
	}
	// The addon cap (100) covers the other feature too.
	m.Add(aiOrg, "tickets", "reply_draft", "2026-11", 60)
	res, _ = g.Complete(context.Background(), capability.AICall{Org: aiOrg, Addon: "tickets", Request: capability.AIRequest{Feature: "reply_draft", Prompt: "x"}})
	if res.Status != capability.AIStatusQuotaExceeded {
		t.Fatalf("addon cap must cut: %+v", res)
	}
	// Previous month's spend does not count.
	g.Now = func() time.Time { return aiNow.AddDate(0, 1, 0) }
	res, _ = g.Complete(context.Background(), triage("x"))
	if !res.OK() {
		t.Fatalf("a new period starts from zero: %+v", res)
	}
	// Meter down: fail closed.
	m.Err = errors.New("db down")
	res, err = g.Complete(context.Background(), triage("x"))
	if err != nil || res.Status != capability.AIStatusUnavailable {
		t.Fatalf("meter failure must be unavailable: %+v %v", res, err)
	}
}

func TestAIGateDefaultAddonCapFromHostConfig(t *testing.T) {
	m := aitest.NewMeter()
	m.Add(aiOrg, "crm", "", "2026-11", 50)
	g := &capability.AIGate{
		Provider: aitest.NewProvider(),
		Policies: aitest.Policies{"crm": capability.NewAIPolicy("crm", []capability.AIGrant{{Feature: "lead_score", Purpose: "x"}}, nil)},
		Meter:    m, Defaults: capability.AIDefaults{MonthlySparksCap: 50}, Now: func() time.Time { return aiNow },
	}
	res, _ := g.Complete(context.Background(), capability.AICall{Org: aiOrg, Addon: "crm", Request: capability.AIRequest{Feature: "lead_score", Prompt: "x"}})
	if res.Status != capability.AIStatusQuotaExceeded {
		t.Fatalf("host default cap must apply to an addon without ai block: %+v", res)
	}
}

func TestAIGateDegradedProviderOutcomesAreResults(t *testing.T) {
	for _, st := range []capability.AIStatus{capability.AIStatusInsufficientPulse, capability.AIStatusRateLimited, capability.AIStatusBudgetExceeded, capability.AIStatusDisabled} {
		g := newGate(aitest.NewProvider(capability.AIDegraded(st, "x")), nil)
		res, err := g.Complete(context.Background(), triage("x"))
		if err != nil || res.Status != st {
			t.Fatalf("%s must come back as a status, got %+v %v", st, res, err)
		}
	}
	p := aitest.NewProvider()
	p.Err = errors.New("dial tcp: refused")
	res, err := newGate(p, nil).Complete(context.Background(), triage("x"))
	if err != nil || res.Status != capability.AIStatusUnavailable {
		t.Fatalf("transport errors must be unavailable: %+v %v", res, err)
	}
	// No gateway configured.
	res, err = (&capability.AIGate{}).Complete(context.Background(), triage("x"))
	if err != nil || res.Status != capability.AIStatusUnavailable {
		t.Fatalf("no gateway must be unavailable: %+v %v", res, err)
	}
}

func TestAIGateJSONResponseFormat(t *testing.T) {
	call := triage("x")
	call.Request.ResponseFormat = &capability.AIResponseFormat{Type: "json_object"}
	res, _ := newGate(aitest.NewProvider(aitest.OK("```json\n{\"priority\": \"high\"}\n```", 2)), nil).Complete(context.Background(), call)
	if !res.OK() || string(res.JSON) != `{"priority":"high"}` {
		t.Fatalf("fenced JSON must decode: %+v", res)
	}
	res, _ = newGate(aitest.NewProvider(aitest.OK("alta prioridad", 2)), nil).Complete(context.Background(), call)
	if res.Status != capability.AIStatusInvalidOutput || res.CostSparks != 2 || res.JSON != nil {
		t.Fatalf("non-JSON must be invalid_output and keep the cost: %+v", res)
	}
}

func TestAIGateRedactionAndSponsor(t *testing.T) {
	sponsor := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	pol := ticketsPolicy()
	pol.Sponsor = sponsor
	p := aitest.NewProvider()
	g := &capability.AIGate{Provider: p, Policies: aitest.Policies{"tickets": pol}, Now: func() time.Time { return aiNow },
		Redact: func(_ context.Context, c capability.AICall) (capability.AICall, error) {
			c.Request.Prompt = strings.ReplaceAll(c.Request.Prompt, "ana@x.com", "[email]")
			c.Addon, c.Request.Feature, c.Request.Tier = "other", "other", "heavy" // must be restored
			return c, nil
		}}
	if _, err := g.Complete(context.Background(), triage("escribe a ana@x.com")); err != nil {
		t.Fatal(err)
	}
	got := p.Last()
	if got.Request.Prompt != "escribe a [email]" || got.Addon != "tickets" || got.Request.Feature != "ticket_triage" || got.Request.Tier != "fast" || got.Sponsor != sponsor {
		t.Fatalf("redaction or sponsor wrong: %+v", got)
	}
	// A feature-level sponsor wins over the addon-level one.
	featureSponsor := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	f := pol.Features["reply_draft"]
	f.Sponsor = featureSponsor
	pol.Features["reply_draft"] = f
	if _, err := g.Complete(context.Background(), capability.AICall{Org: aiOrg, Addon: "tickets", Request: capability.AIRequest{Feature: "reply_draft", Prompt: "x"}}); err != nil || p.Last().Sponsor != featureSponsor {
		t.Fatalf("feature sponsor: %v %v", p.Last().Sponsor, err)
	}
	g.Redact = func(context.Context, capability.AICall) (capability.AICall, error) {
		return capability.AICall{}, errors.New("boom")
	}
	res, err := g.Complete(context.Background(), triage("x"))
	if err != nil || res.Status != capability.AIStatusUnavailable || p.CallCount() != 2 {
		t.Fatalf("failed redaction must not send: %+v %v calls=%d", res, err, p.CallCount())
	}
}

func TestAIHostQuerier(t *testing.T) {
	p := aitest.NewProvider(aitest.OK("hola", 1))
	q := capability.AIHostQuerier{Gate: newGate(p, nil)}
	res, err := q.Query(context.Background(), capability.Query{Org: aiOrg, Caller: "tickets", Capability: capability.AIComplete,
		Input: map[string]any{"feature": "ticket_triage", "prompt": "x", "idempotency_key": "k1"}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := res.One()
	if err != nil || res.Results[0].Provider != capability.AIHostProvider {
		t.Fatalf("one result from the host expected: %+v %v", res, err)
	}
	var got capability.AIResult
	if err := json.Unmarshal(out, &got); err != nil || !got.OK() || got.Content != "hola" || p.Last().Request.IdempotencyKey != "k1" {
		t.Fatalf("output %s err=%v", out, err)
	}
	// The guest cannot pick the addon: the caller decides the policy.
	res, _ = q.Query(context.Background(), capability.Query{Org: aiOrg, Caller: "crm", Capability: capability.AIComplete,
		Input: map[string]any{"feature": "ticket_triage", "prompt": "x"}})
	if res.Results[0].ErrorCode() != capability.ErrCodeForbidden {
		t.Fatalf("other addon must be forbidden, got %q", res.Results[0].ErrorCode())
	}
	if _, err := q.Query(context.Background(), capability.Query{Org: aiOrg, Caller: "tickets", Capability: capability.AIComplete,
		Input: map[string]any{"prompt": "x"}}); !errors.Is(err, capability.ErrInvalidInput) {
		t.Fatalf("missing feature must be invalid input, got %v", err)
	}
	// Other capabilities go to Inner.
	if _, err := q.Query(context.Background(), capability.Query{Org: aiOrg, Capability: capability.GeoGeocode}); !errors.Is(err, capability.ErrNoProvider) {
		t.Fatalf("no inner: ErrNoProvider, got %v", err)
	}
	listing, err := q.ListProviders(context.Background(), capability.Query{Capability: capability.AIComplete})
	if err != nil || listing.Resolution != "ok" {
		t.Fatalf("listing %+v %v", listing, err)
	}
}
