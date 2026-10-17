// Package capability defines provider-neutral capability contracts: a named
// operation (e.g. "messaging.whatsapp.send") with a fixed input shape that any
// addon may PROVIDE (manifest provides_capabilities[]) and any addon may
// REQUIRE from an action/tool/subscription (handler.type "capability").
//
// The consumer never names a connector or an addon. The host resolves, per
// org, which installed addon provides the capability and dispatches to that
// provider's own handler, translating the contract input into the provider's
// payload with the provider's input mapping. Swapping WhatsApp over Link for
// WhatsApp over a local Baileys sidecar is then an install decision, not a
// change to every addon that sends a message.
//
// A contract is either an ACTION (fire the provider, the consumer does not
// read a result — the original messaging.whatsapp.send model) or a QUERY: it
// declares an Output schema and the consumer gets the provider's normalized
// output back. A query's Cardinality is "one" (the provider selected for the
// org/market) or "all" (fan-out to every eligible provider in parallel, each
// under its own deadline, with partial results). See Executor, Select and
// docs/capabilities-commerce.md.
//
// Providers are resolved per (organization, market, capability): a provider
// explicitly selected for the market wins, then the org-level default, then
// the only eligible provider (see Select).
//
// Both sides map inputs with the same tiny expression language (see Resolve):
//
//	record.<column>   a column of the acted-on row
//	payload.<field>   a field of the payload (action form / event / contract input)
//	const:<literal>   a literal string
package capability

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Kind tells whether a contract returns a result to its consumer.
type Kind string

const (
	// KindAction runs the provider; the consumer reads no output ("" too).
	KindAction Kind = "action"
	// KindQuery runs the provider(s) and returns their outputs, validated
	// against Contract.Output.
	KindQuery Kind = "query"
)

// Cardinality is how many providers a query reaches.
type Cardinality string

const (
	// One reaches the provider selected for the org/market ("" too).
	One Cardinality = "one"
	// All fans out to every eligible provider in parallel and merges what
	// arrived in time (shipping rates, discounts).
	All Cardinality = "all"
)

// Contract is the shape of a well-known capability. Hosts and validators use
// it to reject mappings to fields the contract does not have; a capability key
// outside Contracts is still valid (open set) but is not field-checked.
//
// Required/Optional are the input field names (kept for the mapping
// validators); Input, when set, carries their types and is the source those
// lists are derived from. Output is mandatory for a KindQuery contract.
type Contract struct {
	Key      string
	Required []string
	Optional []string

	// Kind is KindAction ("") or KindQuery.
	Kind Kind
	// Cardinality of a query: One ("") or All.
	Cardinality Cardinality
	// Timeout is the per-provider deadline of a query; 0 = the Executor's.
	Timeout time.Duration
	// Idempotent marks a MUTATING query (money moves or an irreversible side
	// effect: charge, refund, label, fulfillment, invoice, tax ledger). The
	// Executor refuses it without a non-blank `idempotency_key` input, hands
	// the key to the provider (Call.IdempotencyKey), defaults the deadline to
	// MutatingTimeout and flags a timeout as indeterminate (the provider call
	// may still succeed: reconcile before retrying).
	Idempotent bool
	// PinRequired marks a follow-up query about something a specific provider
	// already created (confirm, status, refund, cancel, track, commit,
	// reverse): Query.Provider is mandatory, selection never guesses.
	PinRequired bool
	// Input types the input fields (nil = untyped, names only).
	Input *Schema
	// Output is the normalized result shape of a query.
	Output *Schema
	// Errors lists the error codes a provider may report for this contract
	// with NewCodedError (e.g. "invalid_input", "not_found"). A declared
	// code reaches the consumer as Result.ErrorCode(); an undeclared one is
	// reported as "provider_error". Nil = the generic codes only.
	Errors []string
	// Doc is a one-line description.
	Doc string
}

// IsQuery reports whether the contract returns outputs.
func (c Contract) IsQuery() bool { return c.Kind == KindQuery }

