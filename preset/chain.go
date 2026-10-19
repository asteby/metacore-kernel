package preset

import (
	"fmt"

	"github.com/Masterminds/semver/v3"

	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// FetchPreset returns the published preset that satisfies ext (the highest
// version in ext.Version, by the host's catalog) with its bundle's
// defaults/** and assets/** files.
type FetchPreset func(ext v3.PresetExtends) (*v3.Manifest, map[string][]byte, error)

// ResolveChain resolves top (a standalone preset or an overlay) to the
// effective preset the host installs. It walks preset.extends top-down,
// fetching each base, with a hard bound: at most MaxLineage presets
// (ErrLineageTooDeep) and no preset key twice (ErrCyclicExtends), so an
// A→B→A chain fails instead of looping. A fetched base must be the key and
// a version the extends asks for (ErrBaseMismatch). The chain is then
// flattened bottom-up with FlattenWithOptions. The []string lists the
// unresolved "*_file" defaults, prefixed with their preset key.
func ResolveChain(top *v3.Manifest, files map[string][]byte, fetch FetchPreset, opts FlattenOptions) (Resolved, []string, error) {
	if top == nil || top.Kind != v3.KindPreset || top.Preset == nil {
		return Resolved{}, nil, ErrNotPreset
	}
	type link struct {
		m     *v3.Manifest
		files map[string][]byte
	}
	chain := []link{{top, files}}
	visited := map[string]bool{top.Metadata.Key: true}
	for cur := top; BaseOf(cur) != nil; {
		ext := *BaseOf(cur)
		if len(chain) >= MaxLineage {
			return Resolved{}, nil, fmt.Errorf("%w: more than %d presets", ErrLineageTooDeep, MaxLineage)
		}
		if visited[ext.Key] {
			return Resolved{}, nil, fmt.Errorf("%w: %q already in the chain", ErrCyclicExtends, ext.Key)
		}
		if fetch == nil {
			return Resolved{}, nil, fmt.Errorf("%w (%s extends %s %s)", ErrNeedsBase, cur.Metadata.Key, ext.Key, ext.Version)
		}
		bm, bfiles, err := fetch(ext)
		if err != nil {
			return Resolved{}, nil, fmt.Errorf("preset: fetch %s %s: %w", ext.Key, ext.Version, err)
		}
		if bm == nil || bm.Metadata.Key != ext.Key {
			return Resolved{}, nil, fmt.Errorf("%w: asked for %q", ErrBaseMismatch, ext.Key)
		}
		c, err := semver.NewConstraint(ext.Version)
		if err != nil {
			return Resolved{}, nil, fmt.Errorf("%w: range %q: %v", ErrBaseMismatch, ext.Version, err)
		}
		if v, err := semver.NewVersion(bm.Metadata.Version); err != nil || !c.Check(v) {
			return Resolved{}, nil, fmt.Errorf("%w: %s@%s is not in %q", ErrBaseMismatch, bm.Metadata.Key, bm.Metadata.Version, ext.Version)
		}
		visited[ext.Key] = true
		chain = append(chain, link{bm, bfiles})
		cur = bm
	}

	bottom := chain[len(chain)-1]
	r, missing, err := ResolveWithFiles(bottom.m, bottom.files)
	if err != nil {
		return Resolved{}, nil, err
	}
	var allMissing []string
	for _, k := range missing {
		allMissing = append(allMissing, bottom.m.Metadata.Key+": "+k)
	}
	for i := len(chain) - 2; i >= 0; i-- {
		l := chain[i]
		var miss []string
		r, miss, err = FlattenWithOptions(r, l.m, l.files, opts)
		if err != nil {
			return Resolved{}, nil, err
		}
		for _, k := range miss {
			allMissing = append(allMissing, l.m.Metadata.Key+": "+k)
		}
	}
	return r, allMissing, nil
}
