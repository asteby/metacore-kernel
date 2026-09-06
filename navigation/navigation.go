// Package navigation merges core sidebar groups with addon-contributed ones.
// The host exposes the result at GET /api/navigation so the frontend renders
// a single tree without knowing which entries came from addons.
package navigation

import (
	"sort"
	"strings"

	"github.com/asteby/metacore-kernel/manifest"
)

// Item is the serialized sidebar entry returned to the frontend.
type Item struct {
	Title      string `json:"title"`
	URL        string `json:"url,omitempty"`
	Icon       string `json:"icon,omitempty"`
	Model      string `json:"model,omitempty"`
	Permission string `json:"permission,omitempty"`
	// Filter is a static column→value filter the host applies when rendering
	// this entry's list view. It lets an addon publish one nav entry per status
	// (e.g. {"status":"open"} for an "Open" entry pointing at the same model),
	// so the host can deep-link each entry to a distinct, pre-filtered URL.
	// Carried verbatim from the manifest NavItem; omitted when empty.
	Filter map[string]string `json:"filter,omitempty"`
	// ViewType / GroupBy are the v3 kanban hint carried verbatim from the
	// manifest NavItem. ViewType ("kanban" | "table") lets two sibling entries
	// target the SAME model and differ only by presentation (e.g. github's
	// "Board" view_type=kanban vs "Issues" view_type=table); the host encodes
	// them as a real `?view=…&group_by=…` query so each entry gets a DISTINCT,
	// exact href and the sidebar active-state matcher highlights only the open
	// one instead of every sibling at once. Omitted when empty. Snake-case JSON
	// to match the host's NavItem reader (`view_type` / `group_by`).
	ViewType string `json:"view_type,omitempty"`
	GroupBy  string `json:"group_by,omitempty"`
	// Columns / Actions restrict the SDK's rendering of this entry's model to
	// an explicit allowlist, carried verbatim from the manifest NavItem — see
	// manifest.NavItem.Columns/Actions. Omitted means the model's defaults
	// (every column, every action), unchanged from pre-existing behaviour.
	Columns []string `json:"columns,omitempty"`
	Actions []string `json:"actions,omitempty"`
	// Owner identifies where this item came from: "core" or "addon:<key>".
	Owner string `json:"owner,omitempty"`
	Items []Item `json:"items,omitempty"`
}

// Group is a top-level sidebar section.
type Group struct {
	Title string `json:"title"`
	Icon  string `json:"icon,omitempty"`
	// Target — if set, this group merges into a core group with the same id.
	Target string `json:"target,omitempty"`
	Items  []Item `json:"items"`
}

// Contribution is one addon's manifest navigation paired with its key.
type Contribution struct {
	AddonKey string
	Groups   []manifest.NavGroup
}

// InstalledFn answers "is this addon key installed for the organization whose
// navigation we are building?". It is what resolves a contribution's
// server-side `condition` (see manifest.ConditionDef) — in the kernel it is
// backed by installer.Installer.IsInstalled.
type InstalledFn func(addonKey string) bool

// Build merges core groups with addon contributions. Core groups may expose a
// Target id; addon groups with the same Target have their items appended in.
// Orphan addon groups (no matching target) surface under a synthetic "Addons"
// bucket so nothing disappears silently.
func Build(coreGroups []Group, contributions []Contribution) []Group {
	return BuildFor(coreGroups, contributions, nil)
}

// BuildFor is Build with the org's installation state in hand: groups and items
// carrying a `condition` the org does not satisfy (e.g. addon_installed points
// at an addon that org never installed) are dropped before merging, so a
// cross-addon nav entry never reaches a tenant that has no use for it. A nil
// installed keeps every conditional entry — an unaware caller loses nothing.
func BuildFor(coreGroups []Group, contributions []Contribution, installed InstalledFn) []Group {
	result := append([]Group{}, coreGroups...)
	byTarget := map[string]int{}
	for i, g := range result {
		if g.Target != "" {
			byTarget[g.Target] = i
		}
	}
	var orphans []Item
	// Stable ordering across addons.
	sort.SliceStable(contributions, func(i, j int) bool {
		return contributions[i].AddonKey < contributions[j].AddonKey
	})
	for _, c := range contributions {
		for _, ng := range c.Groups {
			if !ng.Condition.Satisfied(installed) {
				continue
			}
			items := toItems(filterByCondition(ng.Items, installed), c.AddonKey)
			if ng.Target != "" {
				if idx, ok := byTarget[ng.Target]; ok {
					result[idx].Items = append(result[idx].Items, items...)
					continue
				}
			}
			if strings.TrimSpace(ng.Title) == "" {
				orphans = append(orphans, items...)
				continue
			}
			result = append(result, Group{
				Title: ng.Title,
				Icon:  ng.Icon,
				Items: items,
			})
		}
	}
	if len(orphans) > 0 {
		result = append(result, Group{Title: "sidebar.addons", Icon: "Puzzle", Items: orphans})
	}
	return result
}

// filterByCondition drops the nav items whose condition the org does not
// satisfy, recursing into children so a conditional sub-entry disappears
// without taking its parent with it.
func filterByCondition(src []manifest.NavItem, installed InstalledFn) []manifest.NavItem {
	if installed == nil || len(src) == 0 {
		return src
	}
	out := make([]manifest.NavItem, 0, len(src))
	for _, it := range src {
		if !it.Condition.Satisfied(installed) {
			continue
		}
		it.Items = filterByCondition(it.Items, installed)
		out = append(out, it)
	}
	return out
}

func toItems(src []manifest.NavItem, addonKey string) []Item {
	out := make([]Item, 0, len(src))
	for _, it := range src {
		out = append(out, Item{
			Title:      it.Title,
			URL:        it.URL,
			Icon:       it.Icon,
			Model:      it.Model,
			Permission: it.Permission,
			Filter:     it.Filter,
			ViewType:   it.ViewType,
			GroupBy:    it.GroupBy,
			Columns:    it.Columns,
			Actions:    it.Actions,
			Owner:      "addon:" + addonKey,
			Items:      toItems(it.Items, addonKey),
		})
	}
	return out
}
