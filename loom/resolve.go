// Package loom resolves, per user and organization, which UI grafts a host
// serves (RFC 0005, docs/rfc/0005-contribuciones-de-ui.md). It is the server
// side source of truth of UI composition: the client never mounts a graft the
// server did not list.
//
// Pure: no database, no HTTP. The host gathers the installed manifests and the
// subject (permissions, effective settings, flags, installation and connector
// state) and serves Result as GET /api/ui/grafts.
package loom

import (
	"fmt"
	"sort"
	"strings"

	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// Installed is one installed addon as the host knows it.
type Installed struct {
	Manifest *v3.Manifest
	// Integrity overrides manifest.frontend.integrity when the host pinned a
	// digest at install time (the trusted one).
	Integrity string
}

// Subject is who asks. Every func is optional; a nil func denies.
type Subject struct {
	// Can answers an RBAC permission for the user in the org.
	Can func(permission string) bool
	// Setting returns the effective setting (org/branch) and whether it is set.
	Setting func(key string) (any, bool)
	// Flag answers a feature flag.
	Flag func(name string) bool
	// AddonInstalled answers Condition.addon_installed.
	AddonInstalled func(key string) bool
	// ConnectorConnected answers Condition.connector_connected for an addon.
	ConnectorConnected func(addon, connector string) bool
}

// PresetUI is the composition the preset (and the org admin) applies on top:
// grafts turned off and order overrides, by graft id. Zero value = none.
type PresetUI struct {
	Disabled map[string]bool
	Order    map[string]int
}

// PartRef is what the client needs to load the Part.
type PartRef struct {
	ID        string `json:"id"`
	Version   string `json:"version,omitempty"`
	Addon     string `json:"addon"`
	Expose    string `json:"expose"`
	Container string `json:"container,omitempty"`
	Entry     string `json:"entry,omitempty"`
	Integrity string `json:"integrity,omitempty"`
	SDK       string `json:"sdk,omitempty"`
	// SelfReview mirrors UIPart.SelfReview (gates apply:auto on the client).
	SelfReview bool `json:"self_review,omitempty"`
	// RecordsFind are the models the Part may look up through the host
	// (UINeeds.Records.Find); the client grants caps.records only for these.
	RecordsFind []string `json:"records_find,omitempty"`
}

// ClientWhen carries the clauses only the client can evaluate (they depend on
// the live screen). Everything else was decided here.
type ClientWhen struct {
	Model []string `json:"model,omitempty"`
	Mode  []string `json:"mode,omitempty"`
}

// Graft is one served graft.
type Graft struct {
	ID       string         `json:"id"`
	Addon    string         `json:"addon"`
	Seam     string         `json:"seam"`
	Part     PartRef        `json:"part"`
	Requires string         `json:"requires,omitempty"`
	Kind     string         `json:"kind"`
	Mode     string         `json:"mode"`
	Order    int            `json:"order"`
	When     *ClientWhen    `json:"when,omitempty"`
	Reads    []string       `json:"reads,omitempty"`
	Writes   []string       `json:"writes,omitempty"`
	Emits    []string       `json:"emits,omitempty"`
	Network  string         `json:"network"`
	Hosts    []string       `json:"hosts,omitempty"`
	Offline  bool           `json:"offline,omitempty"`
	Props    map[string]any `json:"props,omitempty"`
	// Inert is set when a remediable condition is unmet (unmet: disable): the
	// client renders it disabled with this reason.
	Inert string `json:"inert,omitempty"`
	// Legacy marks a projection of contributions.slots[] (context handed whole).
	Legacy bool `json:"legacy,omitempty"`
}

// Skip explains why a graft is not served (Explain).
type Skip struct {
	ID     string `json:"id"`
	Addon  string `json:"addon"`
	Seam   string `json:"seam"`
	Reason string `json:"reason"`
	Detail string `json:"detail,omitempty"`
}

// Skip reasons.
const (
	ReasonPermission    = "permission"
	ReasonSetting       = "setting"
	ReasonFlag          = "flag"
	ReasonCondition     = "condition"
	ReasonPreset        = "preset_disabled"
	ReasonPartMissing   = "part_missing"
	ReasonNoFrontend    = "no_frontend"
	ReasonSeamFiltered  = "seam_not_requested"
	ReasonDuplicateID   = "duplicate_id"
	ReasonReplaceRefuse = "replace_not_allowed"
)

// Result is what the host serves.
type Result struct {
	Grafts  []Graft `json:"grafts"`
	Skipped []Skip  `json:"skipped,omitempty"`
}

// Options narrow the answer.
type Options struct {
	// Seams, when non-empty, limits the answer to these seam ids.
	Seams []string
}

type candidate struct {
	addon string
	g     v3.UIGraft
	part  *PartRef
	// legacy slot projection
	legacy bool
	// manifest-level permission of a legacy slot
	permission string
}

// Resolve returns the grafts the subject may mount, ordered by seam, then
// order, then addon key, then graft id (deterministic).
func Resolve(installed []Installed, sub Subject, preset PresetUI, opts Options) Result {
	want := map[string]bool{}
	for _, s := range opts.Seams {
		want[s] = true
	}
	parts := map[string]PartRef{}
	var cands []candidate
	for _, in := range installed {
		m := in.Manifest
		if m == nil {
			continue
		}
		key := m.Metadata.Key
		fe := m.Frontend
		integrity := in.Integrity
		ref := func(id, version, expose string, selfReview bool) PartRef {
			p := PartRef{ID: id, Version: version, Addon: key, Expose: expose, SelfReview: selfReview}
			if fe != nil {
				p.Container, p.Entry, p.SDK = fe.Container, fe.Entry, fe.SDK
				p.Integrity = fe.Integrity
			}
			if integrity != "" {
				p.Integrity = integrity
			}
			return p
		}
		if m.UI != nil {
			for _, p := range m.UI.Exposes {
				pr := ref(p.ID, p.Version, p.Expose, p.SelfReview)
				if p.Needs != nil && p.Needs.Records != nil {
					pr.RecordsFind = append([]string(nil), p.Needs.Records.Find...)
				}
				parts[p.ID] = pr
			}
			for _, g := range m.UI.Contributions {
				cands = append(cands, candidate{addon: key, g: g})
			}
		}
		if m.Contributions != nil {
			for i, s := range m.Contributions.Slots {
				id := fmt.Sprintf("%s.slot.%d", key, i)
				pr := ref(key+"."+strings.TrimPrefix(s.Entry, "./"), "", s.Entry, false)
				cands = append(cands, candidate{
					addon:      key,
					legacy:     true,
					permission: s.Permission,
					part:       &pr,
					g: v3.UIGraft{
						ID: id, Part: pr.ID, Seam: s.SlotKind, Kind: "view", Mode: "contribute", Order: s.Order,
						Props: s.Props, When: &v3.UIWhen{Condition: s.Condition},
					},
				})
			}
		}
	}

	var res Result
	seen := map[string]bool{}
	for _, c := range cands {
		g := c.g
		skip := func(reason, detail string) {
			res.Skipped = append(res.Skipped, Skip{ID: g.ID, Addon: c.addon, Seam: g.Seam, Reason: reason, Detail: detail})
		}
		if len(want) > 0 && !want[g.Seam] {
			continue
		}
		if seen[g.ID] {
			skip(ReasonDuplicateID, "")
			continue
		}
		seen[g.ID] = true
		if preset.Disabled[g.ID] {
			skip(ReasonPreset, "")
			continue
		}
		if g.Mode == "replace" {
			// replace is preset-only (validated at publish); an addon manifest
			// that slipped one through is not served.
			skip(ReasonReplaceRefuse, "")
			continue
		}
		var part PartRef
		if c.part != nil {
			part = *c.part
		} else if p, ok := parts[g.Part]; ok {
			part = p
		} else {
			skip(ReasonPartMissing, g.Part)
			continue
		}
		if part.Entry == "" {
			skip(ReasonNoFrontend, part.Addon)
			continue
		}
		inert := ""
		if c.permission != "" && !call1(sub.Can, c.permission) {
			skip(ReasonPermission, c.permission)
			continue
		}
		var client *ClientWhen
		if w := g.When; w != nil {
			if miss := missing(w.Permission, sub.Can); len(miss) > 0 {
				skip(ReasonPermission, strings.Join(miss, ", "))
				continue
			}
			if len(w.AnyPermission) > 0 && len(missing(w.AnyPermission, sub.Can)) == len(w.AnyPermission) {
				skip(ReasonPermission, "any of "+strings.Join(w.AnyPermission, ", "))
				continue
			}
			if w.Flag != "" && !call1(sub.Flag, w.Flag) {
				skip(ReasonFlag, w.Flag)
				continue
			}
			if w.Setting != nil && !settingHolds(sub.Setting, w.Setting) {
				skip(ReasonSetting, w.Setting.Key)
				continue
			}
			if cond := w.Condition; cond != nil {
				if cond.AddonInstalled != "" && !call1(sub.AddonInstalled, cond.AddonInstalled) {
					skip(ReasonCondition, "addon_installed "+cond.AddonInstalled)
					continue
				}
				if cond.ConnectorConnected != "" && (sub.ConnectorConnected == nil || !sub.ConnectorConnected(c.addon, cond.ConnectorConnected)) {
					if cond.UnmetPolicy() == v3.UnmetHide {
						skip(ReasonCondition, "connector_connected "+cond.ConnectorConnected)
						continue
					}
					inert = "connector_not_connected:" + cond.ConnectorConnected
				}
			}
			if len(w.Model) > 0 || len(w.Mode) > 0 {
				client = &ClientWhen{Model: w.Model, Mode: w.Mode}
			}
		}
		order := g.Order
		if o, ok := preset.Order[g.ID]; ok {
			order = o
		}
		kind, mode, network := g.Kind, g.Mode, g.Network
		if kind == "" {
			kind = "view"
		}
		if mode == "" {
			mode = "contribute"
		}
		if network == "" {
			network = "none"
		}
		res.Grafts = append(res.Grafts, Graft{
			ID: g.ID, Addon: c.addon, Seam: g.Seam, Part: part, Requires: g.Requires, Kind: kind, Mode: mode,
			Order: order, When: client, Reads: g.Reads, Writes: g.Writes, Emits: g.Emits, Network: network,
			Hosts: g.Hosts, Offline: g.Offline, Props: g.Props, Inert: inert, Legacy: c.legacy,
		})
	}
	sort.SliceStable(res.Grafts, func(i, j int) bool {
		a, b := res.Grafts[i], res.Grafts[j]
		if a.Seam != b.Seam {
			return a.Seam < b.Seam
		}
		if a.Order != b.Order {
			return a.Order < b.Order
		}
		if a.Addon != b.Addon {
			return a.Addon < b.Addon
		}
		return a.ID < b.ID
	})
	if res.Grafts == nil {
		res.Grafts = []Graft{}
	}
	return res
}

func call1(f func(string) bool, v string) bool { return f != nil && f(v) }

func missing(perms []string, can func(string) bool) []string {
	var out []string
	for _, p := range perms {
		if !call1(can, p) {
			out = append(out, p)
		}
	}
	return out
}

func settingHolds(get func(string) (any, bool), w *v3.UISettingWhen) bool {
	if get == nil {
		return false
	}
	v, ok := get(w.Key)
	if !ok {
		return false
	}
	if w.Equals != nil {
		return fmt.Sprint(v) == fmt.Sprint(w.Equals)
	}
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return x != "" && x != "false" && x != "0"
	case nil:
		return false
	case float64:
		return x != 0
	case int:
		return x != 0
	}
	return true
}
