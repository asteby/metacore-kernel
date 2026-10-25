package capability

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Market scopes a query to one of the organization's markets (H13): its code
// (org-defined, e.g. "mx", "us") and the ISO countries it sells to. The zero
// Market means "organization level": no market selection, no country filter.
type Market struct {
	Code      string   `json:"code,omitempty"`
	Countries []string `json:"countries,omitempty"`
}

// Provider is one installed addon that provides a capability in an org, as
// the host resolved it from manifests (provides_capabilities[]) and the
// installation settings.
type Provider struct {
	// Addon is the provider addon key.
	Addon string
	// Label is the human name of the provider binding.
	Label string
	// Countries the provider declared it serves (manifest
	// provides_capabilities[].countries); empty = any country.
	Countries []string
	// Default marks the org-level default provider of the capability.
	Default bool
	// Markets are the market codes the org explicitly selected this provider
	// for. A market selection beats the org default and the country filter.
	Markets []string
	// Input is the provider's own mapping (provides_capabilities[].input):
	// the Executor turns the contract input into the provider payload with
	// Resolve(Input, nil, contractInput).
	Input map[string]string
	// Timeout overrides the per-provider deadline (0 = contract/executor).
	Timeout time.Duration
	// Ref is opaque host data (dispatch target, manifest row …) handed back
	// to the Invoker untouched.
	Ref any
}

// Serves reports whether the provider declared support for any country of
// the market (or declared none, or the market has none).
func (p Provider) Serves(m Market) bool {
	if len(p.Countries) == 0 || len(m.Countries) == 0 {
		return true
	}
	for _, c := range p.Countries {
		for _, mc := range m.Countries {
			if strings.EqualFold(c, mc) {
				return true
			}
		}
	}
	return false
}

// SelectedFor reports whether the org selected this provider for market code.
func (p Provider) SelectedFor(code string) bool {
	if code == "" {
		return false
	}
	for _, m := range p.Markets {
		if strings.EqualFold(m, code) {
			return true
		}
	}
	return false
}

var (
	// ErrNoProvider: no installed provider is eligible for the org/market.
	ErrNoProvider = errors.New("capability: no provider")
	// ErrAmbiguousProvider: several eligible providers and none selected for
	// the market nor marked as the org default.
	ErrAmbiguousProvider = errors.New("capability: ambiguous provider")
	// ErrNotQuery: the contract is an action, it returns no output.
	ErrNotQuery = errors.New("capability: not a query contract")
	// ErrInvalidInput: the contract input misses required fields or has
	// mistyped ones (see InputError).
	ErrInvalidInput = errors.New("capability: invalid input")
	// ErrTimeout: the provider did not answer within its deadline.
	ErrTimeout = errors.New("capability: provider timeout")
	// ErrInvalidOutput: the provider answered something that is not the
	// contract's output (see OutputError).
	ErrInvalidOutput = errors.New("capability: invalid provider output")
	// ErrProviderPanic: the invoker panicked.
	ErrProviderPanic = errors.New("capability: provider panicked")
	// ErrProviderRequired: a follow-up contract (Contract.PinRequired) was
	// queried without naming the provider (Query.Provider).
	ErrProviderRequired = errors.New("capability: provider required")
	// ErrIdempotencyRequired: a mutating contract (Contract.Idempotent) was
	// queried without a non-blank idempotency_key.
	ErrIdempotencyRequired = errors.New("capability: idempotency_key required")
	// ErrIndeterminate: the outcome of a mutating call is unknown (the
	// deadline passed or the caller gave up while the provider may still
	// complete it). Never retry with a new key: reconcile first
	// (payments.status, shipping.track …) or retry with the SAME key.
	ErrIndeterminate = errors.New("capability: indeterminate outcome")
)

// SelectionError details ErrNoProvider / ErrAmbiguousProvider.
type SelectionError struct {
	Capability string
	Market     string
	// Candidates are the eligible providers when ambiguous, or every
	// installed provider when none was eligible.
	Candidates []string
	err        error
}

func (e *SelectionError) Error() string {
	where := e.Capability
	if e.Market != "" {
		where += " (market " + e.Market + ")"
	}
	if len(e.Candidates) == 0 {
		return fmt.Sprintf("%v for %s", e.err, where)
	}
	return fmt.Sprintf("%v for %s: %s", e.err, where, strings.Join(e.Candidates, ", "))
}

