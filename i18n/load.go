package i18n

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// LoadCatalogFS builds a Catalog from JSON (.json) or YAML (.yaml, .yml)
// files in fsys matching the glob patterns (fs.Glob syntax), typically an
// embed.FS shipped with an app or addon:
//
//	//go:embed locales/*.json
//	var locales embed.FS
//	cat, err := i18n.LoadCatalogFS(locales, "locales/*.json")
//
// The language of each file comes from its name: "es-MX.json" → es-MX,
// "branches.es.yaml" → es (the last dot-separated part before the
// extension). A file whose name is not a language tag ("messages.json") is
// read as a multi-language document: {"es": {...}, "en": {...}}.
//
// Nested objects are flattened with dots, so {"models": {"branches":
// {"table": {"title": "Sucursales"}}}} and {"models.branches.table.title":
// "Sucursales"} are equivalent. Array items become ".0", ".1", …; other
// scalars are formatted with fmt. Files are applied in lexical path order, a
// later file overriding an earlier one on the same key.
//
// It fails when a pattern is malformed, when no file matches any pattern
// (usually a wrong embed path) or when a file does not parse.
func LoadCatalogFS(fsys fs.FS, patterns ...string) (*Catalog, error) {
	if fsys == nil {
		return nil, fmt.Errorf("i18n.LoadCatalogFS: nil fs")
	}
	if len(patterns) == 0 {
		patterns = []string{"*.json", "*.yaml", "*.yml"}
	}
	seen := map[string]bool{}
	var files []string
	for _, p := range patterns {
		matches, err := fs.Glob(fsys, p)
		if err != nil {
			return nil, fmt.Errorf("i18n.LoadCatalogFS: pattern %q: %w", p, err)
		}
		for _, m := range matches {
			if !seen[m] && catalogExt(m) != "" {
				seen[m] = true
				files = append(files, m)
			}
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("i18n.LoadCatalogFS: no .json/.yaml/.yml file matches %q", patterns)
	}
	sort.Strings(files)
	cat := NewCatalog(nil)
	for _, f := range files {
		if err := loadCatalogFile(cat, fsys, f); err != nil {
			return nil, err
		}
	}
	return cat, nil
}

func catalogExt(name string) string {
	switch ext := strings.ToLower(path.Ext(name)); ext {
	case ".json", ".yaml", ".yml":
		return ext
	}
	return ""
}

var langTagRe = regexp.MustCompile(`^[A-Za-z]{2,3}([-_][A-Za-z0-9]{2,8})*$`)

func loadCatalogFile(cat *Catalog, fsys fs.FS, name string) error {
	raw, err := fs.ReadFile(fsys, name)
	if err != nil {
		return fmt.Errorf("i18n.LoadCatalogFS: read %s: %w", name, err)
	}
	var doc any
	if catalogExt(name) == ".json" {
		err = json.Unmarshal(raw, &doc)
	} else {
		err = yaml.Unmarshal(raw, &doc)
	}
	if err != nil {
		return fmt.Errorf("i18n.LoadCatalogFS: parse %s: %w", name, err)
	}
	root, ok := asStringMap(doc)
	if !ok {
		if doc == nil {
			return nil // empty file
		}
		return fmt.Errorf("i18n.LoadCatalogFS: %s: top level must be an object", name)
	}

	base := strings.TrimSuffix(path.Base(name), path.Ext(name))
	if i := strings.LastIndexByte(base, '.'); i >= 0 {
		base = base[i+1:]
	}
	if langTagRe.MatchString(base) {
		flat := map[string]string{}
		flatten("", root, flat)
		cat.Add(base, flat)
		return nil
	}
	for lang, v := range root {
		sub, ok := asStringMap(v)
		if !ok || !langTagRe.MatchString(lang) {
			return fmt.Errorf("i18n.LoadCatalogFS: %s: file name is not a language tag, so top-level keys must be languages holding objects (got %q)", name, lang)
		}
		flat := map[string]string{}
		flatten("", sub, flat)
		cat.Add(lang, flat)
	}
	return nil
}

func asStringMap(v any) (map[string]any, bool) {
	switch m := v.(type) {
	case map[string]any:
		return m, true
	case map[any]any:
		out := make(map[string]any, len(m))
		for k, val := range m {
			out[fmt.Sprint(k)] = val
		}
		return out, true
	}
	return nil, false
}

func flatten(prefix string, v any, out map[string]string) {
	join := func(k string) string {
		if prefix == "" {
			return k
		}
		return prefix + "." + k
	}
	if m, ok := asStringMap(v); ok {
		for k, val := range m {
			flatten(join(k), val, out)
		}
		return
	}
	switch t := v.(type) {
	case []any:
		for i, val := range t {
			flatten(join(strconv.Itoa(i)), val, out)
		}
	case nil:
		// null leaves no entry
	case string:
		if prefix != "" {
			out[prefix] = t
		}
	default:
		if prefix != "" {
			out[prefix] = fmt.Sprint(t)
		}
	}
}
