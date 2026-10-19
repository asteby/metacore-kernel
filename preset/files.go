package preset

import (
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// ReadFileFrom adapts a preset bundle's file map (bundle.Bundle.PresetFiles,
// keys like "defaults/roles.json") to the read func InlineFiles takes. A
// leading "./" is accepted; a missing or escaping path is fs.ErrNotExist.
func ReadFileFrom(files map[string][]byte) func(p string) ([]byte, error) {
	return func(p string) ([]byte, error) {
		rel := strings.TrimPrefix(strings.TrimSpace(p), "./")
		if rel == "" || path.Clean(rel) != rel {
			return nil, fmt.Errorf("preset: %q: %w", p, fs.ErrNotExist)
		}
		data, ok := files[rel]
		if !ok {
			return nil, fmt.Errorf("preset: %q: %w", p, fs.ErrNotExist)
		}
		return data, nil
	}
}

// ResolveWithFiles is ResolveFromManifest for a preset read from its bundle:
// it inlines the "*_file" defaults from files (InlineFiles) and keeps files in
// Resolved.Files so the host can materialize the assets the defaults
// reference. The []string lists the "*_file" keys that could not be resolved.
func ResolveWithFiles(m *v3.Manifest, files map[string][]byte) (Resolved, []string, error) {
	r, err := ResolveFromManifest(m)
	if err != nil {
		return Resolved{}, nil, err
	}
	defaults, missing := InlineFiles(r.Defaults, ReadFileFrom(files))
	r.Defaults = defaults
	r.Files = mergeFiles(nil, files)
	return r, missing, nil
}

// FlattenWithFiles is Flatten for an overlay read from its bundle: the
// overlay's "*_file" defaults are inlined from overlayFiles before the merge
// (the base must come from ResolveWithFiles or a previous FlattenWithFiles),
// and Resolved.Files is the base's files with the overlay's on top, so an
// overlay asset at the same path wins. The []string lists the overlay's
// "*_file" keys that could not be resolved.
func FlattenWithFiles(base Resolved, overlay *v3.Manifest, overlayFiles map[string][]byte) (Resolved, []string, error) {
	if BaseOf(overlay) == nil {
		return Resolved{}, nil, ErrNotPreset
	}
	defaults, missing := InlineFiles(overlay.Preset.Defaults, ReadFileFrom(overlayFiles))
	m := *overlay
	p := *overlay.Preset
	p.Defaults = defaults
	m.Preset = &p
	r, err := Flatten(base, &m)
	if err != nil {
		return Resolved{}, nil, err
	}
	r.Files = mergeFiles(base.Files, overlayFiles)
	return r, missing, nil
}

// Branding returns the effective brand kit of the resolved preset:
// Defaults["branding"] (already inlined), with theme_key, font and density
// taken from Defaults["ui_theme"] when branding leaves them empty. The
// []string reports invalid fields and, when Files is set, "./assets/..."
// references missing from it. A preset without branding returns a zero
// Branding and no problems.
func (r Resolved) Branding() (v3.Branding, []string) {
	var b v3.Branding
	var problems []string
	if raw, ok := r.Defaults["branding"]; ok && raw != nil {
		b, problems = v3.ParseBranding(raw)
		if len(problems) == 0 {
			problems = b.Validate()
		}
	}
	if theme, ok := r.Defaults["ui_theme"].(map[string]interface{}); ok {
		fill := func(dst *string, key string) {
			if s, ok := theme[key].(string); ok && *dst == "" {
				*dst = s
			}
		}
		fill(&b.ThemeKey, "theme_key")
		fill(&b.Font, "font")
		fill(&b.Density, "density")
	}
	if r.Files != nil {
		for _, p := range b.AssetPaths() {
			if _, ok := r.Files[p]; !ok {
				problems = append(problems, fmt.Sprintf("branding asset ./%s is not in the preset bundle", p))
			}
		}
	}
	return b, problems
}

// mergeFiles returns a new map with base's entries and overlay's on top.
// Byte slices are shared, not copied: bundle contents are read-only.
func mergeFiles(base, overlay map[string][]byte) map[string][]byte {
	if base == nil && overlay == nil {
		return nil
	}
	out := make(map[string][]byte, len(base)+len(overlay))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range overlay {
		out[k] = v
	}
	return out
}

// AssetRefs returns every bundle path ("assets/...") referenced by a
// "./assets/..." string anywhere in defaults (branding, kiosk or display
// profiles, records...), sorted and de-duplicated. Hosts materialize these
// from Resolved.Files and then call RewriteAssetRefs.
func AssetRefs(defaults map[string]interface{}) []string {
	seen := map[string]struct{}{}
	walkStrings(defaults, func(s string) string {
		if p, ok := v3.BundleAssetPath(s); ok {
			seen[p] = struct{}{}
		}
		return s
	})
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// RewriteAssetRefs returns a copy of defaults with every "./assets/..." string
// whose bundle path is in urls replaced by urls[path]. Other values, and refs
// not in urls, are kept.
func RewriteAssetRefs(defaults map[string]interface{}, urls map[string]string) map[string]interface{} {
	out := deepCopyMap(defaults)
	walkStrings(out, func(s string) string {
		if p, ok := v3.BundleAssetPath(s); ok {
			if u, found := urls[p]; found {
				return u
			}
		}
		return s
	})
	return out
}

// walkStrings calls fn on every string value (not key) under v, in place.
func walkStrings(v interface{}, fn func(string) string) {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, c := range t {
			if s, ok := c.(string); ok {
				t[k] = fn(s)
				continue
			}
			walkStrings(c, fn)
		}
	case []interface{}:
		for i, c := range t {
			if s, ok := c.(string); ok {
				t[i] = fn(s)
				continue
			}
			walkStrings(c, fn)
		}
	}
}
