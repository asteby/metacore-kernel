package installer

import (
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/Masterminds/semver/v3"
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/google/uuid"
)

// This file implements peer-addon dependency resolution (compatibility.requires
// entries that name another addon, as opposed to the reserved kernel / sdk /
// runtime ranges). The core is PURE (no DB): CheckInstallable, CheckUpgradable,
// CheckUninstallable, ResolveOrder, ResolveInstall and UninstallOrder take the
// installed set and manifests as values so the hub and manifestcheck reuse them
// verbatim. The Installer wires them into Install / Upgrade / Uninstall behind
// Installer.EnforceAddonRequires.

// RequiresMode controls how the Installer reacts to unmet peer requires.
type RequiresMode string

const (
	// RequiresOff skips the peer-requires checks entirely.
	RequiresOff RequiresMode = "off"
	// RequiresWarn (the default) logs "installer.requires_unmet" and lets the
	// operation proceed, so existing instances with latent broken requires keep
	// working for one release.
	RequiresWarn RequiresMode = "warn"
	// RequiresEnforce refuses the operation with the typed errors below.
	RequiresEnforce RequiresMode = "enforce"
)

// ParseRequiresMode maps "off|warn|enforce" (case-insensitive) to a mode.
// Empty or unknown input yields RequiresWarn.
func ParseRequiresMode(s string) RequiresMode {
	switch RequiresMode(strings.ToLower(strings.TrimSpace(s))) {
	case RequiresOff:
		return RequiresOff
	case RequiresEnforce:
		return RequiresEnforce
	}
	return RequiresWarn
}

// ErrMissingDependency: Addon requires Required (range Range) and it is not
// installed (Have is empty).
type ErrMissingDependency struct {
	Addon    string
	Required string
	Range    string
	Have     string
}

func (e ErrMissingDependency) Error() string {
	return fmt.Sprintf("installer: addon %q requires %q %s, which is not installed", e.Addon, e.Required, rangeOrAny(e.Range))
}

// ErrIncompatibleVersion: Required is present at version Have, outside the
// range Addon declares. Addon is the DEPENDENT: on an install it is the
// candidate; on an upgrade of a base it is the installed dependent that would
// be left out of range.
type ErrIncompatibleVersion struct {
	Addon    string
	Required string
	Range    string
	Have     string
}

func (e ErrIncompatibleVersion) Error() string {
	return fmt.Sprintf("installer: addon %q requires %q %s, but version %s would be in place", e.Addon, e.Required, rangeOrAny(e.Range), e.Have)
}

// ErrDependencyCycle carries the cycle as a path whose first and last entries
// are the same key (a -> b -> a).
type ErrDependencyCycle struct{ Path []string }

func (e ErrDependencyCycle) Error() string {
	return "installer: dependency cycle: " + strings.Join(e.Path, " -> ")
}

// ErrHasDependents: Addon cannot be removed because installed addons require it.
type ErrHasDependents struct {
	Addon      string
	Dependents []string
}

func (e ErrHasDependents) Error() string {
	return fmt.Sprintf("installer: addon %q is required by installed addons: %s (uninstall them first or use Cascade)", e.Addon, strings.Join(e.Dependents, ", "))
}

func rangeOrAny(r string) string {
	if strings.TrimSpace(r) == "" {
		return "(any version)"
	}
	return r
}

// InstalledAddon is the resolver's view of one installed addon. Requires and
// Provides may be empty when the manifest is not known (then it can neither
// block an uninstall nor an upgrade of a base: unknown, not "no deps").
type InstalledAddon struct {
	Key      string
	Version  string
	Provides []string
	Requires []manifest.AddonRequirement
}

// InstalledFromManifest builds the resolver view of a manifest.
func InstalledFromManifest(m manifest.Manifest) InstalledAddon {
	return InstalledAddon{Key: m.Key, Version: m.Version, Provides: m.Provides, Requires: m.AddonRequires}
}