func (e *SelectionError) Unwrap() error { return e.err }

// InputError details ErrInvalidInput.
type InputError struct {
	Capability string
	Problems   []string
}

func (e *InputError) Error() string {
	return fmt.Sprintf("capability %s: invalid input: %s", e.Capability, strings.Join(e.Problems, "; "))
}

func (e *InputError) Unwrap() error { return ErrInvalidInput }

// OutputError details ErrInvalidOutput.
type OutputError struct {
	Capability string
	Provider   string
	Problems   []string
}

func (e *OutputError) Error() string {
	return fmt.Sprintf("capability %s: provider %s returned an invalid output: %s", e.Capability, e.Provider, strings.Join(e.Problems, "; "))
}

func (e *OutputError) Unwrap() error { return ErrInvalidOutput }

// Select resolves which providers a call reaches for (org, market,
// capability); providers are the ones installed+enabled in the org.
//
// Cardinality One:
//  1. the provider the org selected for the market (Markets ∋ m.Code);
//  2. else, among the providers serving the market's countries, the org
//     default;
//  3. else the only provider serving the market;
//  4. else ErrAmbiguousProvider (several) / ErrNoProvider (none).
//
// Cardinality All: every provider serving the market's countries — unless
// the org selected some providers for this market explicitly, in which case
// exactly those (a market opting into a subset of carriers).
//
// The result is sorted by addon key (stable fan-out order).
func Select(c Contract, providers []Provider, m Market) ([]Provider, error) {
	sorted := append([]Provider(nil), providers...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Addon < sorted[j].Addon })

	var selected, eligible []Provider
	for _, p := range sorted {
		if p.SelectedFor(m.Code) {
			selected = append(selected, p)
		}
		if p.Serves(m) {
			eligible = append(eligible, p)
		}
	}
	fail := func(err error, ps []Provider) error {
		e := &SelectionError{Capability: c.Key, Market: m.Code, err: err}
		for _, p := range ps {
			e.Candidates = append(e.Candidates, p.Addon)
		}
		return e
	}
	if c.Fanout() {
		if len(selected) > 0 {
			return selected, nil
		}
		if len(eligible) == 0 {
			return nil, fail(ErrNoProvider, sorted)
		}
		return eligible, nil
	}
	switch len(selected) {
	case 1:
		return selected, nil
	case 0:
	default:
		return nil, fail(ErrAmbiguousProvider, selected)
	}
	var defaults []Provider
	for _, p := range eligible {
		if p.Default {
			defaults = append(defaults, p)
		}
	}
	switch {
	case len(defaults) == 1:
		return defaults, nil
	case len(defaults) > 1:
		return nil, fail(ErrAmbiguousProvider, defaults)
	case len(eligible) == 1:
		return eligible, nil
	case len(eligible) == 0:
		return nil, fail(ErrNoProvider, sorted)
	}
	return nil, fail(ErrAmbiguousProvider, eligible)
}

// SelectPinned resolves the single provider a follow-up call is bound to: the
// provider that created the payment / shipment / invoice, stored by the host
// on its record. It must be installed and enabled in the org (present in
// providers) and still serve the market's countries; otherwise ErrNoProvider.
// Market selection, org default and ambiguity play no role: the record already
// names the provider.
func SelectPinned(c Contract, providers []Provider, m Market, addon string) ([]Provider, error) {
	var all []string
	for _, p := range providers {
		all = append(all, p.Addon)
		if addon != "" && p.Addon == addon && p.Serves(m) {
			return []Provider{p}, nil
		}
	}
	sort.Strings(all)
	return nil, &SelectionError{Capability: c.Key, Market: m.Code, Candidates: all, err: ErrNoProvider}
}

// Invoker runs one provider: the host dispatches payload (the provider
// payload, JSON object) to the provider's wasm export / native operation for
// org and returns its raw JSON output. It must honour ctx's deadline; the
// Executor stops waiting at the deadline either way.
type Invoker func(ctx context.Context, call Call) (json.RawMessage, error)

// Call is one provider invocation handed to the Invoker.
type Call struct {
	Org        uuid.UUID
	Market     string
	Capability string
	Provider   Provider
	// IdempotencyKey is the contract input's idempotency_key ("" for
	// non-mutating contracts). Providers must dedupe on it: the Executor may
	// give up on a slow call that still completes, and the caller then retries
	// with the same key.
	IdempotencyKey string
	// Payload is the provider payload: the contract input passed through the
	// provider's Input mapping.
	Payload json.RawMessage
}

