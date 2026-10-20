package preset

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/Masterminds/semver/v3"

	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// MaxLineage caps how many presets a flattened chain may stack: a public base,
// an optional public refinement of it, and a company's private overlay.
const MaxLineage = 3

// ErrNeedsBase is returned by ResolveFromManifest for an overlay preset
// (preset.extends set): it cannot be installed on its own. The host fetches
// the base named by BaseOf, resolves it, and calls Flatten.
var ErrNeedsBase = errors.New("preset: manifest extends another preset; resolve the base and call Flatten")

// ErrBaseMismatch is returned by Flatten when the base handed in is not the
// preset (or not a version in the range) the overlay extends.
var ErrBaseMismatch = errors.New("preset: base does not satisfy preset.extends")

// ErrLineageTooDeep is returned by Flatten when the chain would exceed MaxLineage.
var ErrLineageTooDeep = errors.New("preset: extends chain is too deep")

// ErrCyclicExtends is returned by Flatten when the overlay already appears in
// the base's lineage.
var ErrCyclicExtends = errors.New("preset: cyclic preset.extends")

// ErrIncompatibleRange is returned by Flatten when an overlay re-declares a
// base addon with a version range that cannot be combined with the base's.
var ErrIncompatibleRange = errors.New("preset: overlay addon version range is incompatible with the base")

// ErrUnknownRemoval is returned by Flatten when remove_addons names an addon
// the base does not list (almost always a typo).
var ErrUnknownRemoval = errors.New("preset: remove_addons names an addon the base does not list")

// LineageEntry is one preset of a flattened chain, base first.
type LineageEntry struct {
	Key     string
	Version string
}

// BaseOf returns the base an overlay preset extends, or nil for a standalone
// preset (or a manifest that is not a preset).
func BaseOf(m *v3.Manifest) *v3.PresetExtends {
	if m == nil || m.Kind != v3.KindPreset || m.Preset == nil {
		return nil
	}
	return m.Preset.Extends
}

// Flatten applies an overlay preset manifest on top of its already-resolved
// base and returns the effective preset the host installs. It is pure and
// deterministic.
//
//   - Addons: union by key. An overlay entry for a base key combines both
//     version ranges (both must hold), takes the overlay's optional flag and
//     unions requires. remove_addons drops base entries last; the result is
//     re-sorted on requires, so removing an addon another one requires fails.
//   - Defaults: merged per key with MergeStrategies (MergeDefaults).
//   - Key, Version and Name are the overlay's; Lineage is the base's lineage
//     plus the overlay. Files are the base's (FlattenWithFiles adds the
//     overlay's).
func Flatten(base Resolved, overlay *v3.Manifest) (Resolved, error) {
	return flatten(base, overlay, FlattenOptions{})
}

func flatten(base Resolved, overlay *v3.Manifest, opts FlattenOptions) (Resolved, error) {
	ext := BaseOf(overlay)
	if ext == nil {
		return Resolved{}, ErrNotPreset
	}
	if ext.Key != base.Key {
		return Resolved{}, fmt.Errorf("%w: extends %q, got %q", ErrBaseMismatch, ext.Key, base.Key)
	}
	c, err := semver.NewConstraint(ext.Version)
	if err != nil {
		return Resolved{}, fmt.Errorf("%w: range %q: %v", ErrBaseMismatch, ext.Version, err)
	}
	bv, err := semver.NewVersion(base.Version)
	if err != nil || !c.Check(bv) {
		return Resolved{}, fmt.Errorf("%w: %s@%s is not in %q", ErrBaseMismatch, base.Key, base.Version, ext.Version)
	}
	lineage := base.Lineage
	if len(lineage) == 0 {
		lineage = []LineageEntry{{Key: base.Key, Version: base.Version}}
	}
	for _, l := range lineage {
		if l.Key == overlay.Metadata.Key {
			return Resolved{}, fmt.Errorf("%w: %q already in the chain", ErrCyclicExtends, l.Key)
		}
	}
	if len(lineage)+1 > MaxLineage {
		return Resolved{}, fmt.Errorf("%w: %d presets (max %d)", ErrLineageTooDeep, len(lineage)+1, MaxLineage)
	}

	addons, err := mergeAddons(base.Addons, overlay.Preset.Addons, overlay.Preset.RemoveAddons)
	if err != nil {
		return Resolved{}, err
	}
	if err := checkOverlayRoles(base.Defaults["roles"], overlay.Preset.Defaults["roles"], opts.KnownPermission); err != nil {
		return Resolved{}, err
	}

	out := Resolved{
		Key:      overlay.Metadata.Key,
		Version:  overlay.Metadata.Version,
		Name:     overlay.Metadata.Name,
		Addons:   addons,
		Defaults: MergeDefaults(base.Defaults, overlay.Preset.Defaults),
		Lineage:  append(append([]LineageEntry(nil), lineage...), LineageEntry{Key: overlay.Metadata.Key, Version: overlay.Metadata.Version}),
		Files:    mergeFiles(base.Files, nil),
	}
	return out, nil
}

