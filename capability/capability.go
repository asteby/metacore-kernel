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
)

// Contract is the input shape of a well-known capability. Hosts and validators
// use it to reject mappings to fields the contract does not have; a capability
// key outside Contracts is still valid (open set) but is not field-checked.
type Contract struct {
	Key      string
	Required []string
	Optional []string
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

// Contracts is the registry of well-known capability contracts.
var Contracts = map[string]Contract{
	MessagingWhatsAppSend: {
		Key:      MessagingWhatsAppSend,
		Required: []string{"to"},
		Optional: []string{"message", "media_url", "media_type", "device_id"},
	},
}

// Lookup returns the well-known contract for key.
func Lookup(key string) (Contract, bool) {
	c, ok := Contracts[key]
	return c, ok
}

var (
	keyRe   = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)
	fieldRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
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
