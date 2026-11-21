package v3

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/Masterminds/semver/v3"
)

// UIBlock is the Loom block (RFC 0005, docs/rfc/0005-contribuciones-de-ui.md): the
// components this addon exposes (Parts), the extension points it publishes
// for others (Seams) and its grafts (Part -> Seam). Optional and additive: a
// manifest without `ui` is served exactly as before, and the legacy
// contributions.slots[] keeps working (the resolver projects it as grafts).
type UIBlock struct {
	Exposes       []UIPart  `json:"exposes,omitempty"`
	Seams         []UISeam  `json:"seams,omitempty"`
	Contributions []UIGraft `json:"contributions,omitempty"`
}

// UIPart is a component the addon exposes with a contract. It does not know
// where it is mounted.
type UIPart struct {
	// ID is qualified: "<addon_key>.<name>".
	ID      string `json:"id"`
	Version string `json:"version,omitempty"`
	// Expose is the federated remote's expose ("./parts/csf-import").
	Expose      string   `json:"expose"`
	PropsSchema string   `json:"props_schema,omitempty"`
	Needs       *UINeeds `json:"needs,omitempty"`
	Description string   `json:"description,omitempty"`
}

// UINeeds is what a Part needs to run.
type UINeeds struct {
	// Network: "none" (default), "org" (only this host's API) or "hosts".
	Network string `json:"network,omitempty"`
	// Hosts is the allowlist when Network is "hosts"; they go to the addon CSP.
	Hosts   []string `json:"hosts,omitempty"`
	Offline bool     `json:"offline,omitempty"`
}

// UISeam is a typed extension point this addon owns (generalizes
// extension_points.slot_kinds[]).
type UISeam struct {
	// ID is qualified: "<addon_key>.<name>" (e.g. "pos.cart.summary").
	ID      string `json:"id"`
	Version string `json:"version"`
	// Lenses maps each lens the seam offers to its data class.
	Lenses map[string]string `json:"lenses,omitempty"`
	// Writable lists the fields a graft may propose changes to (empty = none).
	Writable []string `json:"writable,omitempty"`
	// Intents the host handles (e.g. "pos.checkout.block").
	Intents []string `json:"intents,omitempty"`
	// Cardinality: "many" (default) or "one".
	Cardinality string        `json:"cardinality,omitempty"`
	Deprecated  *UIDeprecated `json:"deprecated,omitempty"`
	Description string        `json:"description,omitempty"`
}

// UIDeprecated marks a seam major as on its way out.
type UIDeprecated struct {
	Since   string `json:"since"`
	Removal string `json:"removal,omitempty"`
	Message string `json:"message,omitempty"`
}

// UIGraft mounts a Part on a Seam.
type UIGraft struct {
	// ID is qualified: "<addon_key>.<name>".
	ID string `json:"id"`
	// Part is the qualified id of an exposed Part (this addon's or another's).
	Part string `json:"part"`
	// Seam is a seam id: SDK/host catalog or published by an addon.
	Seam string `json:"seam"`
	// Requires is a semver range over the seam contract ("^1").
	Requires string `json:"requires,omitempty"`
	// Kind: view (default) | action | transform | source.
	Kind string `json:"kind,omitempty"`
	// Mode: contribute (default) | wrap | replace. replace is preset-only.
	Mode  string  `json:"mode,omitempty"`
	Order int     `json:"order,omitempty"`
	When  *UIWhen `json:"when,omitempty"`
	// Reads are the lenses the graft receives; nothing else reaches it.
	Reads []string `json:"reads,omitempty"`
	// Writes are the fields it may propose ("prefix.*" allowed).
	Writes []string `json:"writes,omitempty"`
	// Emits are the seam intents it may request.
	Emits []string `json:"emits,omitempty"`
	// Network / Hosts / Offline: same contract as UINeeds.
	Network string         `json:"network,omitempty"`
	Hosts   []string       `json:"hosts,omitempty"`
	Offline bool           `json:"offline,omitempty"`
	Props   map[string]any `json:"props,omitempty"`
}

// UIWhen gates a graft. Every present clause must hold. The server evaluates
// it (the source of truth); the client re-evaluates only the live parts
// (model, mode) for UX and Explain.
type UIWhen struct {
	Model         []string       `json:"model,omitempty"`
	Mode          []string       `json:"mode,omitempty"`
	Permission    []string       `json:"permission,omitempty"`
	AnyPermission []string       `json:"any_permission,omitempty"`
	Flag          string         `json:"flag,omitempty"`
	Setting       *UISettingWhen `json:"setting,omitempty"`
	Condition     *Condition     `json:"condition,omitempty"`
}

// UISettingWhen holds when the effective setting is truthy, or equal to
// Equals when it is present.
type UISettingWhen struct {
	Key    string `json:"key"`
	Equals any    `json:"equals,omitempty"`
}

// UIDataClasses are the lens data classes (redaction is driven by them).
var UIDataClasses = []string{"public", "internal", "money", "pii", "pii.fiscal", "secret"}

// UIManagedColumns can never appear in a graft's writes: they are the
// kernel's, not the form's.
var UIManagedColumns = []string{"id", "organization_id", "created_at", "updated_at", "deleted_at", "created_by", "updated_by"}

// SDKSeamCatalogV1 are the seams the SDK's dynamic components publish
// (record dialog today; table/form/dashboard follow). Hosts add
// HostSlotCatalogOpsV1.
var SDKSeamCatalogV1 = []string{
	"record.dialog.header",
	"record.form.section",
	"field.widget",
}