func mergeAddons(base []Addon, overlay []v3.PresetAddon, remove []string) ([]Addon, error) {
	merged := make([]Addon, 0, len(base)+len(overlay))
	index := make(map[string]int, len(base))
	for _, a := range base {
		index[a.Key] = len(merged)
		a.Requires = append([]string(nil), a.Requires...)
		merged = append(merged, a)
	}
	for _, o := range overlay {
		i, ok := index[o.Key]
		if !ok {
			index[o.Key] = len(merged)
			merged = append(merged, Addon{Key: o.Key, Version: o.Version, Optional: o.Optional, Requires: append([]string(nil), o.Requires...)})
			continue
		}
		version, err := combineRanges(merged[i].Version, o.Version)
		if err != nil {
			return nil, fmt.Errorf("%w: %q: %v", ErrIncompatibleRange, o.Key, err)
		}
		merged[i].Version = version
		merged[i].Optional = o.Optional
		merged[i].Requires = unionStrings(merged[i].Requires, o.Requires)
	}
	if len(remove) > 0 {
		drop := make(map[string]struct{}, len(remove))
		for _, k := range remove {
			if _, ok := index[k]; !ok {
				return nil, fmt.Errorf("%w: %q", ErrUnknownRemoval, k)
			}
			drop[k] = struct{}{}
		}
		kept := merged[:0]
		for _, a := range merged {
			if _, ok := drop[a.Key]; !ok {
				kept = append(kept, a)
			}
		}
		merged = kept
	}
	// Same ordering contract as ResolveFromManifest: required first, then
	// optional, declaration order within each group, then requires.
	ordered := make([]Addon, 0, len(merged))
	for _, a := range merged {
		if !a.Optional {
			ordered = append(ordered, a)
		}
	}
	for _, a := range merged {
		if a.Optional {
			ordered = append(ordered, a)
		}
	}
	return SortByDeps(ordered)
}

// combineRanges returns a range that requires both a and b. Identical or empty
// ranges collapse; an OR range ("||") cannot be intersected textually and is
// only accepted when both sides are equal.
func combineRanges(a, b string) (string, error) {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	switch {
	case b == "" || a == b:
		return a, nil
	case a == "":
		return b, nil
	case strings.Contains(a, "||") || strings.Contains(b, "||"):
		return "", fmt.Errorf("cannot combine %q with %q", a, b)
	}
	combined := a + ", " + b
	c, err := semver.NewConstraint(combined)
	if err != nil {
		return "", err
	}
	if !satisfiable(c, a, b) {
		return "", fmt.Errorf("%q and %q have no version in common", a, b)
	}
	return combined, nil
}

var versionLiteral = regexp.MustCompile(`v?\d+(\.\d+){0,2}`)

// satisfiable probes c with the versions the ranges are built from (and their
// next patch/minor/major): an intersection of ranges, when non-empty, always
// contains one of them. Good enough to reject ">=2.0.0, <1.0.0" at flatten
// time instead of at install time.
func satisfiable(c *semver.Constraints, ranges ...string) bool {
	probe := []*semver.Version{semver.MustParse("0.0.0")}
	for _, r := range ranges {
		for _, lit := range versionLiteral.FindAllString(r, -1) {
			v, err := semver.NewVersion(lit)
			if err != nil {
				continue
			}
			p, mi, ma := v.IncPatch(), v.IncMinor(), v.IncMajor()
			probe = append(probe, v, &p, &mi, &ma)
		}
	}
	for _, v := range probe {
		if c.Check(v) {
			return true
		}
	}
	return false
}

