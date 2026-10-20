package security

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync/atomic"

	"github.com/asteby/metacore-kernel/capability"
	"github.com/google/uuid"
)

// Event-emit policy: who may publish which event names from the GUEST
// boundary (the wasm `event_emit` import, a native sidecar's event channel).
// It is deliberately NOT wired into events.Bus: the host publishes canonical
// CRUD events under the owning addon's key and its own events as "kernel",
// and none of those are guest emissions.
//
// Rules, evaluated in order (EmitPolicy.Check):
//
//  1. kernel_namespace  — "kernel" and "kernel.*" are host-only.
//  2. contract_namespace — an event under a domain reserved by a capability
//     event contract (commerce.payments.*) needs the emitter to PROVIDE a
//     capability of that domain. Such an event is not an addon's namespace,
//     so rule 3 does not apply to it.
//  3. foreign_namespace — an event whose prefix is another installed addon's
//     key (storefront_cart.checkout_paid emitted by "evil") is denied unless
//     that owner lists the emitter in extension_points.events[].emitters.
//  4. undeclared        — the event must match the emitter's own
//     `event:emit` declarations (Capabilities.CanEmit).
//
// Rules 1-3 form the "reserved" group and rule 4 the "declared" group; each
// group has its own mode (off | warn | enforce), independent of
// METACORE_ENFORCE, read from METACORE_EVENT_EMIT_RESERVED and
// METACORE_EVENT_EMIT_DECLARED. Both default to warn: a violation is logged,
// reported (OnViolation: metric, kernel.event_emit.violation) and the publish
// proceeds.
//
// Exception: money domains (EnforcedDomains, default commerce.payments) are
// always enforced by rules 2 and facts_unavailable while the reserved group
// is not off: a non-provider can never publish a payment confirmation, not
// even during the warn rollout, and an unverifiable emitter fails closed.

// EmitRule names the rule a violation broke.
type EmitRule string

const (
	// EmitRuleKernel: a guest emitted "kernel" / "kernel.*".
	EmitRuleKernel EmitRule = "kernel_namespace"
	// EmitRuleContract: a non-provider emitted in a capability contract's
	// reserved domain.
	EmitRuleContract EmitRule = "contract_namespace"
	// EmitRuleForeign: an addon emitted in another installed addon's
	// namespace without being listed in that owner's emitters.
	EmitRuleForeign EmitRule = "foreign_namespace"
	// EmitRuleUnverified: the facts needed by rules 2-3 could not be loaded
	// and the event is outside the emitter's own namespace (reserved group).
	EmitRuleUnverified EmitRule = "facts_unavailable"
	// EmitRuleDeclared: the event is not covered by the emitter's
	// `event:emit` declarations.
	EmitRuleDeclared EmitRule = "undeclared"
)

// EmitGroup is the mode group of a rule.
type EmitGroup string

const (
	// EmitGroupReserved covers rules 1-3 (and facts_unavailable).
	EmitGroupReserved EmitGroup = "reserved"
	// EmitGroupDeclared covers rule 4.
	EmitGroupDeclared EmitGroup = "declared"
)

// Group returns the mode group the rule belongs to.
func (r EmitRule) Group() EmitGroup {
	if r == EmitRuleDeclared {
		return EmitGroupDeclared
	}
	return EmitGroupReserved
}

// EmitMode is how a rule group reacts to a violation.
type EmitMode int32

const (
	// EmitModeWarn logs and reports the violation; the publish proceeds.
	// The default for both groups.
	EmitModeWarn EmitMode = iota
	// EmitModeEnforce logs, reports and denies the publish.
	EmitModeEnforce
	// EmitModeOff skips the group entirely (emergency switch).
	EmitModeOff
)

// String returns the mode name used in logs and env vars.
func (m EmitMode) String() string {
	switch m {
	case EmitModeEnforce:
		return "enforce"
	case EmitModeOff:
		return "off"
	default:
		return "warn"
	}
}

