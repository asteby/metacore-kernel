// Package preset resolves a kind:"Preset" manifest into the ordered list of
// addons a host must install to stand up a whole vertical (e.g. Pitsline) as
// a single unit, and orchestrates installing each one through the host's
// EXISTING per-addon install path.
//
// The kernel deliberately does NOT own bundle download, entitlement checks or
// the transport to a marketplace — those are host concerns (ops talks to the
// hub, link talks to its own registry). What the kernel owns is the contract:
// "given a parsed preset, here is the addon set, here is the order, here is an
// idempotent orchestration that delegates the real bundle install to you and
// collects a per-addon result". Hosts wire their single-addon installer into
// InstallPreset via the InstallAddonFunc callback; the heavy lifting (verify
// signature, create schema, run migrations, write the installations row) stays
// in installer.Installer.Install — preset never reinvents it.
//
//	┌────────────────────────────────────────────────────────────────────┐
//	│ host (ops)                                                           │
//	│   download preset bundle from hub → preset.ResolveFromManifest()     │
//	│   InstallPreset(preset, selection, func(addon) {                     │
//	│       download addon bundle (entitlement-gated) → installer.Install  │
//	│   })                                                                 │
//	└────────────────────────────────────────────────────────────────────┘
package preset

import (
	"errors"
	"fmt"
	"time"

	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// ErrNotPreset is returned by the resolve helpers when the manifest's kind is
// not "Preset" (or the preset block is absent). Hosts surface it as "this
// bundle is not a vertical preset".
var ErrNotPreset = errors.New("preset: manifest is not a kind:Preset (or has no preset block)")

// ErrCyclicDependency is returned by SortByDeps when the addon graph the
// preset declares cannot be topo-sorted — i.e. two addons mutually require
// each other (directly or transitively). Hosts surface it as "the preset
// declares a circular addon dependency"; the manifest author has to break
// the cycle.
var ErrCyclicDependency = errors.New("preset: cyclic addon dependency in preset.addons[].requires")

// ErrUnknownDependency is returned by SortByDeps when an addon's `requires`
// list references a key that is NOT part of the preset. Cross-preset
// dependencies are the host's marketplace concern, not the kernel's; a
// dangling reference is almost always a manifest typo or an extraction
// mistake — fail loudly.
var ErrUnknownDependency = errors.New("preset: preset.addons[].requires references an addon not in this preset")

// Addon is one entry in a resolved preset — the addon key, the version range
// the preset pins, whether it is optional (installed only when the caller
// explicitly opts in), and the in-preset addons it depends on. It mirrors
// v3.PresetAddon but is re-exported from this package so hosts can depend
// on `preset.Addon` without reaching into the v3 manifest types directly.
//
// Requires names other addon keys in the SAME preset that must land before
// this one. The kernel does NOT model cross-preset dependencies — those
// are the host's marketplace concern. Empty/nil means "no constraint
// beyond preset order".
type Addon struct {
	Key      string
	Version  string
	Optional bool
	Requires []string
}

// Resolved is the outcome of resolving a preset manifest: the preset's own key
// (e.g. "pitsline"), the full ordered addon list (required first, then
// optional — preserving manifest order within each group), and the raw
// defaults map the preset declared (roles/categories/dashboards/branding
// pointers, etc.) for the host to apply after the addons land.
type Resolved struct {
	Key      string
	Version  string
	Name     string
	Addons   []Addon
	Defaults map[string]interface{}
}

// Required returns just the non-optional addons, preserving order. These are
// installed unconditionally by InstallPreset.
func (r Resolved) Required() []Addon {
	out := make([]Addon, 0, len(r.Addons))
	for _, a := range r.Addons {
		if !a.Optional {
			out = append(out, a)
		}
	}
	return out
}

// Optional returns just the optional addons, preserving order. The host
// surfaces these as opt-in checkboxes; InstallPreset installs only the ones
// whose key the caller passes in the selection set.
func (r Resolved) Optional() []Addon {
	out := make([]Addon, 0)
	for _, a := range r.Addons {
		if a.Optional {
			out = append(out, a)
		}
	}
	return out
}

// ResolveFromManifest extracts the addon list from an already-parsed v3
// manifest. It is the lowest-level entry point — hosts that already hold a
// *v3.Manifest (e.g. after their own parse) call this directly. Returns
// ErrNotPreset when m is nil, not kind:Preset, or carries no preset block.
//
// The returned Addons preserve the manifest's declaration order with the
// required addons first and the optional ones after; install order matters
// for verticals where a base addon (inventory) must land before an addon that
// extends it (tires_inventory). The preset author controls this by ordering
// the manifest's preset.addons[] — we do NOT topologically sort here because
// the kernel does not resolve inter-addon dependency graphs; the preset author
// asserts the order, the installer's idempotency covers re-runs.
func ResolveFromManifest(m *v3.Manifest) (Resolved, error) {
	if m == nil || m.Kind != v3.KindPreset || m.Preset == nil {
		return Resolved{}, ErrNotPreset
	}
	required := make([]Addon, 0, len(m.Preset.Addons))
	optional := make([]Addon, 0)
	for _, a := range m.Preset.Addons {
		entry := Addon{
			Key:      a.Key,
			Version:  a.Version,
			Optional: a.Optional,
			Requires: append([]string(nil), a.Requires...),
		}
		if a.Optional {
			optional = append(optional, entry)
		} else {
			required = append(required, entry)
		}
	}
	merged := append(required, optional...)
	// Topologically sort the merged list so an addon that declares
	// `requires: [other]` lands after its prerequisite — independent of
	// the manifest's declaration order. SortByDeps preserves preset order
	// among addons with no constraints, so addons without `requires`
	// behave exactly as before.
	sorted, err := SortByDeps(merged)
	if err != nil {
		return Resolved{}, err
	}
	return Resolved{
		Key:      m.Metadata.Key,
		Version:  m.Metadata.Version,
		Name:     m.Metadata.Name,
		Addons:   sorted,
		Defaults: m.Preset.Defaults,
	}, nil
}

// Resolve parses raw manifest.json bytes as a v3 manifest and resolves the
// preset in one shot. Convenience wrapper for hosts that hold the raw bytes
// (e.g. ops, which reads manifest.json straight out of the downloaded preset
// bundle). Parse errors and non-preset manifests both surface as errors.
func Resolve(rawManifest []byte) (Resolved, error) {
	m, err := v3.Parse(rawManifest)
	if err != nil {
		return Resolved{}, fmt.Errorf("preset: parse manifest: %w", err)
	}
	return ResolveFromManifest(m)
}

// AddonStatus is the per-addon outcome InstallPreset records. Exactly one of
// Installed / Skipped / Failed is true per addon. Skipped means "already
// installed" (idempotency) OR "optional and not selected". Failed carries the
// error; for an optional addon a failure is reported but does NOT abort the
// preset, while a required addon failure aborts unless the caller set
// ContinueOnRequiredError.
type AddonStatus struct {
	Key       string
	Version   string
	Optional  bool
	Installed bool
	Skipped   bool
	Failed    bool
	// Reason is a short human-readable note: "already installed",
	// "optional not selected", or the failure detail.
	Reason string
	// Err is the underlying error when Failed is true. nil otherwise.
	Err error
}

// Summary is the aggregate result of InstallPreset.
type Summary struct {
	PresetKey string
	Statuses  []AddonStatus
}

// Installed returns the keys of addons that were freshly installed.
func (s Summary) Installed() []string { return s.filter(func(a AddonStatus) bool { return a.Installed }) }

// Skipped returns the keys of addons that were skipped (already installed or
// optional-not-selected).
func (s Summary) Skipped() []string { return s.filter(func(a AddonStatus) bool { return a.Skipped }) }

// Failed returns the keys of addons whose install failed.
func (s Summary) Failed() []string { return s.filter(func(a AddonStatus) bool { return a.Failed }) }

// FailedReasons returns the failure Reason for every failed addon, keyed by
// addon key. Failed() alone (a []string of keys) loses WHY each one failed —
// callers that only log Failed() end up with an unactionable "these 5 broke"
// with no way to tell a stale entitlement from a transient hub timeout apart.
// Use this whenever a failure is worth logging or surfacing.
func (s Summary) FailedReasons() map[string]string {
	out := make(map[string]string)
	for _, st := range s.Statuses {
		if st.Failed {
			out[st.Key] = st.Reason
		}
	}
	return out
}

func (s Summary) filter(pred func(AddonStatus) bool) []string {
	out := make([]string, 0, len(s.Statuses))
	for _, st := range s.Statuses {
		if pred(st) {
			out = append(out, st.Key)
		}
	}
	return out
}

// InstallAddonFunc is the host-supplied callback that performs the real,
// per-addon install: download the (entitlement-gated) bundle for (key,
// version) and run it through installer.Install. The kernel never reinvents
// this — it just drives the loop and collects results.
//
// The callback returns:
//
//   - (true, nil)  → freshly installed.
//   - (false, nil) → idempotent skip (already installed). The host detects
//     this however it likes (a metacore_installations row already exists).
//   - (_, err)     → install failed. For a 403 / not-entitled the host should
//     wrap a sentinel it recognises; InstallPreset only cares that it is a
//     non-nil error and records it per addon.
type InstallAddonFunc func(addon Addon) (installed bool, err error)

// Options tunes InstallPreset.
type Options struct {
	// IncludeOptional is the set of optional addon keys the caller opted into.
	// Optional addons NOT in this set are skipped with reason "optional not
	// selected". Required addons are always attempted regardless.
	IncludeOptional map[string]struct{}

	// ContinueOnRequiredError keeps going after a REQUIRED addon fails instead
	// of aborting the whole preset. Default false: the first required-addon
	// failure stops the loop (remaining addons are left untouched) and the
	// partial Summary is returned alongside the error so the host can report
	// exactly how far it got. Optional-addon failures NEVER abort regardless
	// of this flag.
	ContinueOnRequiredError bool

	// RetryAttempts is how many extra times a failed addon install is retried
	// before it's recorded as Failed. 0 (default) means no retry — one call,
	// same as before this field existed. A whole-preset install is a long
	// sequential run (20+ addons, each a real bundle fetch + schema
	// migration); a single addon occasionally failing on a transient hub
	// hiccup (rate limit, brief timeout) shouldn't sink the vertical when a
	// second attempt moments later would have worked.
	RetryAttempts int

	// RetryDelay is the pause between retry attempts. Ignored when
	// RetryAttempts is 0. Zero delay retries immediately.
	RetryDelay time.Duration
}

// IncludeOptionalSet is a small helper to build Options.IncludeOptional from a
// slice of keys (the shape ops receives over the wire).
func IncludeOptionalSet(keys []string) map[string]struct{} {
	set := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		set[k] = struct{}{}
	}
	return set
}