// Unbounded reports whether a version range has no upper bound ("*",
// ">=1.0.0"): an overlay extending such a range silently follows every major
// release of its base.
func Unbounded(versionRange string) bool {
	c, err := semver.NewConstraint(versionRange)
	return err == nil && c.Check(semver.MustParse("999999.0.0"))
}

func unionStrings(a, b []string) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, s := range append(append([]string(nil), a...), b...) {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// StrategyKind says how one preset.defaults key merges an overlay on its base.
type StrategyKind string

const (
	// MergePatch applies the overlay object as an RFC 7386 merge patch: keys
	// are merged recursively and a null value deletes the key.
	MergePatch StrategyKind = "merge_patch"
	// MergeReplace takes the overlay value whole.
	MergeReplace StrategyKind = "replace"
	// MergeKeyed merges two lists of objects by identity fields: an overlay
	// item merge-patches the base item with the same identity, a new identity
	// is appended, and an item {"_remove": true} drops it. UnionFields are
	// string lists unioned instead of replaced; a "-x" entry removes x.
	MergeKeyed StrategyKind = "keyed"
	// MergeRecords merges {"<addon.Model>": {"key": "<field>", "rows": [...]}}
	// model by model, rows keyed by the value of the declared key field.
	MergeRecords StrategyKind = "records"
)

// Strategy is the merge rule of one preset.defaults key.
type Strategy struct {
	Kind        StrategyKind
	KeyFields   []string
	UnionFields []string
	// AltKeyFields are identities tried, in order, when an item lacks
	// KeyFields (roles: key, then name, then slug — the published roles.json
	// files use different ones). An overlay item matches the base item that
	// shares its first identity present in the base.
	AltKeyFields [][]string
}

// MergeStrategies is the single table hosts, the marketplace and manifest
// linters use to merge preset.defaults. A key not listed merge-patches when
// both sides are objects and is replaced otherwise.
var MergeStrategies = map[string]Strategy{
	"branding":           {Kind: MergePatch},
	"ui_theme":           {Kind: MergePatch},
	"theme_pack":         {Kind: MergePatch},
	"sidebar_layout":     {Kind: MergePatch},
	"terminology":        {Kind: MergePatch},
	"settings":           {Kind: MergePatch},
	"appliance":          {Kind: MergePatch},
	"onboarding":         {Kind: MergeReplace},
	"roles":              {Kind: MergeKeyed, KeyFields: []string{"key"}, AltKeyFields: [][]string{{"name"}, {"slug"}}, UnionFields: []string{"permissions"}},
	"categories":         {Kind: MergeKeyed, KeyFields: []string{"name"}},
	"sidebar_templates":  {Kind: MergeKeyed, KeyFields: []string{"key"}},
	"dashboards":         {Kind: MergeKeyed, KeyFields: []string{"key"}},
	"document_templates": {Kind: MergeKeyed, KeyFields: []string{"addon", "document_key"}},
	"stages":             {Kind: MergeKeyed, KeyFields: []string{"model", "key"}},
	"extension_fields":   {Kind: MergeKeyed, KeyFields: []string{"model", "field_key"}},
	"records":            {Kind: MergeRecords},
}

// MergeDefaults merges an overlay preset's defaults on top of its base's,
// key by key, using MergeStrategies. Neither input is modified. "*_file" keys
// are plain strings here: hosts inline them (InlineFiles) before merging so a
// file-backed base and an inline overlay merge as data.
func MergeDefaults(base, overlay map[string]interface{}) map[string]interface{} {
	out := deepCopyMap(base)
	if out == nil {
		out = map[string]interface{}{}
	}
	for k, ov := range overlay {
		bv, exists := out[k]
		if !exists {
			if ov != nil {
				out[k] = deepCopy(ov)
			}
			continue
		}
		if ov == nil {
			delete(out, k)
			continue
		}
		st, known := MergeStrategies[k]
		if !known {
			st = Strategy{Kind: MergePatch}
		}
		out[k] = mergeValue(st, bv, ov)
	}
	return out
}

func mergeValue(st Strategy, base, overlay interface{}) interface{} {
	switch st.Kind {
	case MergeReplace:
		return deepCopy(overlay)
	case MergeKeyed:
		bl, okb := base.([]interface{})
		ol, oko := overlay.([]interface{})
		if okb && oko {
			return mergeKeyed(bl, ol, st)
		}
	case MergeRecords:
		bm, okb := base.(map[string]interface{})
		om, oko := overlay.(map[string]interface{})
		if okb && oko {
			return mergeRecords(bm, om)
		}
	}
	return mergePatch(base, overlay)
}

// mergePatch is RFC 7386: objects merge recursively, null deletes, anything
// else replaces.
func mergePatch(base, patch interface{}) interface{} {
	pm, ok := patch.(map[string]interface{})
	if !ok {
		return deepCopy(patch)
	}
	bm, ok := base.(map[string]interface{})
	if !ok {
		bm = map[string]interface{}{}
	}
	out := deepCopyMap(bm)
	for k, v := range pm {
		if v == nil {
			delete(out, k)
			continue
		}
		out[k] = mergePatch(out[k], v)
	}
	return out
}

func mergeKeyed(base, overlay []interface{}, st Strategy) []interface{} {
	out := make([]interface{}, 0, len(base)+len(overlay))
	index := map[string]int{}
	for _, item := range base {
		for _, id := range identities(item, st) {
			if _, dup := index[id]; !dup {
				index[id] = len(out)
			}
		}
		out = append(out, deepCopy(item))
	}
	removed := map[int]bool{}
	for _, item := range overlay {
		om, isObj := item.(map[string]interface{})
		ids := identities(item, st)
		if !isObj || len(ids) == 0 {
			out = append(out, deepCopy(item))
			continue
		}
		i, exists := -1, false
		for _, id := range ids {
			if j, ok := index[id]; ok {
				i, exists = j, true
				break
			}
		}
		if r, _ := om["_remove"].(bool); r {
			if exists {
				removed[i] = true
				// Forget the identity: a later item with it in the same list
				// is a NEW item, not a resurrection merged with the base one.
				for id, j := range index {
					if j == i {
						delete(index, id)
					}
				}
			}
			continue
		}
		if !exists || removed[i] {
			fresh := deepCopyMap(om)
			for _, f := range st.UnionFields {
				if ov, ok := om[f]; ok {
					fresh[f] = unionList(nil, ov) // a "-x" entry has nothing to remove here
				}
			}
			for _, id := range ids {
				index[id] = len(out)
			}
			out = append(out, fresh)
			continue
		}
		bm, _ := out[i].(map[string]interface{})
		merged, _ := mergePatch(bm, om).(map[string]interface{})
		for _, f := range st.UnionFields {
			if ov, ok := om[f]; ok {
				merged[f] = unionList(bm[f], ov)
			}
		}
		out[i] = merged
	}
	if len(removed) == 0 {
		return out
	}
	kept := make([]interface{}, 0, len(out))
	for i, item := range out {
		if !removed[i] {
			kept = append(kept, item)
		}
	}
	return kept
}

// unionList unions two string lists; an overlay "-x" removes x.
func unionList(base, overlay interface{}) []interface{} {
	out := []interface{}{}
	seen := map[string]int{}
	add := func(s string) {
		if _, ok := seen[s]; !ok {
			seen[s] = len(out)
			out = append(out, s)
		}
	}
	if bl, ok := base.([]interface{}); ok {
		for _, v := range bl {
			if s, ok := v.(string); ok {
				add(s)
			}
		}
	}
	drop := map[string]bool{}
	if ol, ok := overlay.([]interface{}); ok {
		for _, v := range ol {
			s, ok := v.(string)
			if !ok {
				continue
			}
			if strings.HasPrefix(s, "-") {
				drop[strings.TrimPrefix(s, "-")] = true
				continue
			}
			add(s)
		}
	}
	if len(drop) == 0 {
		return out
	}
	kept := make([]interface{}, 0, len(out))
	for _, v := range out {
		if !drop[v.(string)] {
			kept = append(kept, v)
		}
	}
	return kept
}

func mergeRecords(base, overlay map[string]interface{}) map[string]interface{} {
	out := deepCopyMap(base)
	for model, ov := range overlay {
		om, ok := ov.(map[string]interface{})
		bm, okb := out[model].(map[string]interface{})
		if ov == nil {
			delete(out, model)
			continue
		}
		if !ok || !okb {
			out[model] = deepCopy(ov)
			continue
		}
		key, _ := om["key"].(string)
		if key == "" {
			key, _ = bm["key"].(string)
		}
		merged, _ := mergePatch(bm, om).(map[string]interface{})
		br, _ := bm["rows"].([]interface{})
		orows, _ := om["rows"].([]interface{})
		if key != "" && orows != nil {
			merged["rows"] = mergeKeyed(br, orows, Strategy{Kind: MergeKeyed, KeyFields: []string{key}})
		}
		out[model] = merged
	}
	return out
}

// identity is the stable identity of item over fields: field names and
// JSON-encoded values, so 1 and "1" never collide.
func identity(item interface{}, fields []string) (string, bool) {
	m, ok := item.(map[string]interface{})
	if !ok || len(fields) == 0 {
		return "", false
	}
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		v, ok := m[f]
		if !ok || v == nil {
			return "", false
		}
		enc, err := json.Marshal(v)
		if err != nil {
			return "", false
		}
		parts = append(parts, f+"="+string(enc))
	}
	return strings.Join(parts, "\x00"), true
}