// Env vars selecting the mode of each group. Independent of METACORE_ENFORCE.
const (
	EnvEmitReserved = "METACORE_EVENT_EMIT_RESERVED"
	EnvEmitDeclared = "METACORE_EVENT_EMIT_DECLARED"
)

// ParseEmitMode parses "warn" | "enforce" | "off" (case-insensitive; also
// "shadow"/"log" for warn, "deny" for enforce, "disabled" for off). ok is
// false for anything else, including the empty string.
func ParseEmitMode(s string) (EmitMode, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "warn", "shadow", "log":
		return EmitModeWarn, true
	case "enforce", "deny":
		return EmitModeEnforce, true
	case "off", "disabled":
		return EmitModeOff, true
	}
	return EmitModeWarn, false
}

// EmitModeFromEnv reads env var name. Unset or empty = warn; an unknown value
// also falls back to warn (logged): the policy is rolled out observe-first,
// so a typo must not start denying production emissions.
func EmitModeFromEnv(name string) EmitMode {
	raw := os.Getenv(name)
	if strings.TrimSpace(raw) == "" {
		return EmitModeWarn
	}
	m, ok := ParseEmitMode(raw)
	if !ok {
		log.Printf("metacore.event_emit.config unknown %s=%q, using warn", name, raw)
	}
	return m
}

// EmitterGrant is one owner declaration extending its namespace to other
// emitters: manifest extension_points.events[] {name, emitters}.
type EmitterGrant struct {
	// Owner is the addon whose namespace the event lives in.
	Owner string
	// Event is the event name or a trailing ".*" glob.
	Event string
	// Emitters are the addon keys allowed to emit it.
	Emitters []string
}

// EmitFacts is what the host knows about an org and an emitter, loaded per
// check by an EmitFactsSource. Zero values mean "unknown" where noted.
type EmitFacts struct {
	// Installed are the addon keys installed in the org (rule 3 resolves the
	// owner of an event among them). A host may pass every known key.
	Installed []string
	// Provides are the capability keys the EMITTER provides
	// (provides_capabilities[].key) in the org (rule 2).
	Provides []string
	// Grants are the owners' emitters declarations (rule 3 exception).
	Grants []EmitterGrant
	// Caps is the emitter's compiled manifest policy (rule 4, CanEmit). nil =
	// unknown: rule 4 is not evaluated.
	Caps *Capabilities
}

// EmitFactsSource loads EmitFacts for (org, emitter).
type EmitFactsSource interface {
	EmitFacts(ctx context.Context, org uuid.UUID, emitter string) (EmitFacts, error)
}

// EmitFactsFunc adapts a function to EmitFactsSource.
type EmitFactsFunc func(ctx context.Context, org uuid.UUID, emitter string) (EmitFacts, error)

// EmitFacts implements EmitFactsSource.
func (f EmitFactsFunc) EmitFacts(ctx context.Context, org uuid.UUID, emitter string) (EmitFacts, error) {
	return f(ctx, org, emitter)
}

// EmitViolation is one broken rule.
type EmitViolation struct {
	Org    uuid.UUID `json:"org_id"`
	Addon  string    `json:"addon"`
	Event  string    `json:"event"`
	Rule   EmitRule  `json:"rule"`
	Group  EmitGroup `json:"group"`
	Mode   string    `json:"mode"`
	Denied bool      `json:"denied"`
	// Owner is the namespace owner (rule 3) or the reserved capability
	// domain (rule 2).
	Owner  string `json:"owner,omitempty"`
	Reason string `json:"reason"`
}

// EmitViolationEvent is the host event a wasm host publishes (as "kernel")
// for every violation, in either mode, so an operator page (OPS integration
// health) can list them. Payload: EmitViolation.
const EmitViolationEvent = "kernel.event_emit.violation"

// ErrEmitDenied is wrapped by every enforce-mode denial.
var ErrEmitDenied = errors.New("event emit denied")

