package capability

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/Masterminds/semver/v3"
)

// Closed, versioned registry of capability keys (Ola X · X-R2).
//
// Two sources make a key KNOWN:
//   - the kernel's own Contracts (field-checked, registered by this package and
//     by hosts through Register): process-wide and read-only for consumers, and
//   - the contracts DECLARED by installed addons with metadata.role "contract"
//     (manifest contracts[]), held in a Registry INSTANCE.
//
// There is deliberately NO package-level state for declared contracts: what an
// installation declares (and at which version) is a property of that
// organization's installs, so the host owns one Registry per organization (or
// per installation) and passes it to the validators. A process-wide map would
// let one tenant's upgrade, or one private addon, change the answer for
// everybody.
//
// A key outside both sources is unknown. Release 1 only WARNS about it
// (manifest rule capability_key_unknown); a later release turns the warning
// into an error. Declared contracts are not field-checked: their input/output
// live in schema files owned by the addon, so they are kept apart from
// Contracts, which requires typed Input/Output schemas.

// DefaultVersion is the version of a kernel contract that does not set one.
const DefaultVersion = "1.0.0"

// Family is the first segment of the contract key ("commerce" for
// "commerce.payments.session"). It is derived, never declared, so family and
// key cannot disagree.
func (c Contract) Family() string { return familyOf(c.Key) }

func familyOf(key string) string {
	if i := strings.Index(key, "."); i > 0 {
		return key[:i]
	}
	return key
}

// Semver is the contract version, DefaultVersion when unset.
func (c Contract) Semver() string {
	if strings.TrimSpace(c.Version) == "" {
		return DefaultVersion
	}
	return c.Version
}

// DeclaredOperation is one operation of an addon-declared contract.
type DeclaredOperation struct {
	Key  string
	Kind Kind
}

// Declared is the registry view of a contract owned by an addon.
type Declared struct {
	// Key is "<family>.<name>".
	Key string
	// Version is the contract semver.
	Version    string
	Operations []DeclaredOperation
}

// Family is the first segment of the declared contract key.
func (d Declared) Family() string { return familyOf(d.Key) }

type declaredEntry struct {
	owner string
	d     Declared
}

// Registry holds the contracts declared by the addons installed in ONE scope
// (an organization or an installation; the host decides). The zero value is not
// usable: build it with NewRegistry. All methods are safe for concurrent use
// and a nil *Registry behaves as an empty one for reads.
type Registry struct {
	mu       sync.RWMutex
	declared map[string]declaredEntry // contract key -> entry
	// implements maps an owner addon to the contracts it implements.
	implements map[string][]Implemented
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{declared: map[string]declaredEntry{}, implements: map[string][]Implemented{}}
}

