// Package i18ncheck verifies that every i18n key a v3 manifest references from
// its user-facing configuration surfaces exists in EVERY locale bundle the
// manifest declares. A missing key reaches the operator as a raw dotted key
// ("mercadopago.settings.session_expiration_minutes") or a humanized English
// fallback in a Spanish UI.
//
// Surfaces checked: settings[] (label, description, options[].label,
// input_rules.pattern_hint, input_rules.forbid_patterns[].message),
// connectors[] (label, credentials[] like settings, form_layout.sections[]
// title/description), edge_devices[].pairing_credentials[] and
// contributions.actions[] (label, fields[].label recursively incl. item_fields,
// steps[].title/description and their fields).
//
// A string is an i18n KEY when it starts with "<addon_key>." or has the shape
// of a dotted key (lowercase segments joined by dots, no spaces: e.g. a key
// under another prefix such as "tires.settings.x", which then must exist as
// is); anything else is literal text (reported as informational Literals).
package i18ncheck

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// dottedKey is the shape of an i18n key under any prefix.
var dottedKey = regexp.MustCompile(`^[a-z0-9_]+(\.[a-z0-9_-]+)+$`)

// Ref is one i18n key referenced by the manifest and where.
type Ref struct {
	Key   string
	Where string
}

// Missing is a referenced key absent from one locale.
type Missing struct {
	Key    string
	Where  string
	Locale string
}

// Report is the result for one manifest.
type Report struct {
	Addon      string
	Locales    []string
	Referenced int
	Missing    []Missing
	// Literals are non-key strings on the checked surfaces (informational).
	Literals []Ref
	// Problems are bundle-level issues (declared bundle unreadable, no
	// locales at all while keys are referenced).
	Problems []string
}

// OK reports a clean manifest.
func (r Report) OK() bool { return len(r.Missing) == 0 && len(r.Problems) == 0 }

// CollectRefs returns the i18n keys the manifest references, deduplicated by
// key (first location wins), in a stable order.
func CollectRefs(m *v3.Manifest) []Ref {
	refs, _ := collect(m)
	return refs
}

// CollectLiterals returns the user-facing strings of the same surfaces that
// are NOT i18n keys (literal text, shown untranslated in every language).
// Informational: the check does not fail on them.
func CollectLiterals(m *v3.Manifest) []Ref {
	_, lits := collect(m)
	return lits
}

func collect(m *v3.Manifest) (out, literals []Ref) {
	prefix := m.Metadata.Key + "."
	seen := map[string]bool{}
	add := func(s, where string) {
		s = strings.TrimSpace(s)
		if s == "" || m.Metadata.Key == "" {
			return
		}
		if !strings.HasPrefix(s, prefix) && !dottedKey.MatchString(s) {
			literals = append(literals, Ref{Key: s, Where: where})
			return
		}
		if seen[s] {
			return
		}
		seen[s] = true
		out = append(out, Ref{Key: s, Where: where})
	}
	setting := func(path string, s v3.Setting) {
		add(s.Label, path+".label")
		add(s.Description, path+".description")
		for _, o := range s.Options {
			add(o.Label, fmt.Sprintf("%s.options[%s].label", path, o.Value))
		}
		if r := s.InputRules; r != nil {
			add(r.PatternHint, path+".input_rules.pattern_hint")
			for i, fp := range r.ForbidPatterns {
				add(fp.Message, fmt.Sprintf("%s.input_rules.forbid_patterns[%d].message", path, i))
			}
		}
	}
	for _, s := range m.Settings {
		setting(fmt.Sprintf("settings[%s]", s.Key), s)
	}
	for _, c := range m.Connectors {
		cp := fmt.Sprintf("connectors[%s]", c.Key)
		add(c.Label, cp+".label")
		if c.FormLayout != nil {
			for _, sec := range c.FormLayout.Sections {
				add(sec.Title, fmt.Sprintf("%s.form_layout.sections[%s].title", cp, sec.Key))
				add(sec.Description, fmt.Sprintf("%s.form_layout.sections[%s].description", cp, sec.Key))
			}
		}
		for _, cr := range c.Credentials {
			setting(fmt.Sprintf("%s.credentials[%s]", cp, cr.Key), cr)
		}
	}
	for _, d := range m.EdgeDevices {
		for _, cr := range d.PairingCredentials {
			setting(fmt.Sprintf("edge_devices[%s].pairing_credentials[%s]", d.Key, cr.Key), cr)
		}
	}
	var fields func(path string, fs []v3.ActionField)
	fields = func(path string, fs []v3.ActionField) {
		for _, f := range fs {
			fp := fmt.Sprintf("%s.fields[%s]", path, f.Key)
			add(f.Label, fp+".label")
			for _, o := range f.Options.Static {
				add(o.Label, fmt.Sprintf("%s.options[%s].label", fp, o.Value))
			}
			if r := f.InputRules; r != nil {
				add(r.PatternHint, fp+".input_rules.pattern_hint")
				for i, x := range r.ForbidPatterns {
					add(x.Message, fmt.Sprintf("%s.input_rules.forbid_patterns[%d].message", fp, i))
				}
			}
			fields(fp+".item_fields", f.ItemFields)
		}
	}
	if m.Contributions != nil {
		for _, a := range m.Contributions.Actions {
			ap := fmt.Sprintf("contributions.actions[%s]", a.Key)
			add(a.Label, ap+".label")
			fields(ap, a.Fields)
			for i, st := range a.Steps {
				sp := fmt.Sprintf("%s.steps[%d]", ap, i)
				add(st.Title, sp+".title")
				add(st.Description, sp+".description")
				fields(sp, st.Fields)
			}
		}
	}
	return out, literals
}

