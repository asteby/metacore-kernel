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

	// RuleAuditColumnType: a model that declares one of the kernel's standard
	// audit columns (created_at, updated_at, deleted_at, created_by_id,
	// updated_by_id, deleted_by_id) must declare it with the standard type
	// (timestamp/timestamptz for *_at, uuid for *_by_id). Introduced with the
	// audit-column standard (docs/audit-columns.md). Promote to a hard error
	// everywhere one minor release after it ships.
	RuleAuditColumnType = "audit_column_type"

	// RuleFinalStageOutgoing: a transition leaving an is_final stage must
	// carry `reopen: true` or an approval block (the explicit exit). Introduced with the
	// strict stage machine (P3). Promote to a hard error everywhere one minor
	// release after it ships.
	// Warning-only in BOTH modes (warnOnlyWindow): the hub reads bundles with
	// Strict, and reopen/archive exits are legitimate in real addons.
	RuleFinalStageOutgoing = "final_stage_outgoing"
)

// RuleWebhookActionSigning and RuleRBACPermissionResolves are declared in
// hub_primitives_validate.go next to the checks that emit them. Unlike the two
// rules above they are WARNING-ONLY IN BOTH MODES for their first minor
// (warnOnlyWindow): placeholder `handler: {type: "webhook"}` fixtures are
// everywhere (host and addon tests), so turning them into publish errors on the
// same release that introduces the rule would break every consumer's bump. The
// warnings come back from ValidateWithOptions / ParseWithOptions in either
// mode; promote them to `tolerate` (and then to plain errors) in the next minor.

// Slot / private-addon rules (C1, docs/spec/v3/slots.md), emitted by
// validateSlotsAndPrivate (validate.go). Warning-only in BOTH modes for their
// first minor (warnOnlyWindow): a published addon that contributes to another
// addon's slot kind, or a private addon without integrity/capabilities, must
// not be rejected at publish by the release that introduces the rule.
const (
	// RuleSlotKindUnknown: contributions.slots[].slot_kind is neither in the
	// ops.host/v1 catalog nor published by this manifest's own
	// extension_points.slot_kinds.
	RuleSlotKindUnknown = "slot_kind_unknown"

	// RulePrivateFrontendIntegrity: metadata.visibility=private and a frontend
	// block require frontend.integrity.
	RulePrivateFrontendIntegrity = "private_frontend_integrity"

	// RulePrivateExplicitCapabilities: metadata.visibility=private and any wasm
	// handler require a non-empty capabilities[].
	RulePrivateExplicitCapabilities = "private_explicit_capabilities"
)

// legacyTolerated lists the rules currently inside their compatibility window.
var legacyTolerated = []string{
	RuleRequiresStateInStages,
	RuleAuditColumnType,
	RuleFinalStageOutgoing,
	RuleWebhookActionSigning,
	RuleRBACPermissionResolves,
	RuleSlotKindUnknown,
	RulePrivateFrontendIntegrity,
	RulePrivateExplicitCapabilities,
	RuleEventEmitCapability,
	RuleRoleMissing,
}

// warnOnlyWindow marks the legacyTolerated rules that are reported as warnings
// even when Options.Strict is set (see the note above).
var warnOnlyWindow = map[string]struct{}{
	RuleFinalStageOutgoing:          {},
	RuleWebhookActionSigning:        {},
	RuleRBACPermissionResolves:      {},
	RuleSlotKindUnknown:             {},
	RulePrivateFrontendIntegrity:    {},
	RulePrivateExplicitCapabilities: {},
	RuleEventEmitCapability:         {},
	RuleRoleMissing:                 {},
}

// LegacyTolerated returns the rule ids currently inside their compatibility
// window.
func LegacyTolerated() []string { return append([]string(nil), legacyTolerated...) }

// RuleEventEmitCapability: events.emits[].name is not covered by any of the
// addon's own `event:emit` capabilities (exact name, "prefix.*" or "*"), so a
// guest of this addon could not publish it. Warning-only in both modes for
// its first minor. Canonical CRUD names (addon.Model.action) are published by
// the host on the owner's behalf and are exempt.
const RuleEventEmitCapability = "event_emit_capability"
