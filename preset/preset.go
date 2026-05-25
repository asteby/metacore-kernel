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

	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// ErrNotPreset is returned by the resolve helpers when the manifest's kind is
// not "Preset" (or the preset block is absent). Hosts surface it as "this
// bundle is not a vertical preset".
var ErrNotPreset = errors.New("preset: manifest is not a kind:Preset (or has no preset block)")

// Addon is one entry in a resolved preset — the addon key, the version range
// the preset pins, and whether it is optional (installed only when the caller
// explicitly opts in). It mirrors v3.PresetAddon but is re-exported from this
// package so hosts can depend on `preset.Addon` without reaching into the v3
// manifest types directly.
type Addon struct {
	Key      string
	Version  string
	Optional bool
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
		entry := Addon{Key: a.Key, Version: a.Version, Optional: a.Optional}
		if a.Optional {
			optional = append(optional, entry)
		} else {
			required = append(required, entry)
		}
	}
	return Resolved{
		Key:      m.Metadata.Key,
		Version:  m.Metadata.Version,
		Name:     m.Metadata.Name,
		Addons:   append(required, optional...),
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
