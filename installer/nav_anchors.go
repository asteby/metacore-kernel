package installer

import (
	"fmt"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/navigation"
	"github.com/google/uuid"
)

// ErrNavAnchors carries the navigation-anchor findings an install or upgrade
// would ADD to the organization (navigation.CheckInstalled): two definitive
// owners of one anchor, or a plug-in whose `as` the present anchor does not
// accept. Findings that already existed without the candidate are not its
// fault and are not reported.
type ErrNavAnchors struct {
	Addon    string
	Problems []string
}

func (e ErrNavAnchors) Error() string {
	return fmt.Sprintf("installer: addon %q navigation anchors: %v", e.Addon, e.Problems)
}

// usesNavAnchors reports whether a manifest publishes anchors or plugs items
// into one. Legacy manifests skip the check entirely.
func usesNavAnchors(m manifest.Manifest) bool {
	if len(m.NavAnchors) > 0 {
		return true
	}
	for _, g := range m.Navigation {
		for _, it := range g.Items {
			if it.AttachTo != "" {
				return true
			}
		}
	}
	return false
}

// NavAnchorProblems is the pure part of the gate: the CheckInstalled errors
// of installed+candidate that installed alone does not have.
func NavAnchorProblems(installed []manifest.Manifest, candidate manifest.Manifest) []string {
	var before, after []navigation.Addon
	for _, m := range installed {
		if m.Key == candidate.Key {
			continue // the candidate replaces its installed version
		}
		before = append(before, navigation.AddonFromManifest(m, false))
	}
	after = append(append(after, before...), navigation.AddonFromManifest(candidate, false))
	old, _ := navigation.CheckInstalled(before)
	had := map[string]bool{}
	for _, e := range old {
		had[e] = true
	}
	now, _ := navigation.CheckInstalled(after)
	var out []string
	for _, e := range now {
		if !had[e] {
			out = append(out, e)
		}
	}
	return out
}

// checkNavAnchors runs NavAnchorProblems against the manifests the lifecycle
// registry knows for the org's installed addons.
func (i *Installer) checkNavAnchors(orgID uuid.UUID, m manifest.Manifest) (problem, lookup error) {
	if !usesNavAnchors(m) {
		return nil, nil
	}
	var rows []Installation
	if err := i.DB.Where("organization_id = ?", orgID).Find(&rows).Error; err != nil {
		return nil, err
	}
	var installed []manifest.Manifest
	for _, r := range rows {
		if i.Lifecycles == nil {
			break
		}
		if lc, ok := i.Lifecycles.Get(r.AddonKey); ok {
			installed = append(installed, lc.Manifest())
		}
	}
	if ps := NavAnchorProblems(installed, m); len(ps) > 0 {
		return ErrNavAnchors{Addon: m.Key, Problems: ps}, nil
	}
	return nil, nil
}