var uiQualifiedID = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z0-9][a-z0-9_-]*)+$`)

// validateLoom returns hard errors for the Loom block. Unknown seams are a
// tolerated warning (the seam may be published by another addon).
func validateLoom(m *Manifest, warn func(rule, msg string)) []string {
	if m.UI == nil {
		return nil
	}
	var errs []string
	key := m.Metadata.Key
	own := func(where, id string) {
		if !uiQualifiedID.MatchString(id) {
			errs = append(errs, fmt.Sprintf("%s %q must be a qualified id <addon_key>.<name>", where, id))
			return
		}
		if key != "" && !strings.HasPrefix(id, key+".") {
			errs = append(errs, fmt.Sprintf("%s %q must start with this addon's key %q", where, id, key+"."))
		}
	}
	parts := map[string]struct{}{}
	for i, p := range m.UI.Exposes {
		where := fmt.Sprintf("ui.exposes[%d].id", i)
		own(where, p.ID)
		if _, dup := parts[p.ID]; dup {
			errs = append(errs, fmt.Sprintf("%s %q is duplicated", where, p.ID))
		}
		parts[p.ID] = struct{}{}
		if p.Version != "" {
			if _, err := semver.NewVersion(p.Version); err != nil {
				errs = append(errs, fmt.Sprintf("ui.exposes[%d].version %q must be a semver version", i, p.Version))
			}
		}
		errs = append(errs, uiNetworkErrs(fmt.Sprintf("ui.exposes[%d].needs", i), p.Needs)...)
	}
	seams := map[string]struct{}{}
	for i, s := range m.UI.Seams {
		where := fmt.Sprintf("ui.seams[%d]", i)
		own(where+".id", s.ID)
		if _, dup := seams[s.ID]; dup {
			errs = append(errs, fmt.Sprintf("%s.id %q is duplicated", where, s.ID))
		}
		seams[s.ID] = struct{}{}
		if _, err := semver.NewVersion(s.Version); err != nil {
			errs = append(errs, fmt.Sprintf("%s.version %q must be a semver version", where, s.Version))
		}
		for _, w := range s.Writable {
			if isManagedColumn(w) {
				errs = append(errs, fmt.Sprintf("%s.writable lists managed column %q", where, w))
			}
		}
	}
	known := map[string]struct{}{}
	for _, k := range HostSlotCatalogOpsV1 {
		known[k] = struct{}{}
	}
	for _, k := range SDKSeamCatalogV1 {
		known[k] = struct{}{}
	}
	for k := range seams {
		known[k] = struct{}{}
	}
	grafts := map[string]struct{}{}
	for i, g := range m.UI.Contributions {
		where := fmt.Sprintf("ui.contributions[%d]", i)
		own(where+".id", g.ID)
		if _, dup := grafts[g.ID]; dup {
			errs = append(errs, fmt.Sprintf("%s.id %q is duplicated", where, g.ID))
		}
		grafts[g.ID] = struct{}{}
		if !uiQualifiedID.MatchString(g.Part) {
			errs = append(errs, fmt.Sprintf("%s.part %q must be a qualified Part id", where, g.Part))
		} else if key != "" && strings.HasPrefix(g.Part, key+".") {
			if _, ok := parts[g.Part]; !ok {
				errs = append(errs, fmt.Sprintf("%s.part %q is not in this manifest's ui.exposes", where, g.Part))
			}
		}
		if _, ok := known[g.Seam]; !ok {
			warn(RuleSlotKindUnknown, fmt.Sprintf("%s.seam %q is not in the SDK/host catalog nor published by this manifest's ui.seams; the host ignores a seam it does not publish", where, g.Seam))
		}
		if g.Requires != "" {
			if _, err := semver.NewConstraint(g.Requires); err != nil {
				errs = append(errs, fmt.Sprintf("%s.requires %q is not a semver range", where, g.Requires))
			}
		}
		if g.Mode == "replace" && m.Kind != KindPreset {
			errs = append(errs, fmt.Sprintf("%s.mode replace is only allowed in a Preset (with fallback to the original)", where))
		}
		for _, w := range g.Writes {
			if isManagedColumn(strings.TrimSuffix(w, ".*")) {
				errs = append(errs, fmt.Sprintf("%s.writes lists managed column %q", where, w))
			}
		}
		errs = append(errs, uiNetworkErrs(where, &UINeeds{Network: g.Network, Hosts: g.Hosts})...)
		if g.When != nil && g.When.Setting != nil && strings.TrimSpace(g.When.Setting.Key) == "" {
			errs = append(errs, fmt.Sprintf("%s.when.setting.key is required", where))
		}
	}
	return errs
}

func uiNetworkErrs(where string, n *UINeeds) []string {
	if n == nil {
		return nil
	}
	switch n.Network {
	case "hosts":
		if len(n.Hosts) == 0 {
			return []string{fmt.Sprintf("%s.network hosts requires a non-empty hosts list", where)}
		}
	case "", "none", "org":
		if len(n.Hosts) > 0 {
			return []string{fmt.Sprintf("%s.hosts is only valid with network hosts", where)}
		}
	}
	return nil
}

func isManagedColumn(c string) bool {
	for _, m := range UIManagedColumns {
		if c == m {
			return true
		}
	}
	return false
}
