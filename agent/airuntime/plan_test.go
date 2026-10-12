package airuntime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func labels(steps []Step) string {
	var parts []string
	for _, s := range steps {
		x := string(s.Backend)
		if s.Tier != "" {
			x += "/" + s.Tier
		} else if s.Provider != "" {
			x += "/" + s.Provider
		}
		if s.Model != "" && s.Backend != BackendOrgEndpoint {
			x += ":" + s.Model
		}
		if s.TextOnly {
			x += "~text"
		}
		parts = append(parts, x)
	}
	return strings.Join(parts, " > ")
}

func TestPlanOrgEndpointIsSingleAttempt(t *testing.T) {
	in := PlanInput{Runtime: Runtime{Mode: ModeAnthropic, Protocol: ProtocolAnthropic, ProxyModel: "claude-x"}, HubEnabled: true, ClaudeCodeAvailable: true, Providers: []Provider{{Name: "groq", SupportsTools: true}}}
	for _, steps := range [][]Step{PlanTools(in), PlanSimple(in)} {
		if len(steps) != 1 || steps[0].Backend != BackendOrgEndpoint || steps[0].Provider != "anthropic-api" || steps[0].Model != "claude-x" {
			t.Fatalf("%+v", steps)
		}
	}
	in.Runtime = Runtime{Mode: ModeCodex, Protocol: ProtocolOpenAI}
	if PlanTools(in)[0].Provider != "openai-proxy" {
		t.Fatal("openai-compatible modes use the openai-proxy provider")
	}
}

func TestPlanToolsHub(t *testing.T) {
	hub := PlanInput{Runtime: Default(), HubEnabled: true}
	if got := labels(PlanTools(hub)); got != "hub/fast > hub/heavy" {
		t.Fatal(got)
	}
	hub.Live = true
	if got := labels(PlanTools(hub)); got != "hub/fast" {
		t.Fatalf("live turns skip heavy: %s", got)
	}
	hub.Live, hub.HasImages, hub.ClaudeCodeAvailable = false, true, true
	want := "hub/fast > hub/heavy > hub/fast~text > hub/heavy~text > claude-code/claude-code:" + ClaudeCodeFastModel + " > claude-code/claude-code:" + ClaudeCodeHeavyModel
	if got := labels(PlanTools(hub)); got != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
	// hub mode without a hub client falls through to the catalog
	nohub := PlanInput{Runtime: Default(), Providers: []Provider{{Name: "openai", DefaultModel: "gpt", SupportsTools: true}}}
	if got := labels(PlanTools(nohub)); got != "provider/openai:gpt" {
		t.Fatal(got)
	}
}

func TestPlanToolsCatalog(t *testing.T) {
	provs := []Provider{
		{Name: "groq", DefaultModel: "llama", SupportsTools: true, SupportsVision: false},
		{Name: "openai", DefaultModel: "gpt", SupportsTools: true, SupportsVision: true},
		{Name: "nobtools", DefaultModel: "x", SupportsTools: false},
		{Name: "mistral", DefaultModel: "mis", SupportsTools: true},
	}
	in := PlanInput{Runtime: Runtime{Mode: ModeLocal}, Providers: provs}
	if got := labels(PlanTools(in)); got != "provider/groq:llama > provider/openai:gpt > provider/mistral:mis" {
		t.Fatalf("no images: every tool-capable provider in order: %s", got)
	}
	in.HasImages = true
	if got := labels(PlanTools(in)); got != "provider/openai:gpt > provider/groq:llama~text > provider/openai:gpt~text > provider/mistral:mis~text" {
		t.Fatalf("images: vision first, text-only second: %s", got)
	}
	cc := append([]Provider{{Name: ClaudeCodeProvider, SupportsTools: true}}, provs...)
	in = PlanInput{Runtime: Runtime{Mode: ModeLocal}, Providers: cc, HasImages: true}
	got := labels(PlanTools(in))
	if strings.Contains(got, "groq") {
		t.Fatalf("groq is skipped when claude-code exists: %s", got)
	}
	if !strings.HasPrefix(got, "claude-code/claude-code:"+ClaudeCodeFastModel+" > claude-code/claude-code:"+ClaudeCodeHeavyModel+" > provider/openai") {
		t.Fatalf("claude-code first with both models: %s", got)
	}
	in.Live = true
	if strings.Contains(labels(PlanTools(in)), ClaudeCodeHeavyModel) {
		t.Fatal("live must skip the heavy model")
	}
}

func TestPlanSimple(t *testing.T) {
	if got := labels(PlanSimple(PlanInput{Runtime: Default(), HubEnabled: true, ClaudeCodeAvailable: true})); got != "hub/balanced > claude-code/claude-code:"+ClaudeCodeFastModel {
		t.Fatal(got)
	}
	in := PlanInput{Runtime: Runtime{Mode: ModeLocal}, Providers: []Provider{{Name: "a", DefaultModel: "m1"}, {Name: "b", DefaultModel: "m2"}}}
	if got := labels(PlanSimple(in)); got != "provider/a:m1 > provider/b:m2" {
		t.Fatal(got)
	}
}

func TestTierForModelAndOrgProvider(t *testing.T) {
	if TierForModel("x", ClaudeCodeHeavyModel) != TierHeavy || TierForModel("x", ClaudeCodeFastModel) != TierFast ||
		TierForModel(ClaudeCodeProvider, "?") != TierFast || TierForModel("openai", "gpt") != TierBalanced {
		t.Fatal("tiers")
	}
}

func TestExecuteWithFallback(t *testing.T) {
	boom := errors.New("down")
	got, err := ExecuteWithFallback("op",
		func() (string, error) { return "", boom },
		func() (string, error) { return "second", nil },
		func() (string, error) { t.Fatal("must stop at first success"); return "", nil })
	if err != nil || got != "second" {
		t.Fatalf("%q %v", got, err)
	}
	_, err = ExecuteWithFallback("op", func() (int, error) { return 0, boom })
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "all op strategies failed") {
		t.Fatalf("%v", err)
	}
	if _, err := ExecuteWithFallback[int]("op"); err == nil {
		t.Fatal("no strategies is an error")
	}
}

func TestRetryOnceOnRateLimit(t *testing.T) {
	n := 0
	res, err := retryOnce(context.Background(), time.Millisecond, func() (string, error) {
		n++
		if n == 1 {
			return "", errors.New("HTTP 429 Too Many Requests")
		}
		return "ok", nil
	})
	if err != nil || res != "ok" || n != 2 {
		t.Fatalf("%q %v n=%d", res, err, n)
	}
	n = 0
	_, err = retryOnce(context.Background(), time.Millisecond, func() (string, error) { n++; return "", errors.New("rate limit exceeded") })
	if err == nil || n != 2 {
		t.Fatalf("exactly one retry, n=%d", n)
	}
	n = 0
	_, _ = retryOnce(context.Background(), time.Millisecond, func() (string, error) { n++; return "", errors.New("500 boom") })
	if n != 1 {
		t.Fatal("non-429 must not retry")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := retryOnce(ctx, time.Hour, func() (string, error) { return "", errors.New("429") }); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled context must abort the wait: %v", err)
	}
	if IsRateLimitErr(nil) || !IsRateLimitErr(errors.New("rate_limit_error")) {
		t.Fatal("IsRateLimitErr")
	}
}

func TestStripThinkTags(t *testing.T) {
	if got := StripThinkTags("<think>\nhmm\n</think>\n Hola"); got != "Hola" {
		t.Fatalf("%q", got)
	}
	if StripThinkTags("sin tags") != "sin tags" {
		t.Fatal("untouched")
	}
}
