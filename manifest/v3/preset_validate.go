package v3

import (
	"fmt"
	"strings"

	"github.com/Masterminds/semver/v3"
)

// validateMetadataVisibility rejects a visibility outside public|private.
// Empty means public.
func validateMetadataVisibility(m *Manifest) []string {
	switch m.Metadata.Visibility {
	case "", VisibilityPublic, VisibilityPrivate:
		return nil
	}
	return []string{fmt.Sprintf("metadata.visibility %q is not one of public|private", m.Metadata.Visibility)}
}

// validatePresetBlock checks the preset block of a kind=Preset manifest:
// a standalone preset lists at least one addon; an overlay (extends) names a
// base other than itself with a valid semver range; remove_addons only makes
// sense on an overlay and cannot remove an addon the same manifest adds.
func validatePresetBlock(m *Manifest) []string {
	p := m.Preset
	if p == nil {
		return nil
	}
	var errs []string
	if p.Extends == nil {
		if len(p.Addons) == 0 {
			errs = append(errs, "preset.addons must list at least one addon (or set preset.extends)")
		}
		if len(p.RemoveAddons) > 0 {
			errs = append(errs, "preset.remove_addons requires preset.extends")
		}
	} else {
		key := strings.TrimSpace(p.Extends.Key)
		switch {
		case key == "":
			errs = append(errs, "preset.extends.key is required")
		case key == m.Metadata.Key:
			errs = append(errs, "preset.extends.key must not be the preset itself")
		}
		if _, err := semver.NewConstraint(p.Extends.Version); err != nil {
			errs = append(errs, fmt.Sprintf("preset.extends.version %q is not a semver range: %v", p.Extends.Version, err))
		}
	}
	added := make(map[string]struct{}, len(p.Addons))
	for _, a := range p.Addons {
		added[a.Key] = struct{}{}
	}
	for _, k := range p.RemoveAddons {
		if _, ok := added[k]; ok {
			errs = append(errs, fmt.Sprintf("preset.remove_addons removes %q which preset.addons also lists", k))
		}
	}
	return errs
}
