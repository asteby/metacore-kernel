package v3

import "strings"

// ProjectNavRequires turns structured NavRequire entries (model + actions) into
// ModelKey capability strings (Product + index → product.index), then unions any
// flat requires_capabilities escape-hatch strings. Order is stable: structured
// first, then flat; duplicates are dropped.
func ProjectNavRequires(reqs []NavRequire, flat []string) []string {
	seen := make(map[string]struct{}, len(reqs)*2+len(flat))
	out := make([]string, 0, len(reqs)*2+len(flat))
	add := func(cap string) {
		cap = strings.TrimSpace(cap)
		if cap == "" {
			return
		}
		if _, ok := seen[cap]; ok {
			return
		}
		seen[cap] = struct{}{}
		out = append(out, cap)
	}
	for _, r := range reqs {
		mod := strings.TrimSpace(r.Model)
		if i := strings.LastIndex(mod, "."); i >= 0 {
			mod = mod[i+1:]
		}
		mod = strings.ToLower(mod)
		if mod == "" {
			continue
		}
		for _, a := range r.Actions {
			a = strings.ToLower(strings.TrimSpace(a))
			if a == "" {
				continue
			}
			add(mod + "." + a)
		}
	}
	for _, c := range flat {
		add(c)
	}
	return out
}