// SortByDeps returns a topological ordering of addons that honours every
// addon's `Requires` constraint while preserving the input order as the
// tie-breaker so addons with no constraints land in their declared
// position. Uses Kahn's algorithm with a stable in-degree zero queue.
//
// Errors:
//
//   - ErrUnknownDependency when an addon's `requires` references a key
//     not in the input — fail-fast typos and extraction bugs.
//   - ErrCyclicDependency when the graph has a cycle (Kahn's terminates
//     with fewer-than-input nodes scheduled).
func SortByDeps(addons []Addon) ([]Addon, error) {
	if len(addons) == 0 {
		return addons, nil
	}
	// Build lookup so we can detect unknown deps in one pass.
	index := make(map[string]int, len(addons))
	for i, a := range addons {
		index[a.Key] = i
	}
	// inDegree counts how many of an addon's requires haven't been
	// scheduled yet; reverse maps each prereq to the addons that depend
	// on it so we can drain it on schedule.
	inDegree := make([]int, len(addons))
	reverse := make(map[int][]int, len(addons))
	for i, a := range addons {
		for _, req := range a.Requires {
			if req == a.Key {
				// Self-referential requires == cycle of length 1.
				return nil, fmt.Errorf("%w: %q requires itself", ErrCyclicDependency, a.Key)
			}
			j, ok := index[req]
			if !ok {
				return nil, fmt.Errorf("%w: addon %q requires %q which is not in the preset",
					ErrUnknownDependency, a.Key, req)
			}
			inDegree[i]++
			reverse[j] = append(reverse[j], i)
		}
	}
	// Queue addons with in-degree zero in their original order. Kahn's
	// algorithm — drain the queue, push downstream addons whose remaining
	// in-degree drops to zero, preserving order via a slice (not a
	// priority queue, so we don't reshuffle).
	queue := make([]int, 0, len(addons))
	for i, deg := range inDegree {
		if deg == 0 {
			queue = append(queue, i)
		}
	}
	scheduled := make([]bool, len(addons))
	out := make([]Addon, 0, len(addons))
	for len(queue) > 0 {
		i := queue[0]
		queue = queue[1:]
		if scheduled[i] {
			continue
		}
		scheduled[i] = true
		out = append(out, addons[i])
		for _, j := range reverse[i] {
			inDegree[j]--
			if inDegree[j] == 0 {
				queue = append(queue, j)
			}
		}
	}
	if len(out) != len(addons) {
		// Cycle: at least one addon never reached in-degree zero.
		unresolved := make([]string, 0, len(addons)-len(out))
		for i, a := range addons {
			if !scheduled[i] {
				unresolved = append(unresolved, a.Key)
			}
		}
		return nil, fmt.Errorf("%w: unresolved addons %v", ErrCyclicDependency, unresolved)
	}
	return out, nil
}

