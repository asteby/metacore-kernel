package airuntime

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

type fakeStore struct {
	cfg    Config
	err    error
	source string
}

func (f fakeStore) OrgConfig(uuid.UUID) (Config, error) { return f.cfg, f.err }
func (f fakeStore) UserAISource(uuid.UUID) string       { return f.source }

type fakeSeats string

func (s fakeSeats) ReadySeat() string { return string(s) }

func TestNormalizeAndProtocol(t *testing.T) {
	if NormalizeMode(ModeClaudeCodeProxy) != ModeClaudeCode {
		t.Fatal("legacy alias not normalized")
	}
	cases := map[string]string{"": ProtocolHub, ModeHub: ProtocolHub, ModeAnthropic: ProtocolAnthropic, ModeOpenAI: ProtocolOpenAI, ModeLocal: ProtocolOpenAI}
	for m, want := range cases {
		if got := ProtocolFor(m); got != want {
			t.Errorf("ProtocolFor(%q)=%q want %q", m, got, want)
		}
	}
}

func TestConfigBilling(t *testing.T) {
	if (Config{}).BillingMode() != BillingMetered || (Config{}).SparksPayer() != "hub" {
		t.Fatal("hub must be metered")
	}
	c := Config{Mode: ModeAnthropic}
	if c.BillingMode() != BillingFree || c.SparksPayer() != "none" {
		t.Fatal("byo must be free")
	}
	c.SparkRate = 2
	if c.BillingMode() != BillingMetered || c.SparksPayer() != "org" {
		t.Fatal("resold must be metered by org")
	}
}

func TestNormalizeConfig(t *testing.T) {
	if _, err := Normalize("nope", "", "", "", false); err == nil {
		t.Fatal("invalid mode accepted")
	}
	if _, err := Normalize(ModeOpenAI, "", "", "", false); err == nil {
		t.Fatal("missing key accepted")
	}
	if _, err := Normalize(ModeOpenAI, "", "", "", true); err != nil {
		t.Fatalf("stored key should suffice: %v", err)
	}
	if _, err := Normalize(ModeOpenAIAPI, "ftp://x", "", "k", false); err == nil {
		t.Fatal("non-http url accepted")
	}
	c, err := Normalize(ModeClaudeCode, "direct", "", "", false)
	if err != nil || c.ProxyBaseURL != "direct" {
		t.Fatalf("direct sentinel rejected: %v %+v", err, c)
	}
	c, _ = Normalize(ModeHub, "http://x", "m", "k", false)
	if c.ProxyBaseURL != "" || c.ProxyAPIKey != "" {
		t.Fatal("hub must clear endpoint fields")
	}
}

func TestContextRoundTrip(t *testing.T) {
	if !IsHub(context.Background()) || !IsHub(nil) { //nolint:staticcheck
		t.Fatal("default must be hub")
	}
	ctx := With(context.Background(), Runtime{Mode: ModeCodex})
	if From(ctx).Mode != ModeCodex || IsHub(ctx) {
		t.Fatal("runtime not stamped")
	}
	if With(context.Background(), Runtime{}) != context.Background() {
		t.Fatal("empty mode must be a no-op")
	}
}

func TestProxyEnvOverride(t *testing.T) {
	env := func(string) string { return "http://host.docker.internal:8765/v1" }
	rt := FromConfig(Config{Mode: ModeClaudeCode}, env)
	if rt.ProxyBaseURL != "http://host.docker.internal:8765/v1" {
		t.Fatalf("env not applied: %s", rt.ProxyBaseURL)
	}
	rt = FromConfig(Config{Mode: ModeClaudeCode, ProxyBaseURL: "http://custom/v1"}, env)
	if rt.ProxyBaseURL != "http://custom/v1" {
		t.Fatal("custom stored url must win")
	}
	rt = FromConfig(Config{Mode: ModeAnthropic, ProxyAPIKey: " k "}, env)
	if rt.ProxyAPIKey != "k" || rt.Protocol != ProtocolAnthropic {
		t.Fatalf("%+v", rt)
	}
}

func TestResolver(t *testing.T) {
	org, user := uuid.New(), uuid.New()
	r := &Resolver{Store: fakeStore{cfg: Config{Mode: ModeAnthropic, ProxyAPIKey: "k"}, source: "inherit"}}
	rt, err := r.Load(org, user)
	if err != nil || rt.Mode != ModeAnthropic {
		t.Fatalf("%+v %v", rt, err)
	}
	if rt, _ := r.Load(uuid.Nil, user); rt.Mode != ModeHub {
		t.Fatal("nil org must be hub")
	}
	r = &Resolver{Store: fakeStore{cfg: Config{Mode: ModeAnthropic, ProxyAPIKey: "k"}, source: "local"}, Seats: fakeSeats(ModeClaudeCode)}
	rt, _ = r.Load(org, user)
	if rt.Mode != ModeClaudeCode || rt.ProxyAPIKey != "" || rt.BillingMode != BillingFree {
		t.Fatalf("local seat not applied: %+v", rt)
	}
	r.Seats = nil
	rt, _ = r.Load(org, user)
	if rt.Mode != ModeLocal || rt.ProxyBaseURL != "" {
		t.Fatalf("local without seat: %+v", rt)
	}
	r = &Resolver{Store: fakeStore{err: errors.New("db")}}
	if rt, err := r.Load(org, user); err == nil || rt.Mode != ModeHub {
		t.Fatal("store error must surface with hub fallback")
	}
	if got := DirectEnv(Runtime{ProxyAPIKey: "t"}); len(got) != 1 || got[0] != "CLAUDE_CODE_OAUTH_TOKEN=t" {
		t.Fatalf("%v", got)
	}
}