// Result is one provider's answer. Exactly one of Output / Err is set.
type Result struct {
	Provider string
	Output   json.RawMessage
	Err      error
	Duration time.Duration
	// reported is the coded error the provider answered even when its code is
	// not declared by the contract (Err then only keeps the message); it feeds
	// Failure.Reported.
	reported *CodedError
}

// OK reports whether the provider answered a valid output.
func (r Result) OK() bool { return r.Err == nil }

// Indeterminate reports whether a mutating call's outcome is unknown (it may
// have succeeded at the provider); reconcile before retrying.
func (r Result) Indeterminate() bool { return errors.Is(r.Err, ErrIndeterminate) }

// ErrorCode classifies Err: timeout | invalid_output | panic | canceled |
// a code the contract declares in Contract.Errors that the provider reported
// with NewCodedError (e.g. not_found) | provider_error ("" when OK).
func (r Result) ErrorCode() string {
	var coded *CodedError
	switch {
	case r.Err == nil:
		return ""
	case errors.Is(r.Err, ErrTimeout):
		return "timeout"
	case errors.Is(r.Err, ErrInvalidOutput):
		return "invalid_output"
	case errors.Is(r.Err, ErrProviderPanic):
		return "panic"
	case errors.Is(r.Err, context.Canceled):
		return "canceled"
	case errors.As(r.Err, &coded) && coded.Code != "":
		return coded.Code
	}
	return "provider_error"
}

// CodedError is a provider failure with a contract-level code (one of
// Contract.Errors: "invalid_input", "not_found" …). The Invoker returns it
// when the provider answered a typed error — a host that maps a guest's
// `{success:false, error:{code, message}}` envelope builds it with
// NewCodedError — so the consumer can branch on the code instead of parsing
// messages. A code the contract does not declare is reported as
// provider_error.
type CodedError struct {
	Code    string
	Message string
}

// NewCodedError builds a CodedError.
func NewCodedError(code, message string) *CodedError {
	return &CodedError{Code: code, Message: message}
}

func (e *CodedError) Error() string {
	if e.Message == "" {
		return e.Code
	}
	return e.Code + ": " + e.Message
}

// QueryResult is what a query returns: one Result per provider reached, in
// provider order. A fan-out never fails as a whole because some providers
// failed: their errors sit next to the outputs that arrived.
type QueryResult struct {
	Capability string
	Market     string
	Results    []Result
}

// Outputs returns the successful results.
func (q QueryResult) Outputs() []Result {
	var out []Result
	for _, r := range q.Results {
		if r.OK() {
			out = append(out, r)
		}
	}
	return out
}

// Failed returns the failed results.
func (q QueryResult) Failed() []Result {
	var out []Result
	for _, r := range q.Results {
		if !r.OK() {
			out = append(out, r)
		}
	}
	return out
}

// One returns the single provider's output or its error (cardinality one).
func (q QueryResult) One() (json.RawMessage, error) {
	if len(q.Results) == 0 {
		return nil, &SelectionError{Capability: q.Capability, Market: q.Market, err: ErrNoProvider}
	}
	r := q.Results[0]
	return r.Output, r.Err
}

// ProviderError is a failed provider in the wire envelope.
type ProviderError struct {
	Provider string `json:"provider"`
	Code     string `json:"code"`
	Message  string `json:"message"`
	// Indeterminate: a mutating call that may still have succeeded.
	Indeterminate bool `json:"indeterminate,omitempty"`
}

// ProviderOutput is a successful provider in the wire envelope.
type ProviderOutput struct {
	Provider string          `json:"provider"`
	Output   json.RawMessage `json:"output"`
}

// Envelope is the JSON shape of a QueryResult for consumers (WASM guests,
// HTTP):
//
//	{"capability":"commerce.shipping.rates","market":"mx",
//	 "results":[{"provider":"skydropx","output":{"rates":[…]}}],
//	 "errors":[{"provider":"envia","code":"timeout","message":"…"}]}
type Envelope struct {
	Capability string           `json:"capability"`
	Market     string           `json:"market,omitempty"`
	Results    []ProviderOutput `json:"results"`
	Errors     []ProviderError  `json:"errors"`
}

