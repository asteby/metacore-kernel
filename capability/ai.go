package capability

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// AIComplete is the host-implemented capability every addon uses for LLM
// inference: the addon declares `ai:complete <feature>` in its manifest
// capabilities and the HOST answers (no addon may provide it), running the
// completion through its AI gateway (OPS: the Hub, billed in Pulse Sparks).
// Addons never hold LLM keys. Reference: docs/capabilities-ai.md and
// docs/rfc/0007-capacidad-ai-complete.md.
const AIComplete = "ai.complete"

// AIPermissionKind is the manifest capability kind that grants one AI feature:
//
//	{"kind": "ai:complete", "target": "ticket_triage", "reason": "<purpose>"}
//
// The target is the feature key (no wildcards) and the reason — the purpose
// the org admin consents to at install — is mandatory.
const AIPermissionKind = "ai:complete"

// AI vocabularies.
var (
	// AITiers is the cost/quality intent of a call, cheapest first. The hub
	// maps a tier to a concrete model; callers never name a model.
	AITiers = []string{"fast", "balanced", "heavy"}
	// AIResponseFormats are the output shapes a caller may ask for.
	AIResponseFormats = []string{"text", "json_object", "json_schema"}
	// AIMessageRoles are the roles of a conversation turn.
	AIMessageRoles = []string{"system", "user", "assistant"}
	// AIBilledTo says whose Pulse paid a call.
	AIBilledTo = []string{"org", "sponsor"}
)

// AIStatus is the outcome of an ai.complete call. Everything but AIStatusOK
// is a NORMAL result the addon renders (no credits this month, try later):
// running out of Pulse never throws.
type AIStatus string

const (
	AIStatusOK AIStatus = "ok"
	// AIStatusInsufficientPulse: the paying org has no Pulse left this month
	// (hub 402 insufficient_pulse).
	AIStatusInsufficientPulse AIStatus = "insufficient_pulse"
	// AIStatusQuotaExceeded: the addon's or the feature's monthly Sparks cap
	// (manifest `ai` block or host configuration) is spent. Decided by the
	// host before calling the hub.
	AIStatusQuotaExceeded AIStatus = "quota_exceeded"
	// AIStatusBudgetExceeded: the org's money budget at the hub is spent
	// (hub 429 budget_exceeded).
	AIStatusBudgetExceeded AIStatus = "budget_exceeded"
	// AIStatusRateLimited: too many calls; retry after RetryAfterSeconds.
	AIStatusRateLimited AIStatus = "rate_limited"
	// AIStatusDisabled: AI is paused for the org or the instance (hub
	// budget_disabled / banned, or the host has AI turned off).
	AIStatusDisabled AIStatus = "disabled"
	// AIStatusNoWallet: the paying org has no Sparks wallet at the hub.
	AIStatusNoWallet AIStatus = "no_wallet"
	// AIStatusUnavailable: the hub or the network failed, the host has no
	// gateway, or the pre-send redaction failed (fail closed).
	AIStatusUnavailable AIStatus = "unavailable"
	// AIStatusInvalidOutput: a JSON response_format was asked and the model
	// did not return a JSON object. The Sparks WERE spent.
	AIStatusInvalidOutput AIStatus = "invalid_output"
)

// AIStatuses lists every status, ok first. New statuses are appended.
var AIStatuses = []string{
	string(AIStatusOK), string(AIStatusInsufficientPulse), string(AIStatusQuotaExceeded),
	string(AIStatusBudgetExceeded), string(AIStatusRateLimited), string(AIStatusDisabled),
	string(AIStatusNoWallet), string(AIStatusUnavailable), string(AIStatusInvalidOutput),
}

// ErrCodeForbidden is the coded error of a call the caller may not make
// (ai.complete: a feature its manifest does not grant).
const ErrCodeForbidden = "forbidden"

// AI limits that are protocol facts, not product decisions (product limits —
// caps, default tier, max tokens — come from the manifest and the host
// configuration).
const (
	// MaxAIIdempotencyKey is the longest idempotency_key accepted.
	MaxAIIdempotencyKey = 128
	// MaxAIFeatureKey is the longest feature key.
	MaxAIFeatureKey = 48
	// AITimeout is the per-call deadline of ai.complete as a capability
	// query: a heavy completion on a slow provider chain.
	AITimeout = 120 * time.Second
)

var aiFeatureRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,47}$`)

// ValidAIFeature reports whether key is a well-formed AI feature key.
func ValidAIFeature(key string) bool { return aiFeatureRe.MatchString(key) }

// AITierRank is the position of tier in AITiers (-1 when unknown).
func AITierRank(tier string) int {
	for i, t := range AITiers {
		if t == tier {
			return i
		}
	}
	return -1
}

// ClampAITier lowers tier to max when it asks for more. An empty or unknown
// max leaves tier alone; an empty tier stays empty (the host default applies).
func ClampAITier(tier, max string) string {
	if tier == "" || AITierRank(max) < 0 {
		return tier
	}
	if AITierRank(tier) > AITierRank(max) {
		return max
	}
	return tier
}

// AIPeriod is the metering period of t: the UTC calendar month ("2026-11"),
// the same period Pulse resets on.
func AIPeriod(t time.Time) string { return t.UTC().Format("2006-01") }

// AIMessage is one conversation turn.
type AIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// AIResponseFormat asks for a structured answer.
type AIResponseFormat struct {
	// Type is text (default), json_object or json_schema.
	Type string `json:"type"`
	// Name labels a json_schema (optional).
	Name string `json:"name,omitempty"`
	// Schema is the JSON Schema of a json_schema answer.
	Schema json.RawMessage `json:"schema,omitempty"`
}

// WantsJSON reports whether the answer must be a JSON object.
func (f *AIResponseFormat) WantsJSON() bool {
	return f != nil && (f.Type == "json_object" || f.Type == "json_schema")
}

// AIRequest is the ai.complete input. The org, the addon, the user and a
// sponsor are NOT part of it: the host stamps them from the session or the
// invocation, so an addon cannot choose who pays.
type AIRequest struct {
	Feature        string            `json:"feature"`
	Messages       []AIMessage       `json:"messages,omitempty"`
	Prompt         string            `json:"prompt,omitempty"`
	System         string            `json:"system,omitempty"`
	Tier           string            `json:"tier,omitempty"`
	ResponseFormat *AIResponseFormat `json:"response_format,omitempty"`
	MaxTokens      int               `json:"max_tokens,omitempty"`
	Temperature    *float64          `json:"temperature,omitempty"`
	IdempotencyKey string            `json:"idempotency_key,omitempty"`
}

// Validate checks the request against the contract rules the schema cannot
// express. It returns every problem found.
func (r AIRequest) Validate() []string {
	var errs []string
	if !ValidAIFeature(r.Feature) {
		errs = append(errs, fmt.Sprintf("feature %q must match %s", r.Feature, aiFeatureRe))
	}
	if len(r.Messages) == 0 && strings.TrimSpace(r.Prompt) == "" {
		errs = append(errs, "messages or prompt is required")
	}
	if len(r.Messages) > 0 && strings.TrimSpace(r.Prompt) != "" {
		errs = append(errs, "send messages or prompt, not both")
	}
	hasUser := strings.TrimSpace(r.Prompt) != ""
	for i, m := range r.Messages {
		if !contains(AIMessageRoles, m.Role) {
			errs = append(errs, fmt.Sprintf("messages[%d].role %q is not one of %s", i, m.Role, strings.Join(AIMessageRoles, "|")))
		}
		if strings.TrimSpace(m.Content) == "" {
			errs = append(errs, fmt.Sprintf("messages[%d].content is empty", i))
		}
		hasUser = hasUser || m.Role == "user"
	}
	if len(r.Messages) > 0 && !hasUser {
		errs = append(errs, "messages needs at least one user turn")
	}
	if r.Tier != "" && AITierRank(r.Tier) < 0 {
		errs = append(errs, fmt.Sprintf("tier %q is not one of %s", r.Tier, strings.Join(AITiers, "|")))
	}
	if f := r.ResponseFormat; f != nil {
		if !contains(AIResponseFormats, f.Type) {
			errs = append(errs, fmt.Sprintf("response_format.type %q is not one of %s", f.Type, strings.Join(AIResponseFormats, "|")))
		}
		if f.Type == "json_schema" {
			var s map[string]any
			if len(f.Schema) == 0 || json.Unmarshal(f.Schema, &s) != nil {
				errs = append(errs, "response_format.schema must be a JSON Schema object when type is json_schema")
			}
		} else if len(f.Schema) > 0 {
			errs = append(errs, "response_format.schema is only allowed when type is json_schema")
		}
	}
	if r.MaxTokens < 0 {
		errs = append(errs, "max_tokens must be >= 0")
	}
	if t := r.Temperature; t != nil && (*t < 0 || *t > 2) {
		errs = append(errs, "temperature must be within [0, 2]")
	}
	if len(r.IdempotencyKey) > MaxAIIdempotencyKey {
		errs = append(errs, fmt.Sprintf("idempotency_key is longer than %d characters", MaxAIIdempotencyKey))
	}
	return errs
}

// Conversation is the request as one message list: System first, then
// Messages, or the Prompt as a single user turn.
func (r AIRequest) Conversation() []AIMessage {
	out := make([]AIMessage, 0, len(r.Messages)+2)
	if s := strings.TrimSpace(r.System); s != "" {
		out = append(out, AIMessage{Role: "system", Content: s})
	}
	if p := strings.TrimSpace(r.Prompt); p != "" {
		return append(out, AIMessage{Role: "user", Content: p})
	}
	return append(out, r.Messages...)
}

// AIUsage is the token count the hub reports.
type AIUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// AIResult is the ai.complete output. Status tells whether there is an
// answer; a non-ok status is a normal outcome, not an error.
type AIResult struct {
	Status            AIStatus        `json:"status"`
	Content           string          `json:"content,omitempty"`
	JSON              json.RawMessage `json:"json,omitempty"`
	Usage             AIUsage         `json:"usage"`
	CostSparks        int64           `json:"cost_sparks"`
	Model             string          `json:"model,omitempty"`
	RequestID         string          `json:"request_id,omitempty"`
	Message           string          `json:"message,omitempty"`
	RetryAfterSeconds int             `json:"retry_after_seconds,omitempty"`
	BilledTo          string          `json:"billed_to,omitempty"`
	Replay            bool            `json:"replay,omitempty"`
}

// OK reports whether the result carries an answer.
func (r AIResult) OK() bool { return r.Status == AIStatusOK }

// AIDegraded builds a result without an answer.
func AIDegraded(status AIStatus, message string) AIResult {
	return AIResult{Status: status, Message: message}
}

// AISpec is the optional `ai` block of an addon manifest (host manifest and
// v3 share it): the LIMITS of the features its `ai:complete` capabilities
// grant. A zero value means "no own limit": the host configuration applies.
type AISpec struct {
	// MonthlySparksCap caps what the addon may spend per org and month,
	// across features (0 = the host default).
	MonthlySparksCap int64 `json:"monthly_sparks_cap,omitempty"`
	// Features are per-feature limits; each key must be granted by an
	// ai:complete capability.
	Features []AIFeatureSpec `json:"features,omitempty"`
}

// AIFeatureSpec limits one AI feature.
type AIFeatureSpec struct {
	Key string `json:"key"`
	// MaxTier is the most expensive tier the feature may use (empty = any).
	MaxTier string `json:"max_tier,omitempty"`
	// MaxTokens caps the output tokens of one call (0 = the host default).
	MaxTokens int `json:"max_tokens,omitempty"`
	// MonthlySparksCap caps the feature per org and month (0 = only the
	// addon cap).
	MonthlySparksCap int64 `json:"monthly_sparks_cap,omitempty"`
	// PersonalData declares that the feature sends personal data (names,
	// contact data, free text written by customers). The install consent
	// shows it and the host asks the hub to redact PII.
	PersonalData bool `json:"personal_data,omitempty"`
}

// AIGrant is one `ai:complete` capability of a manifest.
type AIGrant struct {
	Feature string
	Purpose string
}

// ValidateAIDeclaration checks the ai:complete grants of a manifest together
// with its `ai` block. Grant problems are reported with their index in
// grants; spec problems with their path under "ai".
func ValidateAIDeclaration(grants []AIGrant, spec *AISpec) []string {
	var errs []string
	granted := map[string]bool{}
	for i, g := range grants {
		if !ValidAIFeature(g.Feature) {
			errs = append(errs, fmt.Sprintf("ai:complete grant %d: target %q must be a feature key matching %s (no wildcards)", i, g.Feature, aiFeatureRe))
		}
		if strings.TrimSpace(g.Purpose) == "" {
			errs = append(errs, fmt.Sprintf("ai:complete grant %d (%s): reason is required (the purpose the admin consents to)", i, g.Feature))
		}
		if granted[g.Feature] {
			errs = append(errs, fmt.Sprintf("ai:complete grant %d: feature %q is granted twice", i, g.Feature))
		}
		granted[g.Feature] = true
	}
	if spec == nil {
		return errs
	}
	if spec.MonthlySparksCap < 0 {
		errs = append(errs, "ai.monthly_sparks_cap must be >= 0")
	}
	seen := map[string]bool{}
	for i, f := range spec.Features {
		w := fmt.Sprintf("ai.features[%d]", i)
		switch {
		case !ValidAIFeature(f.Key):
			errs = append(errs, fmt.Sprintf("%s.key %q must match %s", w, f.Key, aiFeatureRe))
		case seen[f.Key]:
			errs = append(errs, fmt.Sprintf("%s.key %q is duplicated", w, f.Key))
		case !granted[f.Key]:
			errs = append(errs, fmt.Sprintf("%s.key %q has no ai:complete capability granting it", w, f.Key))
		}
		seen[f.Key] = true
		if f.MaxTier != "" && AITierRank(f.MaxTier) < 0 {
			errs = append(errs, fmt.Sprintf("%s.max_tier %q is not one of %s", w, f.MaxTier, strings.Join(AITiers, "|")))
		}
		if f.MaxTokens < 0 {
			errs = append(errs, w+".max_tokens must be >= 0")
		}
		if f.MonthlySparksCap < 0 {
			errs = append(errs, w+".monthly_sparks_cap must be >= 0")
		}
		if spec.MonthlySparksCap > 0 && f.MonthlySparksCap > spec.MonthlySparksCap {
			errs = append(errs, fmt.Sprintf("%s.monthly_sparks_cap %d exceeds ai.monthly_sparks_cap %d", w, f.MonthlySparksCap, spec.MonthlySparksCap))
		}
	}
	return errs
}

// AIFeaturePolicy is the resolved policy of one feature an addon may call.
type AIFeaturePolicy struct {
	Key              string
	Purpose          string
	MaxTier          string
	MaxTokens        int
	MonthlySparksCap int64
	PersonalData     bool
	// Sponsor overrides AIPolicy.Sponsor for this feature (host
	// configuration only).
	Sponsor uuid.UUID
}

// AIMonthlyCapSetting is the installation-settings key (next to
// DefaultsSetting / MarketsSetting) where an org admin LOWERS an addon's
// monthly Sparks cap: an integer; the effective cap is LowerCap(manifest,
// setting). It can never raise the manifest's cap.
const AIMonthlyCapSetting = "_ai_monthly_sparks_cap"

// AIPolicy is what an addon may do with ai.complete in one org: its granted
// features and their limits (manifest), possibly lowered by the org's host
// configuration, plus an optional sponsor.
type AIPolicy struct {
	Addon            string
	MonthlySparksCap int64
	Features         map[string]AIFeaturePolicy
	// Sponsor, when set, is the org the host ASKS to pay (the hub authorizes
	// it against an explicit grant of that org; without one the org pays).
	// Host configuration only, never the manifest. Off by default.
	Sponsor uuid.UUID
}

// Feature returns the policy of a granted feature.
func (p AIPolicy) Feature(key string) (AIFeaturePolicy, bool) {
	f, ok := p.Features[key]
	return f, ok
}

// FeatureKeys lists the granted features, sorted.
func (p AIPolicy) FeatureKeys() []string {
	out := make([]string, 0, len(p.Features))
	for k := range p.Features {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// NewAIPolicy builds the policy of addon from its grants and `ai` block.
// Features without an `ai.features[]` entry get zero limits (host defaults).
func NewAIPolicy(addon string, grants []AIGrant, spec *AISpec) AIPolicy {
	p := AIPolicy{Addon: addon, Features: map[string]AIFeaturePolicy{}}
	for _, g := range grants {
		if ValidAIFeature(g.Feature) {
			p.Features[g.Feature] = AIFeaturePolicy{Key: g.Feature, Purpose: strings.TrimSpace(g.Purpose)}
		}
	}
	if spec == nil {
		return p
	}
	p.MonthlySparksCap = spec.MonthlySparksCap
	for _, f := range spec.Features {
		fp, ok := p.Features[f.Key]
		if !ok {
			continue
		}
		fp.MaxTier, fp.MaxTokens, fp.MonthlySparksCap, fp.PersonalData = f.MaxTier, f.MaxTokens, f.MonthlySparksCap, f.PersonalData
		p.Features[f.Key] = fp
	}
	return p
}

// LowerCap returns the stricter of two caps where 0 means "no cap".
func LowerCap(a, b int64) int64 {
	switch {
	case a <= 0:
		return b
	case b <= 0:
		return a
	case a < b:
		return a
	}
	return b
}

var (
	// AIMessageSchema is one conversation turn.
	AIMessageSchema = schema(
		enum(req("role", TypeString, "system | user | assistant"), AIMessageRoles),
		req("content", TypeString, "turn text"),
	)
	// AIResponseFormatSchema asks for a structured answer.
	AIResponseFormatSchema = schema(
		enum(req("type", TypeString, "text (default) | json_object | json_schema"), AIResponseFormats),
		opt("name", TypeString, "label of a json_schema"),
		opt("schema", TypeObject, "JSON Schema of the answer; json_schema only"),
	)
	// AIUsageSchema is the token count of a call.
	AIUsageSchema = schema(
		req("input_tokens", TypeInteger, ""),
		req("output_tokens", TypeInteger, ""),
	)
)

func init() {
	mustRegister(aiCompleteContract())
}

func aiCompleteContract() Contract {
	return Contract{
		Key: AIComplete, Kind: KindQuery, Cardinality: One, Timeout: AITimeout, HostImplemented: true,
		Doc: "LLM completion run by the host's AI gateway and billed to the org's Pulse Sparks; non-ok statuses are normal results",
		Input: schema(
			pattern(req("feature", TypeString, "feature granted by an ai:complete capability; attributes the spend and selects the limits"), aiFeatureRe.String()),
			list(opt("messages", TypeArray, "conversation turns; one of messages / prompt"), AIMessageSchema),
			opt("prompt", TypeString, "single user turn; one of messages / prompt"),
			opt("system", TypeString, "system instruction, sent first"),
			enum(opt("tier", TypeString, "cost/quality intent; clamped to the feature max_tier and the org's; empty = host default"), AITiers),
			obj(opt("response_format", TypeObject, "structured answer: json_object / json_schema"), AIResponseFormatSchema),
			opt("max_tokens", TypeInteger, "output cap; clamped to the feature max_tokens; 0 = feature or host default"),
			opt("temperature", TypeNumber, "0..2"),
			opt("idempotency_key", TypeString, "same key = same result, charged once (max 128 chars)"),
		),
		Output: schema(
			enum(req("status", TypeString, "ok, or why there is no answer (a normal result, not an error)"), AIStatuses),
			opt("content", TypeString, "answer text (status ok)"),
			opt("json", TypeObject, "the answer decoded, when a JSON response_format was asked"),
			obj(req("usage", TypeObject, "tokens reported by the hub"), AIUsageSchema),
			req("cost_sparks", TypeInteger, "Pulse Sparks the hub debited (0 on replay or when no call was made)"),
			opt("model", TypeString, "model that answered (informative)"),
			opt("request_id", TypeString, "hub request id"),
			opt("message", TypeString, "human-readable reason when status is not ok"),
			opt("retry_after_seconds", TypeInteger, "with rate_limited"),
			enum(opt("billed_to", TypeString, "whose Pulse paid"), AIBilledTo),
			opt("replay", TypeBoolean, "result repeated for the same idempotency_key"),
		),
		Errors: []string{ErrCodeInvalidInput, ErrCodeForbidden},
	}
}
