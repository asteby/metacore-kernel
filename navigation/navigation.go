// Package navigation merges core sidebar groups with addon-contributed ones.
// The host exposes the result at GET /api/navigation so the frontend renders
// a single tree without knowing which entries came from addons.
package navigation

import (
	"sort"
	"strings"

	"github.com/asteby/metacore-sdk/pkg/manifest"
)

// Item is the serialized sidebar entry returned to the frontend.
type Item struct {
	Title      string `json:"title"`
	URL        string `json:"url,omitempty"`
	Icon       string `json:"icon,omitempty"`
	Model      string `json:"model,omitempty"`
	Permission string `json:"permission,omitempty"`
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

// Build merges core groups with addon contributions. Core groups may expose a
// Target id; addon groups with the same Target have their items appended in.
// Orphan addon groups (no matching target) surface under a synthetic "Addons"
// bucket so nothing disappears silently.
func Build(coreGroups []Group, contributions []Contribution) []Group {
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
			items := toItems(ng.Items, c.AddonKey)
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

func toItems(src []manifest.NavItem, addonKey string) []Item {
	out := make([]Item, 0, len(src))
	for _, it := range src {
		out = append(out, Item{
			Title:      it.Title,
			URL:        it.URL,
			Icon:       it.Icon,
			Model:      it.Model,
			Permission: it.Permission,
			Owner:      "addon:" + addonKey,
			Items:      toItems(it.Items, addonKey),
		})
	}
	return out
}