// Flatten turns a nested locale bundle into dotted keys (leaf strings only).
func Flatten(v any) map[string]string {
	out := map[string]string{}
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, sub := range t {
				p := k
				if prefix != "" {
					p = prefix + "." + k
				}
				walk(p, sub)
			}
		case string:
			out[prefix] = t
		}
	}
	walk("", v)
	return out
}

// Check compares the references against the per-locale flat bundles. A key
// is present in a locale if it exists as-is or, for bundles written without
// the addon prefix, as the key minus "<addon_key>.". Empty strings count as
// missing.
func Check(m *v3.Manifest, locales map[string]map[string]string) Report {
	rep := Report{Addon: m.Metadata.Key}
	for l := range locales {
		rep.Locales = append(rep.Locales, l)
	}
	sort.Strings(rep.Locales)
	refs, lits := collect(m)
	rep.Referenced = len(refs)
	rep.Literals = lits
	if len(refs) > 0 && len(locales) == 0 {
		rep.Problems = append(rep.Problems, "references i18n keys but declares no locale bundle")
		return rep
	}
	prefix := m.Metadata.Key + "."
	for _, l := range rep.Locales {
		msgs := locales[l]
		for _, r := range refs {
			v, ok := msgs[r.Key]
			if !ok {
				v, ok = msgs[strings.TrimPrefix(r.Key, prefix)]
			}
			if !ok || strings.TrimSpace(v) == "" {
				rep.Missing = append(rep.Missing, Missing{Key: r.Key, Where: r.Where, Locale: l})
			}
		}
	}
	return rep
}

// LoadLocales reads the bundles a manifest in dir declares (i18n.bundles[]);
// without an i18n block it falls back to dir/locales/*.json.
func LoadLocales(dir string, m *v3.Manifest) (map[string]map[string]string, []string) {
	out := map[string]map[string]string{}
	var problems []string
	type bundle struct{ locale, path string }
	var bundles []bundle
	if m.I18n != nil && len(m.I18n.Bundles) > 0 {
		for _, b := range m.I18n.Bundles {
			bundles = append(bundles, bundle{b.Locale, b.Path})
		}
	} else {
		matches, _ := filepath.Glob(filepath.Join(dir, "locales", "*.json"))
		for _, p := range matches {
			rel, _ := filepath.Rel(dir, p)
			bundles = append(bundles, bundle{strings.TrimSuffix(filepath.Base(p), ".json"), rel})
		}
	}
	for _, b := range bundles {
		raw, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(b.path)))
		if err != nil {
			problems = append(problems, fmt.Sprintf("bundle %s (%s) unreadable: %v", b.locale, b.path, err))
			continue
		}
		var doc any
		if err := json.Unmarshal(raw, &doc); err != nil {
			problems = append(problems, fmt.Sprintf("bundle %s (%s) is not JSON: %v", b.locale, b.path, err))
			continue
		}
		flat := Flatten(doc)
		if prev, ok := out[b.locale]; ok {
			for k, v := range flat {
				prev[k] = v
			}
			continue
		}
		out[b.locale] = flat
	}
	return out, problems
}

// CheckDir loads dir/manifest.json (decoded leniently, without the schema
// validation, so the check also runs on manifests other rules reject) and its
// bundles, and returns the report.
func CheckDir(dir string) (Report, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return Report{}, err
	}
	var m v3.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return Report{}, fmt.Errorf("%s: %w", dir, err)
	}
	locales, problems := LoadLocales(dir, &m)
	rep := Check(&m, locales)
	rep.Problems = append(problems, rep.Problems...)
	return rep, nil
}
