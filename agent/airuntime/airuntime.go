// Package airuntime holds the per-organization AI inference preference the
// agent runtime needs to pick a wire protocol and credentials: the mode
// vocabulary, the plain-data Config, the Runtime stamped on a context.Context
// and the Resolver that turns org + user preferences into a Runtime.
//
// It never touches a database. Hosts implement ConfigStore (and optionally
// SeatDetector) over their own tables.
package airuntime

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/google/uuid"
)

// Org AI inference modes.
const (
	ModeHub             = "hub"
	ModeLocal           = "local"
	ModeClaudeCode      = "claude_code"
	ModeCodex           = "codex"
	ModeCursor          = "cursor"
	ModeAnthropic       = "anthropic"
	ModeOpenAI          = "openai"
	ModeDeepSeek        = "deepseek"
	ModeOpenAIAPI       = "openai_api"        // manual / any OpenAI-compatible
	ModeClaudeCodeProxy = "claude_code_proxy" // legacy alias of claude_code
)

// Billing modes.
const (
	BillingMetered = "metered"
	BillingFree    = "free"
)

// Wire protocols.
const (
	ProtocolHub       = "hub"
	ProtocolOpenAI    = "openai"
	ProtocolAnthropic = "anthropic"
)

// DefaultProxyURL is the built-in local OpenAI-compatible proxy endpoint.
const DefaultProxyURL = "http://127.0.0.1:8765/v1"

// ProxyURLEnv overrides DefaultProxyURL for proxy-backed modes whose stored
// URL is empty or the default (Docker: host.docker.internal).
const ProxyURLEnv = "ABY_PROXY_URL"

// NormalizeMode maps legacy mode names to their canonical form.
func NormalizeMode(mode string) string {
	if mode == ModeClaudeCodeProxy {
		return ModeClaudeCode
	}
	return mode
}

// ValidMode reports whether mode (after normalization) is a known mode.
func ValidMode(mode string) bool {
	switch NormalizeMode(mode) {
	case ModeHub, ModeLocal, ModeClaudeCode, ModeCodex, ModeCursor,
		ModeAnthropic, ModeOpenAI, ModeDeepSeek, ModeOpenAIAPI:
		return true
	}
	return false
}

// ProtocolFor returns the wire protocol for a mode.
func ProtocolFor(mode string) string {
	switch NormalizeMode(mode) {
	case ModeHub, "":
		return ProtocolHub
	case ModeAnthropic:
		return ProtocolAnthropic
	default:
		return ProtocolOpenAI
	}
}

// UsesEndpoint reports whether the mode talks to a configurable HTTP endpoint.
func UsesEndpoint(mode string) bool {
	switch NormalizeMode(mode) {
	case ModeClaudeCode, ModeCodex, ModeCursor, ModeAnthropic,
		ModeOpenAI, ModeDeepSeek, ModeOpenAIAPI:
		return true
	}
	return false
}

// NeedsAPIKey reports whether the mode requires a stored API key.
func NeedsAPIKey(mode string) bool {
	switch NormalizeMode(mode) {
	case ModeAnthropic, ModeOpenAI, ModeDeepSeek, ModeOpenAIAPI:
		return true
	}
	return false
}

// Config is the stored per-org preference (the host persists it).
type Config struct {
	OrganizationID uuid.UUID
	Mode           string
	ProxyBaseURL   string
	ProxyModel     string
	ProxyAPIKey    string
	// SparkRate is what the org charges its own users per AI turn when it
	// brings a provider. Zero means the org does not resell.
	SparkRate int
}

// BillingMode is "metered" for hub or resold turns, otherwise "free".
func (c Config) BillingMode() string {
	switch NormalizeMode(c.Mode) {
	case ModeHub, "":
		return BillingMetered
	}
	if c.SparkRate > 0 {
		return BillingMetered
	}
	return BillingFree
}

// SparksPayer says who meters the turn: "hub", "org" or "none".
func (c Config) SparksPayer() string {
	switch NormalizeMode(c.Mode) {
	case ModeHub, "":
		return "hub"
	}
	if c.SparkRate > 0 {
		return "org"
	}
	return "none"
}

