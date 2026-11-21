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
//     by hosts through Register), and
//   - the contracts DECLARED by installed addons with metadata.role "contract"
//     (manifest contracts[]): RegisterDeclared records "<contract>.<operation>"
//     keys with the contract's semver.
//
// A key outside both is unknown. Release 1 (this one) only WARNS about it
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

var (
	declMu   sync.RWMutex
	declared = map[string]declaredEntry{} // contract key -> entry
)

// RegisterDeclared records the contracts an installed addon owns. It rejects a
// malformed key or version, a duplicate operation, and a contract key already
// owned by ANOTHER addon; re-registering the same owner replaces its previous
// declaration (an upgrade). All-or-nothing: on error nothing is recorded.
func RegisterDeclared(owner string, ds ...Declared) error {
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
	declMu.Lock()
	defer declMu.Unlock()
	seen := map[string]bool{}
	for _, d := range ds {
		if seen[d.Key] {
			return fmt.Errorf("capability: contract %q declared twice by %q", d.Key, owner)
		}
		seen[d.Key] = true
		if prev, ok := declared[d.Key]; ok && prev.owner != owner {
			return fmt.Errorf("capability: contract %q is already owned by addon %q", d.Key, prev.owner)
		}
	}
	// Replace the owner's previous declaration wholesale.
	for k, e := range declared {
		if e.owner == owner {
			delete(declared, k)
		}
	}
	for _, d := range ds {
		declared[d.Key] = declaredEntry{owner: owner, d: d}
	}
	return nil
}

// UnregisterDeclared forgets every contract owned by the addon. A host calls it
// when the last installation of the addon is gone; leaving a stale entry only
// keeps a key known (it never breaks a manifest).
func UnregisterDeclared(owner string) {
	declMu.Lock()
	defer declMu.Unlock()
	for k, e := range declared {
		if e.owner == owner {
			delete(declared, k)
		}
	}
}

// LookupDeclared returns the addon-declared contract with that key and its
// owner addon.
func LookupDeclared(contractKey string) (d Declared, owner string, ok bool) {
	declMu.RLock()
	defer declMu.RUnlock()
	e, ok := declared[contractKey]
	return e.d, e.owner, ok
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

// Known reports whether key is in the closed registry: a kernel contract, a
// declared contract key, or a declared "<contract>.<operation>" key.
func Known(key string) (KeyInfo, bool) {
	if c, ok := Lookup(key); ok {
		return KeyInfo{Key: key, Contract: c.Key, Version: c.Semver(), Source: "kernel"}, true
	}
	declMu.RLock()
	defer declMu.RUnlock()
	if e, ok := declared[key]; ok {
		return KeyInfo{Key: key, Contract: key, Version: e.d.Version, Source: "addon:" + e.owner}, true
	}
	if i := strings.LastIndex(key, "."); i > 0 {
		if e, ok := declared[key[:i]]; ok {
			for _, op := range e.d.Operations {
				if op.Key == key[i+1:] {
					return KeyInfo{Key: key, Contract: e.d.Key, Version: e.d.Version, Source: "addon:" + e.owner}, true
				}
			}
		}
	}
	return KeyInfo{}, false
}

// DeclaredKeys lists every key the declared registry makes known, sorted.
func DeclaredKeys() []string {
	declMu.RLock()
	defer declMu.RUnlock()
	var out []string
	for _, e := range declared {
		out = append(out, e.d.Key)
		for _, op := range e.d.Operations {
			out = append(out, e.d.Key+"."+op.Key)
		}
	}
	sort.Strings(out)
	return out
}

// Satisfies reports whether a contract version satisfies a semver range
// ("^1.2", ">=1.0.0 <2"). An invalid version or range is an error.
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
