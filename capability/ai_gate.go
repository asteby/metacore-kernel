package capability

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrAIForbidden: the caller may not use ai.complete for that feature (the
// addon is not installed or enabled in the org, or its manifest does not
// grant the feature). A programming/configuration error, never a balance
// problem.
var ErrAIForbidden = errors.New("capability: ai feature not granted")

// AIHostProvider is the provider key ai.complete results carry: the host
// answers, not an addon.
const AIHostProvider = "host"

// AICall is one ai.complete call as the host sees it. Org and Addon come from
// the session or the wasm invocation, never from the addon's input.
type AICall struct {
	Org   uuid.UUID
	Addon string
	// User is the operator behind a frontend call (audit only; it is never
	// sent to the hub).
	User string
	// Sponsor is the org the host asks to pay (set by AIGate from the policy;
	// uuid.Nil = the org pays).
	Sponsor uuid.UUID
	// PersonalData is the feature's declaration (set by AIGate): the provider
	// asks the hub to redact PII.
	PersonalData bool
	Request      AIRequest
}

// AIProvider runs a completion on the host's AI gateway (OPS: services/hubai
// against the hub). Implementations map gateway outcomes to AIStatus — no
// Pulse, budget, rate limit, gateway down are results, not errors — and
// return an error only for a request the gateway rejected as malformed
// (an *InputError).
type AIProvider interface {
	Complete(ctx context.Context, call AICall) (AIResult, error)
}

// AIProviderFunc adapts a function to AIProvider.
type AIProviderFunc func(ctx context.Context, call AICall) (AIResult, error)

// Complete implements AIProvider.
func (f AIProviderFunc) Complete(ctx context.Context, call AICall) (AIResult, error) {
	return f(ctx, call)
}

// AIPolicySource resolves what an addon may do in an org: its manifest grants
// and `ai` block, lowered by the org's host configuration. It returns an
// error wrapping ErrAIForbidden when the addon is not installed and enabled
// in the org.
type AIPolicySource interface {
	AIPolicy(ctx context.Context, org uuid.UUID, addon string) (AIPolicy, error)
}

// AIPolicySourceFunc adapts a function to AIPolicySource.
type AIPolicySourceFunc func(ctx context.Context, org uuid.UUID, addon string) (AIPolicy, error)

// AIPolicy implements AIPolicySource.
func (f AIPolicySourceFunc) AIPolicy(ctx context.Context, org uuid.UUID, addon string) (AIPolicy, error) {
	return f(ctx, org, addon)
}

// AIUsageRecord is one metered call. It never carries prompt or answer text.
type AIUsageRecord struct {
	Org        uuid.UUID
	Addon      string
	Feature    string
	Period     string
	Status     AIStatus
	CostSparks int64
	Usage      AIUsage
	Model      string
	RequestID  string
	BilledTo   string
	At         time.Time
}

// AIUsageMeter counts what each addon spent per org and period (host side;
// the hub's ledger stays the source of truth for money). Spent returns the
// addon total and the feature total of the period.
type AIUsageMeter interface {
	Spent(ctx context.Context, org uuid.UUID, addon, feature, period string) (addonSparks, featureSparks int64, err error)
	Record(ctx context.Context, rec AIUsageRecord) error
}

// AIRedactor rewrites a call before it leaves the host (mask e-mails,
// phones, tax ids, card numbers…). An error stops the call (fail closed:
// AIStatusUnavailable). The redactor may change the request text only; the
// gate restores org, addon, feature, sponsor and limits after it runs.
type AIRedactor func(ctx context.Context, call AICall) (AICall, error)

// AIDefaults are the host configuration values used when the manifest leaves
// a limit at zero. Zero here too means "no limit at this level" (the hub's
// org budget and Pulse still apply).
type AIDefaults struct {
	// Tier used when the request names none.
	Tier string
	// MaxTokens used when neither the request nor the feature sets one.
	MaxTokens int
	// MonthlySparksCap per addon when its manifest declares none.
	MonthlySparksCap int64
}

// AIGate is the host-neutral ai.complete pipeline: validate, authorize
// against the addon's policy, apply its caps, redact, call the provider,
// check JSON answers and meter. Hosts plug their gateway (Provider), their
// installed manifests (Policies) and their counters (Meter).
type AIGate struct {
	Provider AIProvider
	Policies AIPolicySource
	// Meter enforces the monthly caps and records spend; nil = no host caps
	// (only the hub's org budget and Pulse apply).
	Meter AIUsageMeter
	// Redact runs before the call leaves the host; nil = no host redaction.
	Redact   AIRedactor
	Defaults AIDefaults
	Now      func() time.Time
	// Logf receives one audit line per call (never the text); nil = silent.
	Logf func(format string, args ...any)
}

func (g *AIGate) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

func (g *AIGate) logf(format string, args ...any) {
	if g.Logf != nil {
		g.Logf(format, args...)
	}
}

// Complete runs one call. The error is reserved for caller bugs: an
// *InputError (ErrInvalidInput) or ErrAIForbidden. Every runtime condition —
// no Pulse, a cap reached, the gateway down — is an AIResult status.
func (g *AIGate) Complete(ctx context.Context, call AICall) (AIResult, error) {
	req := call.Request
	if problems := req.Validate(); len(problems) > 0 {
		return AIResult{}, &InputError{Capability: AIComplete, Problems: problems}
	}
	if call.Org == uuid.Nil {
		return AIResult{}, &InputError{Capability: AIComplete, Problems: []string{"no organization"}}
	}
	if strings.TrimSpace(call.Addon) == "" {
		return AIResult{}, fmt.Errorf("%w: ai.complete needs the calling addon", ErrAIForbidden)
	}
	if g == nil || g.Policies == nil || g.Provider == nil {
		return AIDegraded(AIStatusUnavailable, "the host has no AI gateway configured"), nil
	}
	policy, err := g.Policies.AIPolicy(ctx, call.Org, call.Addon)
	if err != nil {
		if errors.Is(err, ErrAIForbidden) {
			return AIResult{}, err
		}
		g.logf("audit ai.complete org=%s addon=%s feature=%s decision=error policy: %v", call.Org, call.Addon, req.Feature, err)
		return AIDegraded(AIStatusUnavailable, "could not read the addon's AI policy"), nil
	}
	feature, ok := policy.Feature(req.Feature)
	if !ok {
		return AIResult{}, fmt.Errorf("%w: addon %q does not declare ai:complete %q", ErrAIForbidden, call.Addon, req.Feature)
	}

	// Limits: the request may ask for less, never for more.
	if req.Tier == "" {
		req.Tier = g.Defaults.Tier
	}
	if req.Tier == "" {
		// Never leave the tier to the gateway's default when the feature
		// caps it: the gateway's default may be above the cap.
		req.Tier = feature.MaxTier
	}
	req.Tier = ClampAITier(req.Tier, feature.MaxTier)
	limit := feature.MaxTokens
	if limit <= 0 {
		limit = g.Defaults.MaxTokens
	}
	if limit > 0 && (req.MaxTokens <= 0 || req.MaxTokens > limit) {
		req.MaxTokens = limit
	}

	period := AIPeriod(g.now())
	addonCap := policy.MonthlySparksCap
	if addonCap <= 0 {
		addonCap = g.Defaults.MonthlySparksCap
	}
	if g.Meter != nil && (addonCap > 0 || feature.MonthlySparksCap > 0) {
		spentAddon, spentFeature, err := g.Meter.Spent(ctx, call.Org, call.Addon, req.Feature, period)
		if err != nil {
			// Fail closed: without the counter the caps cannot hold.
			g.logf("audit ai.complete org=%s addon=%s feature=%s decision=error meter: %v", call.Org, call.Addon, req.Feature, err)
			return AIDegraded(AIStatusUnavailable, "could not read the addon's AI usage"), nil
		}
		var cut string
		switch {
		case addonCap > 0 && spentAddon >= addonCap:
			cut = fmt.Sprintf("the addon spent its %d Sparks of AI for %s", addonCap, period)
		case feature.MonthlySparksCap > 0 && spentFeature >= feature.MonthlySparksCap:
			cut = fmt.Sprintf("the feature spent its %d Sparks of AI for %s", feature.MonthlySparksCap, period)
		}
		if cut != "" {
			res := AIDegraded(AIStatusQuotaExceeded, cut)
			g.record(ctx, call, req.Feature, period, res)
			return res, nil
		}
	}

	call.Request = req
	call.Sponsor = feature.Sponsor
	if call.Sponsor == uuid.Nil {
		call.Sponsor = policy.Sponsor
	}
	call.PersonalData = feature.PersonalData
	if g.Redact != nil {
		redacted, err := g.Redact(ctx, call)
		if err != nil {
			g.logf("audit ai.complete org=%s addon=%s feature=%s decision=error redaction: %v", call.Org, call.Addon, req.Feature, err)
			return AIDegraded(AIStatusUnavailable, "the pre-send redaction failed; nothing was sent"), nil
		}
		// The redactor rewrites text only.
		redacted.Org, redacted.Addon, redacted.User = call.Org, call.Addon, call.User
		redacted.Sponsor, redacted.PersonalData = call.Sponsor, call.PersonalData
		redacted.Request.Feature, redacted.Request.Tier = req.Feature, req.Tier
		redacted.Request.MaxTokens, redacted.Request.ResponseFormat = req.MaxTokens, req.ResponseFormat
		redacted.Request.IdempotencyKey = req.IdempotencyKey
		if problems := redacted.Request.Validate(); len(problems) > 0 {
			g.logf("audit ai.complete org=%s addon=%s feature=%s decision=error redaction produced an invalid request: %s", call.Org, call.Addon, req.Feature, strings.Join(problems, "; "))
			return AIDegraded(AIStatusUnavailable, "the pre-send redaction failed; nothing was sent"), nil
		}
		call = redacted
	}

	res, err := g.Provider.Complete(ctx, call)
	if err != nil {
		var ie *InputError
		if errors.As(err, &ie) {
			return AIResult{}, err
		}
		res = AIDegraded(AIStatusUnavailable, err.Error())
	}
	if res.Status == "" {
		res.Status = AIStatusOK
	}
	if res.BilledTo == "" && res.CostSparks > 0 {
		res.BilledTo = "org"
	}
	if res.OK() && req.ResponseFormat.WantsJSON() {
		raw := res.JSON
		if len(raw) == 0 {
			raw = []byte(res.Content)
		}
		if obj, ok := jsonObject(raw); ok {
			res.JSON = obj
		} else {
			res.Status, res.JSON = AIStatusInvalidOutput, nil
			res.Message = "the model did not return a JSON object"
		}
	}
	g.record(ctx, call, req.Feature, period, res)
	return res, nil
}

func (g *AIGate) record(ctx context.Context, call AICall, feature, period string, res AIResult) {
	g.logf("audit ai.complete org=%s addon=%s feature=%s status=%s cost_sparks=%d billed_to=%s request_id=%s",
		call.Org, call.Addon, feature, res.Status, res.CostSparks, res.BilledTo, res.RequestID)
	if g.Meter == nil {
		return
	}
	if err := g.Meter.Record(ctx, AIUsageRecord{
		Org: call.Org, Addon: call.Addon, Feature: feature, Period: period, Status: res.Status,
		CostSparks: res.CostSparks, Usage: res.Usage, Model: res.Model, RequestID: res.RequestID,
		BilledTo: res.BilledTo, At: g.now(),
	}); err != nil {
		g.logf("audit ai.complete org=%s addon=%s feature=%s meter record failed: %v", call.Org, call.Addon, feature, err)
	}
}

// jsonObject decodes raw (optionally wrapped in a ``` fence) as one JSON
// object and returns it compacted.
func jsonObject(raw []byte) (json.RawMessage, bool) {
	s := strings.TrimSpace(string(raw))
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```")
		if nl := strings.IndexByte(s, '\n'); nl >= 0 && !strings.HasPrefix(strings.TrimSpace(s[:nl]), "{") {
			s = s[nl+1:]
		}
		s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "```"))
	}
	var m map[string]any
	if json.Unmarshal([]byte(s), &m) != nil || m == nil {
		return nil, false
	}
	var buf bytes.Buffer
	if json.Compact(&buf, []byte(s)) != nil {
		return nil, false
	}
	return buf.Bytes(), true
}