// Envelope converts the result to its wire shape.
func (q QueryResult) Envelope() Envelope {
	env := Envelope{Capability: q.Capability, Market: q.Market, Results: []ProviderOutput{}, Errors: []ProviderError{}}
	for _, r := range q.Results {
		if r.OK() {
			env.Results = append(env.Results, ProviderOutput{Provider: r.Provider, Output: r.Output})
			continue
		}
		env.Errors = append(env.Errors, ProviderError{Provider: r.Provider, Code: r.ErrorCode(), Message: r.Err.Error(), Indeterminate: r.Indeterminate()})
	}
	return env
}

// MarshalJSON renders the Envelope.
func (q QueryResult) MarshalJSON() ([]byte, error) { return json.Marshal(q.Envelope()) }

// Executor runs a query over already-selected providers: one goroutine per
// provider, each under its own deadline, panics isolated, outputs validated.
// The zero value is ready to use.
type Executor struct {
	// Timeout is the default per-provider deadline when neither the provider
	// nor the contract sets one (0 = DefaultQueryTimeout).
	Timeout time.Duration
	// MaxParallel caps concurrent provider calls of one fan-out (0 = all at
	// once).
	MaxParallel int
	// SkipOutputValidation trusts provider outputs (tests, benchmarks).
	SkipOutputValidation bool
	// Now is the clock (tests); nil = time.Now.
	Now func() time.Time
	// OnFailure, when set, is told about every provider that failed (timeout,
	// panic, invalid_output, a provider-reported code…) so the host can keep a
	// diagnostic. It runs on the provider's goroutine, must not block and its
	// panics are swallowed: it can never change the query result.
	OnFailure func(ctx context.Context, f Failure)
}

// Failure describes one failed provider call for Executor.OnFailure. It carries
// no payload or output: only who failed and how.
type Failure struct {
	Org        uuid.UUID
	Market     string
	Capability string
	// Provider is the addon key of the provider that failed.
	Provider string
	// Code is Result.ErrorCode(): timeout | invalid_output | panic | canceled |
	// a contract code | provider_error.
	Code          string
	Indeterminate bool
	Duration      time.Duration
	Err           error
	// Reported is what the provider itself answered (code + message) when it
	// replied with a typed error, whether or not the contract declares the code.
	Reported *CodedError
}

func (e Executor) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e Executor) timeoutFor(c Contract, p Provider) time.Duration {
	switch {
	case p.Timeout > 0:
		return p.Timeout
	case c.Timeout > 0:
		return c.Timeout
	case e.Timeout > 0:
		return e.Timeout
	}
	return DefaultQueryTimeout
}

// Run invokes the providers (already chosen by Select) with input — the
// resolved contract input — and returns one Result per provider in order.
// It fails as a whole only before invoking anything: not a query, invalid
// input, or no providers. A cardinality-one contract invokes providers[0]
// only.
func (e Executor) Run(ctx context.Context, c Contract, org uuid.UUID, market string, providers []Provider, input map[string]any, invoke Invoker) (QueryResult, error) {
	res := QueryResult{Capability: c.Key, Market: market}
	if !c.IsQuery() {
		return res, fmt.Errorf("%w: %s", ErrNotQuery, c.Key)
	}
	if invoke == nil {
		return res, errors.New("capability: nil invoker")
	}
	if problems := c.CheckInput(input); len(problems) > 0 {
		return res, &InputError{Capability: c.Key, Problems: problems}
	}
	if c.Idempotent {
		if k, _ := input["idempotency_key"].(string); strings.TrimSpace(k) == "" {
			return res, fmt.Errorf("%w: %s", ErrIdempotencyRequired, c.Key)
		}
	}
	if len(providers) == 0 {
		return res, &SelectionError{Capability: c.Key, Market: market, err: ErrNoProvider}
	}
	if !c.Fanout() {
		providers = providers[:1]
	}
	res.Results = make([]Result, len(providers))
	var sem chan struct{}
	if e.MaxParallel > 0 {
		sem = make(chan struct{}, e.MaxParallel)
	}
	var wg sync.WaitGroup
	for i, p := range providers {
		wg.Add(1)
		go func(i int, p Provider) {
			defer wg.Done()
			if sem != nil {
				select {
				case sem <- struct{}{}:
					defer func() { <-sem }()
				case <-ctx.Done():
					res.Results[i] = Result{Provider: p.Addon, Err: ctx.Err()}
					return
				}
			}
			res.Results[i] = e.one(ctx, c, org, market, p, input, invoke)
			e.reportFailure(ctx, c, org, market, res.Results[i])
		}(i, p)
	}
	wg.Wait()
	return res, nil
}