// identities lists every identity item has under st (KeyFields first, then
// AltKeyFields), in order.
func identities(item interface{}, st Strategy) []string {
	var out []string
	for _, fields := range append([][]string{st.KeyFields}, st.AltKeyFields...) {
		if id, ok := identity(item, fields); ok {
			out = append(out, id)
		}
	}
	return out
}

func deepCopy(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		return deepCopyMap(t)
	case []interface{}:
		out := make([]interface{}, len(t))
		for i, x := range t {
			out[i] = deepCopy(x)
		}
		return out
	default:
		return t
	}
}

func deepCopyMap(m map[string]interface{}) map[string]interface{} {
	if m == nil {
		return nil
	}
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		out[k] = deepCopy(v)
	}
	return out
}

// InlineFiles resolves the top-level "<name>_file" keys of preset.defaults
// (e.g. "roles_file": "./defaults/roles.json") by reading the JSON file from
// the preset bundle and setting "<name>" to its decoded value (a file whose
// only key is "<name>" is unwrapped). An inline
// "<name>" already present wins and the file is not read. The "_file" key is
// kept, so a re-run is a no-op. read receives the path without a leading "./".
// It returns the keys it could not resolve (missing or invalid file) in
// sorted order; those keep no inline value.
func InlineFiles(defaults map[string]interface{}, read func(path string) ([]byte, error)) (map[string]interface{}, []string) {
	out := deepCopyMap(defaults)
	if out == nil || read == nil {
		return out, nil
	}
	var failed []string
	for k, v := range defaults {
		if !strings.HasSuffix(k, "_file") {
			continue
		}
		name := strings.TrimSuffix(k, "_file")
		path, ok := v.(string)
		if !ok || name == "" || strings.TrimSpace(path) == "" {
			continue
		}
		if _, inline := out[name]; inline {
			continue
		}
		raw, err := read(strings.TrimPrefix(strings.TrimSpace(path), "./"))
		if err != nil {
			failed = append(failed, k)
			continue
		}
		var decoded interface{}
		if err := json.Unmarshal(raw, &decoded); err != nil {
			failed = append(failed, k)
			continue
		}
		// A file may wrap its value under its own name ({"roles": [...]} in
		// roles.json, the appliance shape): unwrap it so both shapes inline
		// to the same value.
		if obj, ok := decoded.(map[string]interface{}); ok && len(obj) == 1 {
			if inner, wrapped := obj[name]; wrapped {
				decoded = inner
			}
		}
		out[name] = decoded
	}
	sort.Strings(failed)
	return out, failed
}