// Fanout reports whether a query reaches every eligible provider.
func (c Contract) Fanout() bool { return c.Cardinality == All }

// CheckInput validates a resolved contract input: required fields present
// (non-blank) and, when the contract types its input, every declared field of
// the right type. Undeclared fields pass through untouched (open input).
func (c Contract) CheckInput(input map[string]any) []string {
	var errs []string
	for _, f := range MissingRequired(c, input) {
		errs = append(errs, f+": required")
	}
	if c.Input != nil {
		for _, e := range c.Input.ValidateOpen(input) {
			if !strings.HasSuffix(e, ": required") {
				errs = append(errs, e)
			}
		}
	}
	return errs
}

// ValidateOutput checks one provider output against the contract's Output
// schema (nil Output accepts any JSON object).
func (c Contract) ValidateOutput(raw []byte) []string {
	if c.Output == nil {
		if _, err := decodeObject(raw); err != nil {
			return []string{err.Error()}
		}
		return nil
	}
	return c.Output.ValidateJSON(raw)
}

// Fields returns every field of the contract, required first.
func (c Contract) Fields() []string {
	return append(append([]string(nil), c.Required...), c.Optional...)
}

// Has reports whether field belongs to the contract.
func (c Contract) Has(field string) bool {
	for _, f := range c.Fields() {
		if f == field {
			return true
		}
	}
	return false
}

// MessagingWhatsAppSend sends one WhatsApp message. `to` is a phone number
// (digits, any formatting) or a full JID; `device_id` selects the sending
// device/session when the provider manages several (providers default it).
const MessagingWhatsAppSend = "messaging.whatsapp.send"

// Contracts is the registry of well-known capability contracts. Commerce and
// fiscal query contracts are registered from commerce.go.
var Contracts = map[string]Contract{
	MessagingWhatsAppSend: {
		Key:      MessagingWhatsAppSend,
		Kind:     KindAction,
		Required: []string{"to"},
		Optional: []string{"message", "media_url", "media_type", "device_id"},
	},
}

// Lookup returns the well-known contract for key.
func Lookup(key string) (Contract, bool) {
	c, ok := Contracts[key]
	return c, ok
}