// EmitDeniedError is the error Check returns when an enforced rule denies.
type EmitDeniedError struct{ Violation EmitViolation }

func (e *EmitDeniedError) Error() string {
	return fmt.Sprintf("addon %q may not emit %q (%s): %s", e.Violation.Addon, e.Violation.Event, e.Violation.Rule, e.Violation.Reason)
}

// Unwrap makes errors.Is(err, ErrEmitDenied) hold.
func (e *EmitDeniedError) Unwrap() error { return ErrEmitDenied }

// EmitPolicy evaluates the event-emit rules. Safe for concurrent use; modes
// can be switched at runtime with SetMode.
type EmitPolicy struct {
	// Source loads per-org facts. nil = only rule 1 is evaluated.
	Source EmitFactsSource
	// ReservedDomain resolves the reserved capability domain of an event;
	// nil = capability.ReservedEventDomain (the registered event contracts).
	ReservedDomain func(event string) (string, bool)
	// Logger receives one line per violation; nil = log.Default().
	Logger *log.Logger
	// OnViolation, when set, is called for every violation in any mode
	// (metrics, alerting). Called synchronously; keep it cheap.
	OnViolation func(ctx context.Context, v EmitViolation)
	// EnforcedDomains are reserved capability domains whose rule-2 (and
	// facts_unavailable) violations are denied in warn mode too. nil =
	// DefaultEnforcedDomains; an empty non-nil slice disables the exception.
	EnforcedDomains []string

	reserved atomic.Int32
	declared atomic.Int32
}

// DefaultEnforcedDomains: payment confirmations move money.
var DefaultEnforcedDomains = []string{"commerce.payments"}

func (p *EmitPolicy) enforcedDomain(domain string) bool {
	domains := p.EnforcedDomains
	if domains == nil {
		domains = DefaultEnforcedDomains
	}
	for _, d := range domains {
		if d == domain {
			return true
		}
	}
	return false
}

// NewEmitPolicy builds a policy with the group modes read from
// METACORE_EVENT_EMIT_RESERVED / METACORE_EVENT_EMIT_DECLARED (default warn).
func NewEmitPolicy(src EmitFactsSource) *EmitPolicy {
	p := &EmitPolicy{Source: src}
	p.SetMode(EmitGroupReserved, EmitModeFromEnv(EnvEmitReserved))
	p.SetMode(EmitGroupDeclared, EmitModeFromEnv(EnvEmitDeclared))
	return p
}

// SetMode switches a group's mode atomically.
func (p *EmitPolicy) SetMode(g EmitGroup, m EmitMode) {
	if g == EmitGroupDeclared {
		p.declared.Store(int32(m))
		return
	}
	p.reserved.Store(int32(m))
}

// Mode returns a group's current mode.
func (p *EmitPolicy) Mode(g EmitGroup) EmitMode {
	if g == EmitGroupDeclared {
		return EmitMode(p.declared.Load())
	}
	return EmitMode(p.reserved.Load())
}

