package capability

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// EventContract is a well-known event that the providers of a capability
// domain publish, e.g. commerce.payments.confirmed, which the payment
// providers emit from their own webhooks. The consumer subscribes to the
// event name and never to a provider ("stripe.payment_updated"), exactly as it
// queries a capability without naming who answers it.
//
// The contract also reserves the namespace: every event under EmittedBy
// (commerce.payments.*) may only be emitted by an addon that PROVIDES at least
// one capability under that domain (provides_capabilities[]). The host enforces
// it at the guest boundary (security.EmitPolicy, rule "contract_namespace"), so
// a non-provider cannot forge a payment confirmation.
type EventContract struct {
	// Name is the event name ("commerce.payments.confirmed").
	Name string
	// EmittedBy is the capability domain whose providers may emit the event
	// ("commerce.payments"). Name must lie under it.
	EmittedBy string
	// Payload is the shape of the event payload. The keys the host stamps on
	// delivery (HostStampedEventFields) are not part of it.
	Payload *Schema
	// Doc is a one-line description.
	Doc string
}

// HostStampedEventFields are the top-level keys the dispatcher writes into a
// delivered payload (event name, publisher, actor, branch). They are ignored
// by EventContract.ValidatePayload.
var HostStampedEventFields = []string{"event", "emitter", "actor_id", "branch_id"}

// ValidatePayload checks an event payload (a JSON object) against the
// contract, ignoring HostStampedEventFields. Emitters can call it before
// publishing and subscribers after receiving.
func (e EventContract) ValidatePayload(raw []byte) []string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v map[string]any
	if err := dec.Decode(&v); err != nil || v == nil {
		return []string{"payload is not a JSON object"}
	}
	for _, k := range HostStampedEventFields {
		if _, declared := e.Payload.Field(k); !declared {
			delete(v, k)
		}
	}
	return e.Payload.Validate(v)
}

// EventContracts is the registry of well-known event contracts.
var EventContracts = map[string]EventContract{}

// LookupEvent returns the well-known event contract named name.
func LookupEvent(name string) (EventContract, bool) {
	e, ok := EventContracts[name]
	return e, ok
}

// EventNames lists the registered event contract names, sorted.
func EventNames() []string {
	out := make([]string, 0, len(EventContracts))
	for k := range EventContracts {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// RegisterEvent adds a well-known event contract. It rejects malformed names
// and domains, a name outside its domain, duplicates and a missing payload.
func RegisterEvent(e EventContract) error {
	if !ValidKey(e.Name) {
		return fmt.Errorf("capability: invalid event name %q", e.Name)
	}
	if !ValidKey(e.EmittedBy) {
		return fmt.Errorf("capability: event %q: invalid EmittedBy domain %q", e.Name, e.EmittedBy)
	}
	if !strings.HasPrefix(e.Name, e.EmittedBy+".") {
		return fmt.Errorf("capability: event %q must be named under its domain %q", e.Name, e.EmittedBy)
	}
	if e.Payload == nil {
		return fmt.Errorf("capability: event %q needs a Payload schema", e.Name)
	}
	if err := e.Payload.checkPatterns(); err != nil {
		return fmt.Errorf("capability: event %q payload: %w", e.Name, err)
	}
	if _, dup := EventContracts[e.Name]; dup {
		return fmt.Errorf("capability: event %q already registered", e.Name)
	}
	EventContracts[e.Name] = e
	return nil
}

func mustRegisterEvent(e EventContract) {
	if err := RegisterEvent(e); err != nil {
		panic(err)
	}
}

// ReservedEventDomain reports whether event lies in a namespace reserved by a
// registered event contract and returns that capability domain
// ("commerce.payments" for "commerce.payments.anything"). Only providers of a
// capability under the domain may emit such an event (see ProvidesDomain).
// When several domains match, the longest wins.
func ReservedEventDomain(event string) (string, bool) {
	best := ""
	for _, e := range EventContracts {
		d := e.EmittedBy
		if (event == d || strings.HasPrefix(event, d+".")) && len(d) > len(best) {
			best = d
		}
	}
	return best, best != ""
}

// ProvidesDomain reports whether any of the capability keys an addon provides
// lies under domain ("commerce.payments.confirm" provides
// "commerce.payments").
func ProvidesDomain(provides []string, domain string) bool {
	for _, k := range provides {
		if k == domain || strings.HasPrefix(k, domain+".") {
			return true
		}
	}
	return false
}