func (e Executor) reportFailure(ctx context.Context, c Contract, org uuid.UUID, market string, r Result) {
	if e.OnFailure == nil || r.OK() {
		return
	}
	defer func() { _ = recover() }()
	e.OnFailure(ctx, Failure{Org: org, Market: market, Capability: c.Key, Provider: r.Provider,
		Code: r.ErrorCode(), Indeterminate: r.Indeterminate(), Duration: r.Duration, Err: r.Err, Reported: r.reported})
}

type invokeOutcome struct {
	out json.RawMessage
	err error
}

func (e Executor) one(parent context.Context, c Contract, org uuid.UUID, market string, p Provider, input map[string]any, invoke Invoker) Result {
	start := e.now()
	r := Result{Provider: p.Addon}
	payload, err := json.Marshal(Resolve(p.Input, nil, input))
	if err != nil {
		r.Err = fmt.Errorf("capability %s: provider %s: encode payload: %w", c.Key, p.Addon, err)
		return r
	}
	var idemKey string
	if c.Idempotent {
		idemKey, _ = input["idempotency_key"].(string)
	}
	timeout := e.timeoutFor(c, p)
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	done := make(chan invokeOutcome, 1) // buffered: a late invoker never blocks
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				done <- invokeOutcome{err: fmt.Errorf("%w: %s: %v", ErrProviderPanic, p.Addon, rec)}
			}
		}()
		out, err := invoke(ctx, Call{Org: org, Market: market, Capability: c.Key, Provider: p, IdempotencyKey: idemKey, Payload: payload})
		done <- invokeOutcome{out: out, err: err}
	}()

	var oc invokeOutcome
	select {
	case oc = <-done:
	case <-ctx.Done():
		oc.err = ctx.Err()
	}
	r.Duration = e.now().Sub(start)
	switch {
	case oc.err != nil && errors.Is(oc.err, context.DeadlineExceeded) && parent.Err() == nil:
		if c.Idempotent {
			r.Err = fmt.Errorf("%w: %w: %s after %s; the provider may still complete it, reconcile or retry with the same idempotency_key", ErrTimeout, ErrIndeterminate, p.Addon, timeout)
		} else {
			r.Err = fmt.Errorf("%w: %s after %s", ErrTimeout, p.Addon, timeout)
		}
		return r
	case oc.err != nil && c.Idempotent && (errors.Is(oc.err, context.DeadlineExceeded) || errors.Is(oc.err, context.Canceled)):
		// The caller gave up while the provider call was in flight.
		r.Err = fmt.Errorf("capability %s: provider %s: %w: %w", c.Key, p.Addon, ErrIndeterminate, oc.err)
		return r
	case oc.err != nil:
		var coded *CodedError
		if errors.As(oc.err, &coded) {
			r.reported = coded
		}
		if errors.As(oc.err, &coded) && !contains(c.Errors, coded.Code) {
			// Not part of the contract: keep the message, drop the code so
			// consumers only ever branch on declared codes.
			r.Err = fmt.Errorf("capability %s: provider %s: %s", c.Key, p.Addon, oc.err.Error())
			return r
		}
		r.Err = fmt.Errorf("capability %s: provider %s: %w", c.Key, p.Addon, oc.err)
		return r
	}
	if !e.SkipOutputValidation {
		if problems := c.ValidateOutput(oc.out); len(problems) > 0 {
			r.Err = &OutputError{Capability: c.Key, Provider: p.Addon, Problems: problems}
			return r
		}
	}
	r.Output = oc.out
	return r
}

// Querier is what a consumer (host service, storefront route, WASM host
// function) calls: it resolves the providers for (org, market, capability)
// and returns their outputs.
type Querier interface {
	Query(ctx context.Context, q Query) (QueryResult, error)
}

