package v3

// Compatibility window for validation rules.
//
// Tightening the validator must never invalidate addons that were already
// published (kernel #453 made `requires_state` outside the stage machine a
// hard error and broke Upgrade for workshop / customers in production the
// next day). The policy: a NEW rule that rejects manifests which used to pass
// is registered here as legacy-tolerated and ships as a WARNING for at least
// one minor release; only manifests being published (Options.Strict) see it as
// an error. After the window the rule is removed from this registry and
// becomes a plain error everywhere. See docs/validation-compat-window.md.

// Options selects how ValidateWithOptions treats legacy-tolerated rules.
type Options struct {
	// Strict makes every rule a hard error. Use it when a manifest is being
	// PUBLISHED (hub) or authored (CI). Leave it false when INSTALLING or
	// UPGRADING an already-published bundle (ops, installer): tolerated rules
	// then come back as warnings.
	Strict bool
}

// Rule ids of the legacy-tolerated rules. Warnings are prefixed "[id] ".
const (
	// RuleRequiresStateInStages: contributions.actions[].requires_state values
	// must be stages of the target model's stage machine (kernel #453).
	// Promote to a hard error everywhere one minor release after it ships.
	RuleRequiresStateInStages = "requires_state_in_stages"
)

// legacyTolerated lists the rules currently inside their compatibility window.
var legacyTolerated = []string{
	RuleRequiresStateInStages,
}

// LegacyTolerated returns the rule ids currently inside their compatibility
// window.
func LegacyTolerated() []string { return append([]string(nil), legacyTolerated...) }