// Register records the contracts an installed addon owns. It rejects a
// malformed key or version, a duplicate operation, a collision with a kernel
// contract, and a contract key already owned by ANOTHER addon of this registry;
// re-registering the same owner replaces its previous declaration (an upgrade).
// All-or-nothing: on error nothing is recorded.
func (r *Registry) Register(owner string, ds ...Declared) error {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return fmt.Errorf("capability: declared contracts need an owner addon key")
	}
	for _, d := range ds {
		if !ValidKey(d.Key) {
			return fmt.Errorf("capability: invalid declared contract key %q", d.Key)
		}
		if _, err := semver.NewVersion(d.Version); err != nil {
			return fmt.Errorf("capability: declared contract %q has invalid version %q: %w", d.Key, d.Version, err)
		}
		if _, builtin := Lookup(d.Key); builtin {
			return fmt.Errorf("capability: declared contract %q collides with a kernel contract", d.Key)
		}
		seen := map[string]bool{}
		for _, op := range d.Operations {
			if !errorCodeRe.MatchString(op.Key) {
				return fmt.Errorf("capability: declared contract %q has malformed operation %q", d.Key, op.Key)
			}
			if seen[op.Key] {
				return fmt.Errorf("capability: declared contract %q repeats operation %q", d.Key, op.Key)
			}
			seen[op.Key] = true
			if _, builtin := Lookup(d.Key + "." + op.Key); builtin {
				return fmt.Errorf("capability: declared operation %q collides with a kernel contract", d.Key+"."+op.Key)
			}
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := map[string]bool{}
	for _, d := range ds {
		if seen[d.Key] {
			return fmt.Errorf("capability: contract %q declared twice by %q", d.Key, owner)
		}
		seen[d.Key] = true
		if prev, ok := r.declared[d.Key]; ok && prev.owner != owner {
			return fmt.Errorf("capability: contract %q is already owned by addon %q", d.Key, prev.owner)
		}
	}
	// Replace the owner's previous declaration wholesale.
	for k, e := range r.declared {
		if e.owner == owner {
			delete(r.declared, k)
		}
	}
	for _, d := range ds {
		r.declared[d.Key] = declaredEntry{owner: owner, d: d}
	}
	return nil
}

// Unregister forgets every contract owned by the addon.
func (r *Registry) Unregister(owner string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, e := range r.declared {
		if e.owner == owner {
			delete(r.declared, k)
		}
	}
	delete(r.implements, owner)
}

// Implemented is one contract an addon implements (a manifest's implements[]
// entry): the contract key and the semver range it was written against.
type Implemented struct {
	Contract string
	// Range is the implements[].version range ("^1.0").
	Range string
}

// Implementer is an addon that implements a contract in this registry's scope.
type Implementer struct {
	Owner string
	// Range is the version range the owner declared for the contract.
	Range string
}

// RegisterImplements records the contracts an installed addon implements,
// replacing that owner's previous list (an upgrade). Nothing about the contract
// has to be declared in this registry: the implementer may be installed before
// or without its contract owner. All-or-nothing; rejects a malformed contract
// key, an invalid range or a contract listed twice.
func (r *Registry) RegisterImplements(owner string, impls ...Implemented) error {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return fmt.Errorf("capability: implemented contracts need an owner addon key")
	}
	seen := map[string]bool{}
	for _, im := range impls {
		if !ValidKey(im.Contract) {
			return fmt.Errorf("capability: invalid implemented contract key %q", im.Contract)
		}
		if _, err := semver.NewConstraint(im.Range); err != nil {
			return fmt.Errorf("capability: implemented contract %q has invalid range %q: %w", im.Contract, im.Range, err)
		}
		if seen[im.Contract] {
			return fmt.Errorf("capability: contract %q implemented twice by %q", im.Contract, owner)
		}
		seen[im.Contract] = true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(impls) == 0 {
		delete(r.implements, owner)
		return nil
	}
	r.implements[owner] = append([]Implemented(nil), impls...)
	return nil
}

// Implementers lists, sorted by owner, the addons of this scope that implement
// the contract. It is the "who depends on X" query behind provider selection and
// the uninstall/upgrade guards. A nil registry has none.
func (r *Registry) Implementers(contractKey string) []Implementer {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []Implementer
	for owner, impls := range r.implements {
		for _, im := range impls {
			if im.Contract == contractKey {
				out = append(out, Implementer{Owner: owner, Range: im.Range})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Owner < out[j].Owner })
	return out
}

// Lookup returns the declared contract with that key and its owner addon.
func (r *Registry) Lookup(contractKey string) (d Declared, owner string, ok bool) {
	if r == nil {
		return Declared{}, "", false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.declared[contractKey]
	return e.d, e.owner, ok
}

// InstalledContracts is what one installed addon declares: the input of
// Rehydrate.
type InstalledContracts struct {
	Owner     string
	Contracts []Declared
	// Implements are the contracts the addon implements (feeds Implementers).
	Implements []Implemented
}

// RehydrateReport is the outcome of Rehydrate.
type RehydrateReport struct {
	// Registered lists the owners whose contracts were recorded.
	Registered []string
	// Failed maps an owner to why its contracts were refused.
	Failed map[string]error
}

// Rehydrate rebuilds the registry from what the installations persist. The
// registry is memory: after a restart it is empty, and every declared key would
// be unknown again (and every implements[].version check would be skipped). A
// host calls Rehydrate at boot, per scope, with the installed addons' declared
// contracts. It REPLACES the registry content (a clean slate, so it is the
// same for a fresh instance and for a re-run), applies owners in the order
// given (the first owner of a contract key wins, the rest are reported) and
// never aborts: a failing owner is reported, the others still register.
func (r *Registry) Rehydrate(installed []InstalledContracts) RehydrateReport {
	rep := RehydrateReport{Failed: map[string]error{}}
	r.mu.Lock()
	r.declared = map[string]declaredEntry{}
	r.implements = map[string][]Implemented{}
	r.mu.Unlock()
	for _, in := range installed {
		if len(in.Contracts) == 0 && len(in.Implements) == 0 {
			continue
		}
		var failed error
		if len(in.Contracts) > 0 {
			failed = r.Register(in.Owner, in.Contracts...)
		}
		if failed == nil && len(in.Implements) > 0 {
			failed = r.RegisterImplements(in.Owner, in.Implements...)
		}
		if failed != nil {
			rep.Failed[in.Owner] = failed
			continue
		}
		rep.Registered = append(rep.Registered, in.Owner)
	}
	return rep
}

// KeyInfo describes a known capability key.
type KeyInfo struct {
	// Key is the capability key looked up.
	Key string
	// Contract is the contract it belongs to (== Key for a kernel contract).
	Contract string
	// Version is the contract semver.
	Version string
	// Source is "kernel" or "addon:<owner>".
	Source string
}

// KnownBuiltin reports whether key is one of the kernel's own contracts. It is
// process-wide and read-only for consumers.
func KnownBuiltin(key string) (KeyInfo, bool) {
	if c, ok := Lookup(key); ok {
		return KeyInfo{Key: key, Contract: c.Key, Version: c.Semver(), Source: "kernel"}, true
	}
	return KeyInfo{}, false
}

// Known reports whether key is in the closed registry of THIS scope: a kernel
// contract, a declared contract key, or a declared "<contract>.<operation>".
func (r *Registry) Known(key string) (KeyInfo, bool) {
	if info, ok := KnownBuiltin(key); ok {
		return info, true
	}
	if r == nil {
		return KeyInfo{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if e, ok := r.declared[key]; ok {
		return KeyInfo{Key: key, Contract: key, Version: e.d.Version, Source: "addon:" + e.owner}, true
	}
	if i := strings.LastIndex(key, "."); i > 0 {
		if e, ok := r.declared[key[:i]]; ok {
			for _, op := range e.d.Operations {
				if op.Key == key[i+1:] {
					return KeyInfo{Key: key, Contract: e.d.Key, Version: e.d.Version, Source: "addon:" + e.owner}, true
				}
			}
		}
	}
	return KeyInfo{}, false
}

// KnownCapability implements the manifest validators' CapabilityCatalog.
func (r *Registry) KnownCapability(key string) bool { _, ok := r.Known(key); return ok }

// ContractVersion implements the manifest validators' ContractVersioned: the
// version of a declared contract (a kernel contract answers its Semver).
func (r *Registry) ContractVersion(contractKey string) (string, bool) {
	if c, ok := Lookup(contractKey); ok {
		return c.Semver(), true
	}
	if d, _, ok := r.Lookup(contractKey); ok {
		return d.Version, true
	}
	return "", false
}

// Keys lists every key the registry's declared contracts make known, sorted.
func (r *Registry) Keys() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []string
	for _, e := range r.declared {
		out = append(out, e.d.Key)
		for _, op := range e.d.Operations {
			out = append(out, e.d.Key+"."+op.Key)
		}
	}
	sort.Strings(out)
	return out
}

// Satisfies reports whether a contract version satisfies a semver range
// ("^1.2", ">=1.0.0 <2"). An invalid version or range is an error. Semver
// rules apply: a PRERELEASE ("1.1.0-beta") does not satisfy "^1.0" (a range
// matches a prerelease only when the range itself names one).
func Satisfies(version, constraint string) (bool, error) {
	c, err := semver.NewConstraint(constraint)
	if err != nil {
		return false, fmt.Errorf("invalid semver range %q: %w", constraint, err)
	}
	v, err := semver.NewVersion(version)
	if err != nil {
		return false, fmt.Errorf("invalid semver version %q: %w", version, err)
	}
	return c.Check(v), nil
}