// EffectiveProxyBaseURL is the stored URL or the mode's default.
func (c Config) EffectiveProxyBaseURL() string {
	if c.ProxyBaseURL != "" {
		return c.ProxyBaseURL
	}
	switch NormalizeMode(c.Mode) {
	case ModeAnthropic:
		return "https://api.anthropic.com/v1"
	case ModeOpenAI, ModeOpenAIAPI:
		return "https://api.openai.com/v1"
	case ModeDeepSeek:
		return "https://api.deepseek.com/v1"
	}
	return DefaultProxyURL
}

// EffectiveProxyModel is the stored model or the mode's default.
func (c Config) EffectiveProxyModel() string {
	if c.ProxyModel != "" {
		return c.ProxyModel
	}
	switch NormalizeMode(c.Mode) {
	case ModeCodex:
		return "codex"
	case ModeCursor:
		return "cursor-agent"
	case ModeAnthropic:
		return "claude-sonnet-4-5"
	case ModeOpenAI, ModeOpenAIAPI:
		return "gpt-4o-mini"
	case ModeDeepSeek:
		return "deepseek-chat"
	}
	return "auto"
}

// Normalize validates a user-submitted config and fills endpoint defaults.
// hasStoredKey says whether the host already holds an API key for the org, so
// an empty apiKey on update is accepted. It returns the config to persist
// (credentials cleared for modes without an endpoint).
func Normalize(mode, baseURL, model, apiKey string, hasStoredKey bool) (Config, error) {
	mode = NormalizeMode(strings.TrimSpace(strings.ToLower(mode)))
	if !ValidMode(mode) {
		return Config{}, fmt.Errorf("invalid mode %q", mode)
	}
	baseURL = strings.TrimSpace(baseURL)
	model = strings.TrimSpace(model)
	apiKey = strings.TrimSpace(apiKey)

	if !UsesEndpoint(mode) {
		return Config{Mode: mode}, nil
	}
	tmp := Config{Mode: mode, ProxyBaseURL: baseURL, ProxyModel: model}
	if baseURL == "" {
		baseURL = tmp.EffectiveProxyBaseURL()
	}
	// The direct runner sentinel is not a URL.
	if !strings.EqualFold(baseURL, DirectSentinel) {
		if u, err := url.Parse(baseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return Config{}, fmt.Errorf("base URL must be http(s)")
		}
	}
	if model == "" {
		model = tmp.EffectiveProxyModel()
	}
	if NeedsAPIKey(mode) && apiKey == "" && !hasStoredKey {
		return Config{}, fmt.Errorf("API key required for %s", mode)
	}
	return Config{Mode: mode, ProxyBaseURL: baseURL, ProxyModel: model, ProxyAPIKey: apiKey}, nil
}

// DirectSentinel as a proxy URL forces the in-process `claude -p` runner.
const DirectSentinel = "direct"

// Runtime is stamped on a context so the model layer picks the right wire and
// credentials for the turn.
type Runtime struct {
	Mode         string
	Protocol     string
	BillingMode  string
	ProxyBaseURL string
	ProxyModel   string
	ProxyAPIKey  string
}

// Default is the hub-metered runtime used when nothing is stamped.
func Default() Runtime {
	return Runtime{Mode: ModeHub, Protocol: ProtocolHub, BillingMode: BillingMetered}
}

type runtimeKey struct{}

// With stamps rt on ctx. An empty mode is a no-op.
func With(ctx context.Context, rt Runtime) context.Context {
	if rt.Mode == "" {
		return ctx
	}
	return context.WithValue(ctx, runtimeKey{}, rt)
}

// From returns the stamped runtime or Default.
func From(ctx context.Context) Runtime {
	if ctx == nil {
		return Default()
	}
	if v, ok := ctx.Value(runtimeKey{}).(Runtime); ok && v.Mode != "" {
		return v
	}
	return Default()
}

// IsHub reports whether the turn runs on hub inference.
func IsHub(ctx context.Context) bool {
	m := NormalizeMode(From(ctx).Mode)
	return m == ModeHub || m == ""
}

