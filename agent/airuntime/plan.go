package airuntime

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"
)

// Hub tiers: the quality/latency intent a turn asks the metered hub for.
const (
	TierFast     = "fast"
	TierBalanced = "balanced"
	TierHeavy    = "heavy"
)

// Claude Code provider identity and its two models.
const (
	ClaudeCodeProvider    = "claude-code"
	ClaudeCodeFastModel   = "claude-haiku-4-5-20251001"
	ClaudeCodeHeavyModel  = "claude-sonnet-4-5"
	providerGroq          = "groq"
	rateLimitRetryBackoff = 1500 * time.Millisecond
)

// Backend says how a Step is executed.
type Backend string

// Backends.
const (
	// BackendOrgEndpoint is the org's own endpoint (Claude Code proxy, Codex,
	// Anthropic, DeepSeek, any OpenAI-compatible URL).
	BackendOrgEndpoint Backend = "org-endpoint"
	// BackendHub is the metered hub, addressed by Tier.
	BackendHub Backend = "hub"
	// BackendClaudeCode is the host's Claude Code OAuth seat.
	BackendClaudeCode Backend = "claude-code"
	// BackendProvider is a provider row from the catalog.
	BackendProvider Backend = "provider"
)

// Provider is a catalog entry the host stores (the runtime never reads the
// table). Only what ordering needs is here.
type Provider struct {
	Name           string
	DefaultModel   string
	Priority       int // lower = tried first
	SupportsVision bool
	SupportsTools  bool
	// StripThinkTags asks to drop <think>...</think> from replies.
	StripThinkTags bool
}

// ErrProviderNotConfigured signals a provider that is missing, inactive or has
// no stored key. Callers can fall back to environment variables.
var ErrProviderNotConfigured = errors.New("provider not configured")

// ProviderStore is the provider-catalog port. Implemented by the host over its
// ai_providers table (decrypting keys is its business).
type ProviderStore interface {
	// ActiveWithKeys lists active providers that can authenticate, ordered by
	// priority.
	ActiveWithKeys() ([]Provider, error)
	// ResolveCreds returns the named active provider and its API key, or
	// ErrProviderNotConfigured.
	ResolveCreds(name string) (Provider, string, error)
}

// Step is one attempt in a fallback chain. The host maps it onto its LLM
// client; the runtime only decides order and shape.
type Step struct {
	Backend  Backend
	Provider string // org-endpoint: "anthropic-api" | "openai-proxy"; provider/claude-code: name
	Model    string
	Tier     string // hub only
	// TextOnly means images were stripped from the messages for this attempt.
	TextOnly bool
	Label    string // for logs: "vision", "text-only", "last-resort"...
}

// PlanInput describes the turn to plan.
type PlanInput struct {
	Runtime Runtime
	// HubEnabled is whether the host has a hub client configured (the hub path
	// applies when the runtime is hub mode AND this is true).
	HubEnabled bool
	Providers  []Provider
	// HasImages is whether the messages carry vision parts.
	HasImages bool
	// Live is a voice turn: only the fast model, latency over depth.
	Live bool
	// ClaudeCodeAvailable is whether the host has a Claude Code OAuth seat.
	ClaudeCodeAvailable bool
}

// UseHub reports whether the turn runs on hub inference.
func (in PlanInput) UseHub() bool {
	m := NormalizeMode(in.Runtime.Mode)
	return (m == ModeHub || m == "") && in.HubEnabled
}

// OrgEndpointProvider picks the provider name an org endpoint mode maps to.
func OrgEndpointProvider(rt Runtime) string {
	if rt.Protocol == ProtocolAnthropic || NormalizeMode(rt.Mode) == ModeAnthropic {
		return "anthropic-api"
	}
	return "openai-proxy"
}

// PlanTools orders the attempts for a turn WITH tools:
//
//   - org endpoint mode: exactly one attempt, no fallback (the org chose it);
//   - hub: fast tier, then heavy (skipped when Live), then text-only retries
//     when images are present, then Claude Code as a last resort;
//   - otherwise the provider catalog by priority: vision-capable first when
//     images are present, then a text-only pass for the rest.
func PlanTools(in PlanInput) []Step {
	if UsesEndpoint(in.Runtime.Mode) {
		return []Step{{Backend: BackendOrgEndpoint, Provider: OrgEndpointProvider(in.Runtime), Model: in.Runtime.ProxyModel, Label: "org"}}
	}
	if in.UseHub() {
		out := []Step{{Backend: BackendHub, Tier: TierFast, Label: "hub"}}
		if !in.Live {
			out = append(out, Step{Backend: BackendHub, Tier: TierHeavy, Label: "hub"})
		}
		if in.HasImages {
			out = append(out, Step{Backend: BackendHub, Tier: TierFast, TextOnly: true, Label: "text-only"})
			if !in.Live {
				out = append(out, Step{Backend: BackendHub, Tier: TierHeavy, TextOnly: true, Label: "text-only"})
			}
		}
		return appendClaudeCodeLastResort(out, in)
	}

	hasClaudeCode := false
	for _, p := range in.Providers {
		if p.Name == ClaudeCodeProvider && p.SupportsTools {
			hasClaudeCode = true
		}
	}
	var out []Step
	add := func(p Provider, textOnly bool, label string) {
		if p.Name == ClaudeCodeProvider {
			out = append(out, Step{Backend: BackendClaudeCode, Provider: p.Name, Model: ClaudeCodeFastModel, TextOnly: textOnly, Label: label})
			// Live voice turns skip the heavy model: latency matters more.
			if !in.Live {
				out = append(out, Step{Backend: BackendClaudeCode, Provider: p.Name, Model: ClaudeCodeHeavyModel, TextOnly: textOnly, Label: label})
			}
			return
		}
		out = append(out, Step{Backend: BackendProvider, Provider: p.Name, Model: p.DefaultModel, TextOnly: textOnly, Label: label})
	}
	// Pass 1: vision-capable (or all, with no images) with the original messages.
	for _, p := range in.Providers {
		if !p.SupportsTools {
			continue
		}
		if hasClaudeCode && p.Name == providerGroq {
			continue // avoids Groq's 413 TPM when a stronger seat exists
		}
		if in.HasImages && !p.SupportsVision && p.Name != ClaudeCodeProvider {
			continue
		}
		add(p, false, "vision")
	}
	// Pass 2: text-only so a non-vision fallback does not 422 on image parts.
	if in.HasImages {
		for _, p := range in.Providers {
			if !p.SupportsTools {
				continue
			}
			if hasClaudeCode && p.Name == providerGroq {
				continue
			}
			add(p, true, "text-only")
		}
	}
	return out
}