// InstallPreset drives an idempotent install of a resolved preset's addons by
// delegating each per-addon bundle install to install. It returns a Summary
// with a per-addon AddonStatus list (installed / skipped / failed), so the
// host can surface a clear "this addon needs a license / failed / already
// present" breakdown rather than an all-or-nothing result.
//
// Semantics:
//
//   - Required addons are always attempted, in resolved order.
//   - Optional addons are attempted only when their key is in
//     opts.IncludeOptional; otherwise Skipped ("optional not selected").
//   - install returning (false, nil) is recorded as Skipped ("already
//     installed") — this is the idempotency contract: re-running the preset
//     install skips addons that already have an installation row.
//   - install returning an error on an OPTIONAL addon is recorded as Failed
//     and the loop CONTINUES (an unentitled optional addon must not block the
//     rest of the vertical).
//   - install returning an error on a REQUIRED addon is recorded as Failed and,
//     unless opts.ContinueOnRequiredError is set, the loop ABORTS and the
//     partial Summary is returned with a non-nil error.
//
// A nil install func is a programming error and returns an error immediately.
func InstallPreset(r Resolved, install InstallAddonFunc, opts Options) (Summary, error) {
	sum := Summary{PresetKey: r.Key}
	if install == nil {
		return sum, errors.New("preset: InstallAddonFunc is required")
	}
	for _, a := range r.Addons {
		// Optional addons not opted into are skipped up front — no bundle
		// download, no entitlement check, nothing.
		if a.Optional {
			if _, ok := opts.IncludeOptional[a.Key]; !ok {
				sum.Statuses = append(sum.Statuses, AddonStatus{
					Key:      a.Key,
					Version:  a.Version,
					Optional: true,
					Skipped:  true,
					Reason:   "optional not selected",
				})
				continue
			}
		}

		installed, err := install(a)
		for attempt := 0; err != nil && attempt < opts.RetryAttempts; attempt++ {
			if opts.RetryDelay > 0 {
				time.Sleep(opts.RetryDelay)
			}
			installed, err = install(a)
		}
		switch {
		case err != nil:
			sum.Statuses = append(sum.Statuses, AddonStatus{
				Key:      a.Key,
				Version:  a.Version,
				Optional: a.Optional,
				Failed:   true,
				Reason:   err.Error(),
				Err:      err,
			})
			if !a.Optional && !opts.ContinueOnRequiredError {
				return sum, fmt.Errorf("preset %q: required addon %q failed: %w", r.Key, a.Key, err)
			}
		case !installed:
			sum.Statuses = append(sum.Statuses, AddonStatus{
				Key:      a.Key,
				Version:  a.Version,
				Optional: a.Optional,
				Skipped:  true,
				Reason:   "already installed",
			})
		default:
			sum.Statuses = append(sum.Statuses, AddonStatus{
				Key:       a.Key,
				Version:   a.Version,
				Optional:  a.Optional,
				Installed: true,
				Reason:    "installed",
			})
		}
	}
	return sum, nil
}