// ApplyProxyEnv lets the deployment override the proxy URL through
// ABY_PROXY_URL (getenv injected for tests) for proxy-backed modes whose
// stored URL is empty or the default.
func ApplyProxyEnv(getenv func(string) string, mode, stored, effective string) string {
	switch mode {
	case ModeClaudeCode, ModeCodex, ModeCursor:
	default:
		return effective
	}
	if stored != "" && stored != DefaultProxyURL {
		return effective
	}
	if v := strings.TrimSpace(getenv(ProxyURLEnv)); v != "" {
		return v
	}
	return effective
}

// FromConfig derives the Runtime for a stored Config.
func FromConfig(cfg Config, getenv func(string) string) Runtime {
	mode := NormalizeMode(strings.TrimSpace(cfg.Mode))
	if mode == "" {
		mode = ModeHub
	}
	rt := Runtime{Mode: mode, Protocol: ProtocolFor(mode), BillingMode: cfg.BillingMode()}
	if UsesEndpoint(mode) {
		rt.ProxyBaseURL = cfg.EffectiveProxyBaseURL()
		if getenv != nil {
			rt.ProxyBaseURL = ApplyProxyEnv(getenv, mode, cfg.ProxyBaseURL, rt.ProxyBaseURL)
		}
		rt.ProxyModel = cfg.EffectiveProxyModel()
		rt.ProxyAPIKey = strings.TrimSpace(cfg.ProxyAPIKey)
	}
	return rt
}

// DirectEnv are the env vars a `claude -p` runner needs to authenticate as the
// org.
func DirectEnv(rt Runtime) []string {
	if rt.ProxyAPIKey == "" {
		return nil
	}
	return []string{"CLAUDE_CODE_OAUTH_TOKEN=" + rt.ProxyAPIKey}
}

// SourceLocal is the user preference that bypasses hub and the org key.
const SourceLocal = "local"

// ConfigStore is what a host implements over its own storage.
type ConfigStore interface {
	// OrgConfig returns the org's config; a missing row must return the zero
	// Config (hub) and a nil error.
	OrgConfig(orgID uuid.UUID) (Config, error)
	// UserAISource returns "local" or "inherit" (anything else is inherit).
	UserAISource(userID uuid.UUID) string
}

// SeatDetector reports which local agent CLI seat is logged in on this
// machine: ModeClaudeCode, ModeCodex, ModeCursor or "" for none.
type SeatDetector interface {
	ReadySeat() string
}

// Resolver turns org + user preferences into a Runtime.
type Resolver struct {
	Store  ConfigStore
	Seats  SeatDetector // optional
	Getenv func(string) string
}

// Load is the org default, then the user's own choice. ai_source=local skips
// the hub and the org key: the seat on this machine runs the turn unmetered.
func (r *Resolver) Load(orgID, userID uuid.UUID) (Runtime, error) {
	if orgID == uuid.Nil {
		return Default(), nil
	}
	cfg, err := r.Store.OrgConfig(orgID)
	if err != nil {
		return Default(), err
	}
	rt := FromConfig(cfg, r.Getenv)
	if userID != uuid.Nil && r.Store.UserAISource(userID) == SourceLocal {
		rt = r.applyLocal(rt)
	}
	return rt, nil
}

func (r *Resolver) applyLocal(rt Runtime) Runtime {
	mode := ModeLocal
	if r.Seats != nil {
		if s := r.Seats.ReadySeat(); s != "" {
			mode = s
		}
	}
	rt.Mode = mode
	rt.Protocol = ProtocolFor(mode)
	rt.BillingMode = BillingFree
	rt.ProxyAPIKey = ""
	if UsesEndpoint(mode) {
		cfg := Config{Mode: mode}
		rt.ProxyBaseURL = cfg.EffectiveProxyBaseURL()
		rt.ProxyModel = cfg.EffectiveProxyModel()
	} else {
		rt.ProxyBaseURL, rt.ProxyModel = "", ""
	}
	return rt
}