// normReq trims the key and folds the "key?" spelling into Optional, so a
// hand-built Manifest behaves like one projected by FromV3.
func normReq(r manifest.AddonRequirement) manifest.AddonRequirement {
	r.Key = strings.TrimSpace(r.Key)
	if strings.HasSuffix(r.Key, "?") {
		r.Key = strings.TrimSpace(strings.TrimSuffix(r.Key, "?"))
		r.Optional = true
	}
	r.Range = strings.TrimSpace(r.Range)
	return r
}

func peerReqs(reqs []manifest.AddonRequirement) []manifest.AddonRequirement {
	out := make([]manifest.AddonRequirement, 0, len(reqs))
	for _, r := range reqs {
		r = normReq(r)
		if r.Key == "" || manifest.IsReservedRequirementKey(r.Key) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// inRange reports whether version satisfies rng. An empty range accepts all.
// Pre-releases of the base satisfy a plain ">=x" range ("-0" widening) so a
// 1.2.0-rc.1 base does not spuriously break ">=1.0.0" dependents. An
// unparseable version or range is treated as satisfied (the validators own
// syntax errors; the resolver must not invent blocks from garbage).
func inRange(version, rng string) bool {
	rng = strings.TrimSpace(rng)
	if rng == "" || rng == "*" {
		return true
	}
	c, err := semver.NewConstraint(rng)
	if err != nil {
		return true
	}
	v, err := semver.NewVersion(strings.TrimSpace(version))
	if err != nil {
		return true
	}
	if c.Check(v) {
		return true
	}
	if v.Prerelease() != "" {
		core, err := semver.NewVersion(fmt.Sprintf("%d.%d.%d", v.Major(), v.Minor(), v.Patch()))
		if err == nil {
			return c.Check(core)
		}
	}
	return false
}

// lookup finds the installed addon that satisfies key: the addon itself, else
// one that lists key in Provides (version unknown => any range passes).
func lookup(installed []InstalledAddon, key string) (ia InstalledAddon, viaProvides bool, ok bool) {
	for _, a := range installed {
		if a.Key == key {
			return a, false, true
		}
	}
	for _, a := range installed {
		for _, p := range a.Provides {
			if p == key {
				return a, true, true
			}
		}
	}
	return InstalledAddon{}, false, false
}

// CheckInstallable verifies every non-optional peer requirement of candidate
// against the installed set. All violations are returned joined (errors.As
// finds each ErrMissingDependency / ErrIncompatibleVersion); nil when none. An
// optional requirement never fails, even when present out of range.
func CheckInstallable(installed []InstalledAddon, candidate manifest.Manifest) error {
	var errs []error
	for _, r := range peerReqs(candidate.AddonRequires) {
		if r.Optional {
			continue
		}
		have, viaProvides, ok := lookup(installed, r.Key)
		switch {
		case !ok:
			errs = append(errs, ErrMissingDependency{Addon: candidate.Key, Required: r.Key, Range: r.Range})
		case !viaProvides && !inRange(have.Version, r.Range):
			errs = append(errs, ErrIncompatibleVersion{Addon: candidate.Key, Required: r.Key, Range: r.Range, Have: have.Version})
		}
	}
	return errors.Join(errs...)
}

// CheckUpgradable is the compatibility matrix for replacing an installed
// addon by candidate (a newer version of the same key): candidate's own
// requires must hold (CheckInstallable, minus itself) AND no installed
// non-optional dependent may be left outside its declared range of
// candidate.Key. Optional dependents never block.
func CheckUpgradable(installed []InstalledAddon, candidate manifest.Manifest) error {
	others := make([]InstalledAddon, 0, len(installed))
	for _, a := range installed {
		if a.Key != candidate.Key {
			others = append(others, a)
		}
	}
	var errs []error
	if err := CheckInstallable(others, candidate); err != nil {
		errs = append(errs, err)
	}
	for _, d := range others {
		for _, r := range peerReqs(d.Requires) {
			if r.Key != candidate.Key || r.Optional {
				continue
			}
			if !inRange(candidate.Version, r.Range) {
				errs = append(errs, ErrIncompatibleVersion{Addon: d.Key, Required: candidate.Key, Range: r.Range, Have: candidate.Version})
			}
		}
	}
	return errors.Join(errs...)
}

// Dependents returns the keys of installed addons that require key with a
// non-optional requirement, sorted. A requirement on a key that key only
// "provides" counts too.
func Dependents(installed []InstalledAddon, key string) []string {
	provides := map[string]bool{key: true}
	for _, a := range installed {
		if a.Key == key {
			for _, p := range a.Provides {
				provides[p] = true
			}
		}
	}
	var out []string
	for _, d := range installed {
		if d.Key == key {
			continue
		}
		for _, r := range peerReqs(d.Requires) {
			if !r.Optional && provides[r.Key] {
				out = append(out, d.Key)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// CheckUninstallable returns ErrHasDependents when installed addons still
// require key. Cascade is the caller's explicit decision (UninstallOrder).
func CheckUninstallable(installed []InstalledAddon, key string) error {
	if deps := Dependents(installed, key); len(deps) > 0 {
		return ErrHasDependents{Addon: key, Dependents: deps}
	}
	return nil
}

// UninstallOrder returns the removal order for a cascade: every transitive
// non-optional dependent of key, dependents before the things they require
// (reverse install order), with key last.
func UninstallOrder(installed []InstalledAddon, key string) []string {
	seen := map[string]bool{key: true}
	queue := []string{key}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, d := range Dependents(installed, cur) {
			if !seen[d] {
				seen[d] = true
				queue = append(queue, d)
			}
		}
	}
	var set []manifest.Manifest
	for _, a := range installed {
		if seen[a.Key] {
			set = append(set, manifest.Manifest{Key: a.Key, Version: a.Version, Provides: a.Provides, AddonRequires: a.Requires})
		}
	}
	order, err := ResolveOrder(set)
	if err != nil { // installed cycle (legacy data): fall back to a stable order
		order = order[:0]
		for k := range seen {
			order = append(order, k)
		}
		sort.Strings(order)
	}
	out := make([]string, 0, len(order))
	for i := len(order) - 1; i >= 0; i-- {
		if order[i] != key {
			out = append(out, order[i])
		}
	}
	return append(out, key)
}

// ResolveOrder returns the keys of manifests in install order (a required
// addon before its dependents), deterministic (ties broken by key). Only edges
// between members of the set count; requirements on addons outside it are
// ignored here (CheckInstallable / ResolveInstall judge those). A "provides"
// of a member satisfies a requirement on the provided key. A cycle among
// non-optional requirements is ErrDependencyCycle; an optional edge that would
// close a cycle is dropped (soft deps never create a cycle).
func ResolveOrder(manifests []manifest.Manifest) ([]string, error) {
	keys := map[string]bool{}
	providerOf := map[string]string{}
	for _, m := range manifests {
		keys[m.Key] = true
	}
	for _, m := range manifests {
		for _, p := range m.Provides {
			if !keys[p] {
				if _, dup := providerOf[p]; !dup {
					providerOf[p] = m.Key
				}
			}
		}
	}
	resolve := func(k string) (string, bool) {
		if keys[k] {
			return k, true
		}
		if p, ok := providerOf[k]; ok {
			return p, true
		}
		return "", false
	}
	// deps[a] = set of keys a must come after.
	hard := map[string]map[string]bool{}
	var soft [][2]string
	for k := range keys {
		hard[k] = map[string]bool{}
	}
	for _, m := range manifests {
		for _, r := range peerReqs(m.AddonRequires) {
			to, ok := resolve(r.Key)
			if !ok || to == m.Key {
				continue
			}
			if r.Optional {
				soft = append(soft, [2]string{m.Key, to})
			} else {
				hard[m.Key][to] = true
			}
		}
	}
	if cyc := findCycle(hard); cyc != nil {
		return nil, ErrDependencyCycle{Path: cyc}
	}
	sort.Slice(soft, func(i, j int) bool {
		if soft[i][0] != soft[j][0] {
			return soft[i][0] < soft[j][0]
		}
		return soft[i][1] < soft[j][1]
	})
	for _, e := range soft {
		if reaches(hard, e[1], e[0]) { // adding e[0] -> e[1] would close a cycle
			continue
		}
		hard[e[0]][e[1]] = true
	}
	// Kahn, smallest key first.
	indeg := map[string]int{}
	rev := map[string][]string{}
	for k, ds := range hard {
		indeg[k] += 0
		for d := range ds {
			indeg[k]++
			rev[d] = append(rev[d], k)
		}
	}
	var ready []string
	for k, n := range indeg {
		if n == 0 {
			ready = append(ready, k)
		}
	}
	out := make([]string, 0, len(keys))
	for len(ready) > 0 {
		sort.Strings(ready)
		cur := ready[0]
		ready = ready[1:]
		out = append(out, cur)
		for _, n := range rev[cur] {
			indeg[n]--
			if indeg[n] == 0 {
				ready = append(ready, n)
			}
		}
	}
	return out, nil
}

// reaches reports whether to is reachable from from following deps edges.
func reaches(deps map[string]map[string]bool, from, to string) bool {
	seen := map[string]bool{}
	var walk func(string) bool
	walk = func(n string) bool {
		if n == to {
			return true
		}
		if seen[n] {
			return false
		}
		seen[n] = true
		for d := range deps[n] {
			if walk(d) {
				return true
			}
		}
		return false
	}
	return walk(from)
}

// findCycle returns one cycle as a path (first == last) or nil. Deterministic.
func findCycle(deps map[string]map[string]bool) []string {
	const (
		white = iota
		grey
		black
	)
	color := map[string]int{}
	var stack []string
	var cycle []string
	var visit func(string) bool
	visit = func(n string) bool {
		color[n] = grey
		stack = append(stack, n)
		next := make([]string, 0, len(deps[n]))
		for d := range deps[n] {
			next = append(next, d)
		}
		sort.Strings(next)
		for _, d := range next {
			switch color[d] {
			case grey:
				i := len(stack) - 1
				for stack[i] != d {
					i--
				}
				cycle = append(append([]string(nil), stack[i:]...), d)
				return true
			case white:
				if visit(d) {
					return true
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[n] = black
		return false
	}
	roots := make([]string, 0, len(deps))
	for k := range deps {
		roots = append(roots, k)
	}
	sort.Strings(roots)
	for _, k := range roots {
		if color[k] == white && visit(k) {
			return cycle
		}
	}
	return nil
}

// ResolveInstall plans a multi-addon install (a preset): it orders the batch
// (ResolveOrder) and checks every member's non-optional requires against the
// already-installed addons PLUS the rest of the batch (at the batch versions).
// Returns the order and the joined violations; the order is nil on a cycle.
func ResolveInstall(installed []InstalledAddon, batch []manifest.Manifest) ([]string, error) {
	order, err := ResolveOrder(batch)
	if err != nil {
		return nil, err
	}
	inBatch := map[string]bool{}
	for _, m := range batch {
		inBatch[m.Key] = true
	}
	world := make([]InstalledAddon, 0, len(installed)+len(batch))
	for _, a := range installed {
		if !inBatch[a.Key] { // the batch version replaces the installed one
			world = append(world, a)
		}
	}
	for _, m := range batch {
		world = append(world, InstalledFromManifest(m))
	}
	var errs []error
	for _, m := range batch {
		if e := CheckInstallable(world, m); e != nil {
			errs = append(errs, e)
		}
	}
	return order, errors.Join(errs...)
}

// ---------------------------------------------------------------------------
// Installer wiring
// ---------------------------------------------------------------------------

func (i *Installer) requiresMode() RequiresMode {
	if i.EnforceAddonRequires == "" {
		return RequiresWarn
	}
	return i.EnforceAddonRequires
}

// installedAddons snapshots the org's installed addons with the manifests the
// in-process lifecycle registry knows (requires/provides). Rows without a
// known manifest still count as installed (key + version).
func (i *Installer) installedAddons(orgID uuid.UUID) ([]InstalledAddon, error) {
	var rows []Installation
	if err := i.DB.Where("organization_id = ?", orgID).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]InstalledAddon, 0, len(rows))
	for _, r := range rows {
		ia := InstalledAddon{Key: r.AddonKey, Version: r.Version}
		if lc, ok := i.Lifecycles.Get(r.AddonKey); ok {
			m := lc.Manifest()
			ia.Provides, ia.Requires = m.Provides, m.AddonRequires
		}
		out = append(out, ia)
	}
	return out, nil
}

// applyRequires turns a resolver error into the mode's outcome: nil for off,
// a logged warning for warn, the error itself for enforce.
func (i *Installer) applyRequires(op string, orgID uuid.UUID, addonKey string, err error) error {
	if err == nil {
		return nil
	}
	switch i.requiresMode() {
	case RequiresOff:
		return nil
	case RequiresEnforce:
		return err
	}
	slog.Warn("installer.requires_unmet",
		"op", op, "org_id", orgID.String(), "addon_key", addonKey,
		"mode", string(RequiresWarn), "err", err.Error())
	return nil
}

// lookupFailed handles a failure to read the installed set: enforce fails
// closed, warn/off log and leave the operation exactly as it was before the
// requires gate existed.
func (i *Installer) lookupFailed(op string, orgID uuid.UUID, addonKey string, err error) error {
	if i.requiresMode() == RequiresEnforce {
		return fmt.Errorf("installer: resolve requires: %w", err)
	}
	slog.Warn("installer.requires_lookup_failed", "op", op, "org_id", orgID.String(), "addon_key", addonKey, "err", err.Error())
	return nil
}

// checkRequires runs the install/upgrade gate. Install doubles as the
// marketplace "Actualizar" path, so both use the upgrade matrix: the
// candidate's own requires AND the ranges of installed dependents of it.
func (i *Installer) checkRequires(op string, orgID uuid.UUID, m manifest.Manifest) error {
	if i.requiresMode() == RequiresOff {
		return nil
	}
	installed, err := i.installedAddons(orgID)
	if err != nil {
		return i.lookupFailed(op, orgID, m.Key, err)
	}
	return i.applyRequires(op, orgID, m.Key, CheckUpgradable(installed, m))
}

// UninstallOptions configures UninstallWithOptions.
type UninstallOptions struct {
	// DropSchema is the dropSchema argument of Uninstall.
	DropSchema bool
	// Cascade removes the installed dependents first, in reverse dependency
	// order, then the addon. NEVER the default: it is destructive, so the
	// caller must ask for it explicitly. It acts regardless of
	// EnforceAddonRequires (an explicit request is not a compatibility mode).
	Cascade bool
}

// UninstallWithOptions is Uninstall plus the explicit cascade: with Cascade
// the installed dependents are removed first, in reverse dependency order,
// then the addon; without it, it behaves exactly like Uninstall.
func (i *Installer) UninstallWithOptions(orgID uuid.UUID, addonKey string, opts UninstallOptions) error {
	if !opts.Cascade {
		return i.Uninstall(orgID, addonKey, opts.DropSchema)
	}
	installed, err := i.installedAddons(orgID)
	if err != nil {
		return fmt.Errorf("installer: resolve dependents: %w", err)
	}
	for _, k := range UninstallOrder(installed, addonKey) {
		if err := i.uninstallAddon(orgID, k, opts.DropSchema); err != nil {
			return fmt.Errorf("cascade uninstall of %q: %w", k, err)
		}
	}
	return nil
}

// checkUninstallRequires refuses (enforce) or logs (warn) an uninstall that
// would orphan installed dependents.
func (i *Installer) checkUninstallRequires(orgID uuid.UUID, addonKey string) error {
	if i.requiresMode() == RequiresOff {
		return nil
	}
	installed, err := i.installedAddons(orgID)
	if err != nil {
		return i.lookupFailed("uninstall", orgID, addonKey, err)
	}
	return i.applyRequires("uninstall", orgID, addonKey, CheckUninstallable(installed, addonKey))
}