// Check evaluates the rules for a guest emission of event by emitter in org.
// It returns every violation found (warned and denied alike, already logged
// and passed to OnViolation) and, when an enforced rule was broken, an
// *EmitDeniedError for the first one. Host publications ("kernel", canonical
// CRUD events) must not be routed through Check.
func (p *EmitPolicy) Check(ctx context.Context, org uuid.UUID, emitter, event string) ([]EmitViolation, error) {
	if p == nil {
		return nil, nil
	}
	reservedMode, declaredMode := p.Mode(EmitGroupReserved), p.Mode(EmitGroupDeclared)
	if reservedMode == EmitModeOff && declaredMode == EmitModeOff {
		return nil, nil
	}

	var found []EmitViolation
	add := func(rule EmitRule, owner, reason string) {
		found = append(found, EmitViolation{Org: org, Addon: emitter, Event: event, Rule: rule, Owner: owner, Reason: reason})
	}

	// Rule 1 needs no facts.
	kernelSpace := event == "kernel" || strings.HasPrefix(event, "kernel.")
	if kernelSpace {
		add(EmitRuleKernel, "kernel", "the kernel namespace is reserved to the host")
	}

	var facts EmitFacts
	var factsErr error
	if p.Source != nil {
		facts, factsErr = p.Source.EmitFacts(ctx, org, emitter)
	} else {
		factsErr = errors.New("no facts source configured")
	}

	if !kernelSpace {
		domainOf := p.ReservedDomain
		if domainOf == nil {
			domainOf = capability.ReservedEventDomain
		}
		ownSpace := strings.HasPrefix(event, emitter+".")
		if domain, reserved := domainOf(event); reserved {
			// Rule 2. A contract domain belongs to no addon: rule 3 is moot.
			switch {
			case factsErr != nil:
				add(EmitRuleUnverified, domain, "cannot verify the emitter provides "+domain+".*: "+factsErr.Error())
			case !capability.ProvidesDomain(facts.Provides, domain):
				add(EmitRuleContract, domain, "only providers of a "+domain+".* capability may emit in that namespace")
			}
		} else if factsErr != nil {
			if !ownSpace {
				add(EmitRuleUnverified, "", "cannot resolve the namespace owner: "+factsErr.Error())
			}
		} else if owner := namespaceOwner(event, facts.Installed); owner != "" && owner != emitter {
			// Rule 3.
			if !granted(facts.Grants, owner, event, emitter) {
				add(EmitRuleForeign, owner, "namespace of addon "+owner+", which does not list this emitter in extension_points.events[].emitters")
			}
		}
	}

	// Rule 4.
	if factsErr == nil && facts.Caps != nil {
		if err := facts.Caps.CanEmit(event); err != nil {
			add(EmitRuleDeclared, "", err.Error())
		}
	}

	var denied error
	out := found[:0]
	for _, v := range found {
		mode := reservedMode
		if v.Rule.Group() == EmitGroupDeclared {
			mode = declaredMode
		}
		if mode == EmitModeOff {
			continue
		}
		if (v.Rule == EmitRuleContract || v.Rule == EmitRuleUnverified) && p.enforcedDomain(v.Owner) {
			mode = EmitModeEnforce
		}
		v.Group = v.Rule.Group()
		v.Mode = mode.String()
		v.Denied = mode == EmitModeEnforce
		p.report(ctx, v)
		if v.Denied && denied == nil {
			denied = &EmitDeniedError{Violation: v}
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, denied
}

func (p *EmitPolicy) report(ctx context.Context, v EmitViolation) {
	logger := p.Logger
	if logger == nil {
		logger = log.Default()
	}
	action := "warned"
	if v.Denied {
		action = "denied"
	}
	logger.Printf("metacore.event_emit.violation mode=%s action=%s org=%s addon=%s event=%s rule=%s owner=%s reason=%q",
		v.Mode, action, v.Org, v.Addon, v.Event, v.Rule, v.Owner, v.Reason)
	if p.OnViolation != nil {
		p.OnViolation(ctx, v)
	}
}

// namespaceOwner returns the installed addon whose key is the longest dotted
// prefix of event ("" when none). Addon keys carry no dots today, so this is
// the first segment; the longest-prefix rule keeps it correct if they ever do.
func namespaceOwner(event string, installed []string) string {
	best := ""
	for _, k := range installed {
		if k == "" {
			continue
		}
		if strings.HasPrefix(event, k+".") && len(k) > len(best) {
			best = k
		}
	}
	return best
}

// granted reports whether owner lists emitter for event. Grants declared by
// anyone but the namespace owner are ignored.
func granted(grants []EmitterGrant, owner, event, emitter string) bool {
	for _, g := range grants {
		if g.Owner != owner || !matchAny([]string{g.Event}, event) {
			continue
		}
		for _, e := range g.Emitters {
			if e == emitter {
				return true
			}
		}
	}
	return false
}