// Query is one capability query.
type Query struct {
	Org        uuid.UUID
	Market     Market
	Capability string
	Input      map[string]any
	// Provider pins the call to one provider (addon key). It must be an
	// installed, enabled provider of the org and capability that still serves
	// the market; otherwise ErrNoProvider. Contracts with PinRequired
	// (confirm, status, refund, cancel, track, commit, reverse) refuse to run
	// without it: the host stores the provider on the payment / shipment /
	// invoice record and webhook routes carry it in the path
	// (/webhooks/payments/{addon}). Empty on other contracts = normal
	// selection (market, default, only provider).
	Provider string
	// Caller is the addon key that issued the query, stamped by the host
	// (the wasm capability_query import sets it from the invocation, never
	// from the guest). Empty = the host itself. Provider selection ignores
	// it; host-implemented contracts (payments ledger) authorize on it.
	Caller string
}

// ProviderSource lists the providers of a capability installed and enabled
// in an org (host-implemented: manifests + installation settings; see
// SelectionFromSettings).
type ProviderSource interface {
	Providers(ctx context.Context, org uuid.UUID, capability string) ([]Provider, error)
}

// ProviderSourceFunc adapts a function to ProviderSource.
type ProviderSourceFunc func(ctx context.Context, org uuid.UUID, capability string) ([]Provider, error)

// Providers implements ProviderSource.
func (f ProviderSourceFunc) Providers(ctx context.Context, org uuid.UUID, capability string) ([]Provider, error) {
	return f(ctx, org, capability)
}

// Engine is the host-neutral Querier: Source + Select + Executor + Invoke.
type Engine struct {
	Source   ProviderSource
	Invoke   Invoker
	Executor Executor
	// Lookup resolves contracts; nil = the package registry (Lookup).
	Lookup func(key string) (Contract, bool)
}

// Query implements Querier. Only well-known query contracts are queryable:
// an unknown key has no output contract to validate against.
func (g *Engine) Query(ctx context.Context, q Query) (QueryResult, error) {
	lookup := g.Lookup
	if lookup == nil {
		lookup = Lookup
	}
	c, ok := lookup(q.Capability)
	if !ok {
		return QueryResult{Capability: q.Capability, Market: q.Market.Code}, fmt.Errorf("%w: unknown contract %q", ErrNotQuery, q.Capability)
	}
	if g.Source == nil {
		return QueryResult{Capability: c.Key, Market: q.Market.Code}, errors.New("capability: engine has no provider source")
	}
	if c.PinRequired && strings.TrimSpace(q.Provider) == "" {
		return QueryResult{Capability: c.Key, Market: q.Market.Code}, fmt.Errorf("%w: %s must name the provider that created the record", ErrProviderRequired, c.Key)
	}
	all, err := g.Source.Providers(ctx, q.Org, c.Key)
	if err != nil {
		return QueryResult{Capability: c.Key, Market: q.Market.Code}, fmt.Errorf("capability %s: list providers: %w", c.Key, err)
	}
	var chosen []Provider
	if q.Provider != "" {
		chosen, err = SelectPinned(c, all, q.Market, q.Provider)
	} else {
		chosen, err = Select(c, all, q.Market)
	}
	if err != nil {
		return QueryResult{Capability: c.Key, Market: q.Market.Code}, err
	}
	return g.Executor.Run(ctx, c, q.Org, q.Market.Code, chosen, q.Input, g.Invoke)
}

// Installation-settings keys hosts use to persist provider selection. The
// org-level default list already exists in hosts (ops); the market map is
// its per-market twin.
const (
	// DefaultsSetting is a []string of capability keys this addon is the
	// org-level default provider of.
	DefaultsSetting = "_capability_defaults"
	// MarketsSetting is a map capability key -> []market code this addon is
	// selected for.
	MarketsSetting = "_capability_markets"
)

// SelectionFromSettings reads an installation's settings (decoded JSON) and
// reports whether the addon is the org default of capability and for which
// markets it is selected — the Default/Markets of its Provider.
func SelectionFromSettings(settings map[string]any, capability string) (isDefault bool, markets []string) {
	if list, ok := settings[DefaultsSetting].([]any); ok {
		for _, v := range list {
			if s, _ := v.(string); s == capability {
				isDefault = true
			}
		}
	}
	if byCap, ok := settings[MarketsSetting].(map[string]any); ok {
		if list, ok := byCap[capability].([]any); ok {
			for _, v := range list {
				if s, _ := v.(string); s != "" {
					markets = append(markets, s)
				}
			}
		}
	}
	return isDefault, markets
}