// AIHostQuerier answers ai.complete with Gate and passes every other query
// to Inner. The host installs it as the capability querier of the wasm host
// (capability_query) so a guest reaches ai.complete like any capability; the
// addon key is the invocation's (Query.Caller), never the guest's input.
type AIHostQuerier struct {
	Inner Querier
	Gate  *AIGate
}

// Query implements Querier.
func (h AIHostQuerier) Query(ctx context.Context, q Query) (QueryResult, error) {
	if q.Capability != AIComplete {
		if h.Inner == nil {
			return QueryResult{Capability: q.Capability}, fmt.Errorf("%w: no querier", ErrNoProvider)
		}
		return h.Inner.Query(ctx, q)
	}
	res := QueryResult{Capability: q.Capability, Market: q.Market.Code}
	c, _ := Lookup(AIComplete)
	if problems := c.CheckInput(q.Input); len(problems) > 0 {
		return res, &InputError{Capability: c.Key, Problems: problems}
	}
	var req AIRequest
	raw, err := json.Marshal(q.Input)
	if err == nil {
		err = json.Unmarshal(raw, &req)
	}
	if err != nil {
		return res, &InputError{Capability: c.Key, Problems: []string{err.Error()}}
	}
	start := time.Now()
	out, err := h.Gate.Complete(ctx, AICall{Org: q.Org, Addon: q.Caller, Request: req})
	r := Result{Provider: AIHostProvider, Duration: time.Since(start)}
	switch {
	case err == nil:
		r.Output, _ = json.Marshal(out)
	case errors.Is(err, ErrInvalidInput):
		return res, err
	case errors.Is(err, ErrAIForbidden):
		r.Err = NewCodedError(ErrCodeForbidden, err.Error())
	default:
		r.Err = NewCodedError(ErrCodeInvalidInput, err.Error())
	}
	res.Results = []Result{r}
	return res, nil
}

// ListProviders implements ProviderLister for the hosts that expose it: the
// host is the only "provider" of ai.complete; other capabilities go to Inner.
func (h AIHostQuerier) ListProviders(ctx context.Context, q Query) (ProviderListing, error) {
	if q.Capability != AIComplete {
		if l, ok := h.Inner.(ProviderLister); ok {
			return l.ListProviders(ctx, q)
		}
		return ProviderListing{}, fmt.Errorf("%w: no provider lister", ErrNoProvider)
	}
	return ProviderListing{Capability: AIComplete, Market: q.Market.Code, Providers: []ProviderView{}, Resolution: "ok"}, nil
}