// Keys lists the well-known contract keys, sorted.
func Keys() []string {
	out := make([]string, 0, len(Contracts))
	for k := range Contracts {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Register adds a well-known contract (used by this package's init and by
// hosts that ship their own vetted contracts). It derives Required/Optional
// from Input and rejects malformed keys, duplicates and queries without an
// Output schema.
func Register(c Contract) error {
	if !ValidKey(c.Key) {
		return fmt.Errorf("capability: invalid contract key %q", c.Key)
	}
	if _, dup := Contracts[c.Key]; dup {
		return fmt.Errorf("capability: contract %q already registered", c.Key)
	}
	switch c.Kind {
	case "", KindAction:
		if c.Output != nil || c.Cardinality == All {
			return fmt.Errorf("capability: action contract %q cannot declare output or cardinality all", c.Key)
		}
	case KindQuery:
		if c.Output == nil {
			return fmt.Errorf("capability: query contract %q needs an Output schema", c.Key)
		}
	default:
		return fmt.Errorf("capability: contract %q has unknown kind %q", c.Key, c.Kind)
	}
	switch c.Cardinality {
	case "", One, All:
	default:
		return fmt.Errorf("capability: contract %q has unknown cardinality %q", c.Key, c.Cardinality)
	}
	for _, code := range c.Errors {
		if !errorCodeRe.MatchString(code) {
			return fmt.Errorf("capability: contract %q declares malformed error code %q", c.Key, code)
		}
		if reservedErrorCodes[code] {
			return fmt.Errorf("capability: contract %q declares error code %q, reserved for the executor", c.Key, code)
		}
	}
	if err := c.Input.checkPatterns(); err != nil {
		return fmt.Errorf("capability: contract %q input: %w", c.Key, err)
	}
	if err := c.Output.checkPatterns(); err != nil {
		return fmt.Errorf("capability: contract %q output: %w", c.Key, err)
	}
	if c.Input != nil {
		c.Required, c.Optional = c.Input.Required(), c.Input.Optional()
	}
	Contracts[c.Key] = c
	return nil
}

func mustRegister(c Contract) {
	if err := Register(c); err != nil {
		panic(err)
	}
}

// reservedErrorCodes are the codes Result.ErrorCode derives itself; a
// contract cannot redeclare them.
var reservedErrorCodes = map[string]bool{
	"timeout": true, "invalid_output": true, "panic": true, "canceled": true, "provider_error": true,
}

var (
	errorCodeRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	keyRe       = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)
	fieldRe     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// ValidKey reports whether key is a well-formed dotted capability key.
func ValidKey(key string) bool { return keyRe.MatchString(key) }

// ValidateSource checks one mapping expression.
func ValidateSource(expr string) error {
	switch {
	case strings.HasPrefix(expr, "const:"):
		return nil
	case strings.HasPrefix(expr, "record."):
		if !fieldRe.MatchString(strings.TrimPrefix(expr, "record.")) {
			return fmt.Errorf("invalid record field in %q", expr)
		}
		return nil
	case strings.HasPrefix(expr, "payload."):
		if !fieldRe.MatchString(strings.TrimPrefix(expr, "payload.")) {
			return fmt.Errorf("invalid payload field in %q", expr)
		}
		return nil
	}
	return fmt.Errorf("source %q must be record.<column>, payload.<field> or const:<literal>", expr)
}

// ValidateInput checks a mapping (target field -> source expression). When
// contract is non-nil every target must be a contract field (consumer side).
func ValidateInput(input map[string]string, contract *Contract) []string {
	var errs []string
	keys := make([]string, 0, len(input))
	for k := range input {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, target := range keys {
		if !fieldRe.MatchString(target) {
			errs = append(errs, fmt.Sprintf("input key %q is not a valid field name", target))
			continue
		}
		if contract != nil && !contract.Has(target) {
			errs = append(errs, fmt.Sprintf("input key %q is not a field of capability %q (fields: %s)", target, contract.Key, strings.Join(contract.Fields(), ", ")))
		}
		if err := ValidateSource(input[target]); err != nil {
			errs = append(errs, fmt.Sprintf("input[%q]: %v", target, err))
		}
	}
	return errs
}

// Resolve builds a payload from mapping. Every payload field is carried over
// first (so unmapped fields pass through under their own name), then each
// mapped target is set from its source; a source that resolves to nothing
// leaves the target unset.
func Resolve(mapping map[string]string, record, payload map[string]any) map[string]any {
	out := make(map[string]any, len(payload)+len(mapping))
	for k, v := range payload {
		out[k] = v
	}
	for target, expr := range mapping {
		v, ok := source(expr, record, payload)
		if !ok {
			continue
		}
		out[target] = v
	}
	return out
}

func source(expr string, record, payload map[string]any) (any, bool) {
	switch {
	case strings.HasPrefix(expr, "const:"):
		return strings.TrimPrefix(expr, "const:"), true
	case strings.HasPrefix(expr, "record."):
		v, ok := record[strings.TrimPrefix(expr, "record.")]
		return v, ok && v != nil
	case strings.HasPrefix(expr, "payload."):
		v, ok := payload[strings.TrimPrefix(expr, "payload.")]
		return v, ok && v != nil
	}
	return nil, false
}

// MissingRequired lists the contract's required fields absent (or empty
// strings) in input — the host's pre-dispatch check.
func MissingRequired(c Contract, input map[string]any) []string {
	var out []string
	for _, f := range c.Required {
		v, ok := input[f]
		if !ok || v == nil {
			out = append(out, f)
			continue
		}
		if s, isStr := v.(string); isStr && strings.TrimSpace(s) == "" {
			out = append(out, f)
		}
	}
	return out
}