// PlanSimple orders the attempts for a turn WITHOUT tools: the org endpoint
// alone; hub (balanced) plus Claude Code as last resort; or the catalog by
// priority.
func PlanSimple(in PlanInput) []Step {
	if UsesEndpoint(in.Runtime.Mode) {
		return []Step{{Backend: BackendOrgEndpoint, Provider: OrgEndpointProvider(in.Runtime), Model: in.Runtime.ProxyModel, Label: "org"}}
	}
	if in.UseHub() {
		out := []Step{{Backend: BackendHub, Tier: TierBalanced, Label: "hub"}}
		if in.ClaudeCodeAvailable {
			out = append(out, Step{Backend: BackendClaudeCode, Provider: ClaudeCodeProvider, Model: ClaudeCodeFastModel, Label: "last-resort"})
		}
		return out
	}
	out := make([]Step, 0, len(in.Providers))
	for _, p := range in.Providers {
		out = append(out, Step{Backend: BackendProvider, Provider: p.Name, Model: p.DefaultModel, Label: "provider"})
	}
	return out
}

func appendClaudeCodeLastResort(out []Step, in PlanInput) []Step {
	if !in.ClaudeCodeAvailable {
		return out
	}
	out = append(out, Step{Backend: BackendClaudeCode, Provider: ClaudeCodeProvider, Model: ClaudeCodeFastModel, Label: "last-resort"})
	if !in.Live {
		out = append(out, Step{Backend: BackendClaudeCode, Provider: ClaudeCodeProvider, Model: ClaudeCodeHeavyModel, Label: "last-resort"})
	}
	return out
}

// TierForModel maps a provider/model to the hub tier it stands for.
func TierForModel(provider, model string) string {
	switch model {
	case ClaudeCodeHeavyModel:
		return TierHeavy
	case ClaudeCodeFastModel:
		return TierFast
	}
	if provider == ClaudeCodeProvider {
		return TierFast
	}
	return TierBalanced
}

// ExecuteWithFallback runs strategies in order until one succeeds.
func ExecuteWithFallback[T any](operationName string, strategies ...func() (T, error)) (T, error) {
	var lastErr error
	for i, strategy := range strategies {
		result, err := strategy()
		if err == nil {
			if i > 0 {
				log.Printf("%s succeeded with strategy %d (fallback)", operationName, i+1)
			}
			return result, nil
		}
		lastErr = err
		log.Printf("%s strategy %d failed: %v", operationName, i+1, err)
	}
	var zero T
	if lastErr == nil {
		return zero, fmt.Errorf("no %s strategies available", operationName)
	}
	return zero, fmt.Errorf("all %s strategies failed. Last error: %w", operationName, lastErr)
}

// IsRateLimitErr reports whether err looks like a provider 429.
func IsRateLimitErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "429") || strings.Contains(msg, "rate_limit") || strings.Contains(msg, "rate limit")
}

// RetryOnceOnRateLimit runs fn and, on a 429, waits briefly and retries ONCE
// before yielding to the next strategy. Long backoff stacks make a request
// exceed an upstream proxy's origin wait and surface as a 502 to the user.
func RetryOnceOnRateLimit[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	return retryOnce(ctx, rateLimitRetryBackoff, fn)
}

func retryOnce[T any](ctx context.Context, wait time.Duration, fn func() (T, error)) (T, error) {
	res, err := fn()
	if err == nil || !IsRateLimitErr(err) {
		return res, err
	}
	var zero T
	select {
	case <-ctx.Done():
		return zero, ctx.Err()
	case <-time.After(wait):
	}
	return fn()
}

var thinkTags = regexp.MustCompile(`(?s)<think>.*?</think>`)

// StripThinkTags removes <think>...</think> reasoning blocks from a reply.
func StripThinkTags(text string) string {
	if !strings.Contains(text, "<think>") {
		return text
	}
	return strings.TrimSpace(thinkTags.ReplaceAllString(text, ""))
}
