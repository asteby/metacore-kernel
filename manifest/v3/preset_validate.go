package v3

import (
	"fmt"
	"strings"

	"github.com/Masterminds/semver/v3"
)

// OverlayMinKernel is the kernel contract version (manifest.APIVersion) that
// understands preset.extends and metadata.visibility.
const OverlayMinKernel = "3.1.0"

// validateMetadataVisibility rejects a visibility outside public|private.
// Empty means public.
func validateMetadataVisibility(m *Manifest) []string {
	switch m.Metadata.Visibility {
	case "", VisibilityPublic, VisibilityPrivate:
		return nil
	}
	return []string{fmt.Sprintf("metadata.visibility %q is not one of public|private", m.Metadata.Visibility)}
}

// validateOverlayKernelGate: a manifest that declares metadata.visibility or
// preset.extends must require a kernel that understands them (compatibility
// requires kernel, excluding 3.0.x). A 3.0.x host then reports the manifest
// incompatible (EvaluateCompatibility) and a marketplace does not serve it,
// instead of installing an overlay as if it were the whole preset.
func validateOverlayKernelGate(m *Manifest) []string {
	extends := m.Preset != nil && m.Preset.Extends != nil
	if m.Metadata.Visibility == "" && !extends {
		return nil
	}
	what := "metadata.visibility"
	if extends {
		what = "preset.extends"
	}
	for _, r := range m.Compatibility.Requires {
		if r.Key != "kernel" {
			continue
		}
		c, err := semver.NewConstraint(r.Version)
		if err != nil {
			return nil // reported by the compatibility rules
		}
		if c.Check(semver.MustParse("3.0.0")) || !c.Check(semver.MustParse(OverlayMinKernel)) {
			return []string{fmt.Sprintf("%s needs compatibility.requires kernel \">=%s\" (got %q): older hosts do not understand it", what, OverlayMinKernel, r.Version)}
		}
		return nil
	}
	return []string{fmt.Sprintf("%s needs compatibility.requires kernel \">=%s\": older hosts do not understand it", what, OverlayMinKernel)}
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
	errs = append(errs, validatePresetBranding(p.Defaults)...)
	return errs
}

// validatePresetBranding checks an inline preset.defaults.branding. A
// branding_file is checked when the bundle is read (bundle.Read), where both
// the file and the assets it references are available. A null branding is an
// overlay removing the base's and is valid.
func validatePresetBranding(defaults map[string]interface{}) []string {
	v, ok := defaults["branding"]
	if !ok || v == nil {
		return nil
	}
	b, errs := ParseBranding(v)
	if len(errs) == 0 {
		errs = b.Validate()
	}
	for i, e := range errs {
		errs[i] = "preset.defaults." + e
	}
	return errs
}
