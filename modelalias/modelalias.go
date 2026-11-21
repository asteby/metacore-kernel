// Package modelalias resolves the several slugs a model can be reached by
// (its key, its table, and the extra names it declares in models[].aliases) to
// its canonical key.
//
// A migrated front end rarely uses the backend's canonical names: it asks for
// /data/client, /metadata/table/order-items or /data/global-payments while the
// model is BranchCashDesk or Customer. Declaring aliases in the manifest lets
// the kernel answer those requests without a per-host rewrite layer.
//
// Matching ignores case and separators, and tolerates singular/plural:
// "branch-cash-desk", "branch_cash_desks" and "BranchCashDesk" are the same
// name. Resolution is conservative: an exact (normalized) match wins; a
// singular/plural variant is accepted only when it points at exactly one
// model, so a guess is never silent.
//
// Index is the host-side registry. Add rejects a name that already belongs to
// another model, so installing a manifest whose alias collides with another
// installed model fails at install time instead of shadowing it at request
// time.
package modelalias

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"unicode"
)

// Normalize reduces a slug to its comparison form: lower case with every
// non-alphanumeric separator removed.
func Normalize(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

// ConflictError reports a name already owned by another model.
type ConflictError struct {
	Name     string // the offending name as declared
	Owner    string // canonical key that already owns it
	Claimant string // canonical key that tried to claim it
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("modelalias: %q of model %q collides with model %q", e.Name, e.Claimant, e.Owner)
}

// Index maps normalized names to canonical model keys. It is safe for
// concurrent use; the zero value is not usable, call New.
type Index struct {
	mu    sync.RWMutex
	names map[string]string   // normalized name -> canonical key
	byKey map[string][]string // canonical key -> normalized names it owns
}

// New returns an empty Index.
func New() *Index {
	return &Index{names: map[string]string{}, byKey: map[string][]string{}}
}

// Add registers a model: its canonical key, table and aliases all resolve to
// key. Re-adding the same key replaces its previous names (an upgrade). If any
// name is already owned by a DIFFERENT model, nothing is registered and a
// *ConflictError is returned.
//
// The key and the table of a model are claimed too, so an alias of one model
// can never shadow another model's key or table.
func (x *Index) Add(key, table string, aliases []string) error {
	if Normalize(key) == "" {
		return fmt.Errorf("modelalias: empty model key")
	}
	want := map[string]string{} // normalized -> declared form
	for _, n := range append([]string{key, table}, aliases...) {
		if nn := Normalize(n); nn != "" {
			if _, ok := want[nn]; !ok {
				want[nn] = n
			}
		}
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	for nn, declared := range want {
		if owner, ok := x.names[nn]; ok && owner != key {
			return &ConflictError{Name: declared, Owner: owner, Claimant: key}
		}
	}
	x.removeLocked(key)
	owned := make([]string, 0, len(want))
	for nn := range want {
		x.names[nn] = key
		owned = append(owned, nn)
	}
	sort.Strings(owned)
	x.byKey[key] = owned
	return nil
}

// Remove forgets a model (uninstall).
func (x *Index) Remove(key string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.removeLocked(key)
}

func (x *Index) removeLocked(key string) {
	for _, nn := range x.byKey[key] {
		if x.names[nn] == key {
			delete(x.names, nn)
		}
	}
	delete(x.byKey, key)
}

// Resolve returns the canonical key for slug. ok is false when no model owns
// the name, or when only singular/plural variants match and they are ambiguous.
func (x *Index) Resolve(slug string) (string, bool) {
	n := Normalize(slug)
	if n == "" || x == nil {
		return "", false
	}
	x.mu.RLock()
	defer x.mu.RUnlock()
	if key, ok := x.names[n]; ok {
		return key, true
	}
	var found string
	for _, v := range variants(n) {
		if key, ok := x.names[v]; ok {
			if found != "" && found != key {
				return "", false
			}
			found = key
		}
	}
	return found, found != ""
}

// variants lists the singular/plural spellings of a normalized name.
func variants(n string) []string {
	out := []string{n + "s", n + "es"}
	switch {
	case strings.HasSuffix(n, "ies") && len(n) > 3:
		out = append(out, n[:len(n)-3]+"y")
	case strings.HasSuffix(n, "es") && len(n) > 2:
		out = append(out, n[:len(n)-2], n[:len(n)-1])
	case strings.HasSuffix(n, "s") && len(n) > 1:
		out = append(out, n[:len(n)-1])
	}
	if strings.HasSuffix(n, "y") && len(n) > 1 {
		out = append(out, n[:len(n)-1]+"ies")
	}
	return out
}

// Resolver is the narrow view the kernel services consume.
type Resolver interface {
	Resolve(slug string) (key string, ok bool)
}

// Canonical returns slug unchanged when known(slug) is true (the canonical
// lookup always wins, so aliases cost nothing to models that do not use them);
// otherwise the resolved key; otherwise slug unchanged so the caller reports
// its usual "model not found". A nil resolver is a no-op.
func Canonical(slug string, known func(string) bool, r Resolver) string {
	if r == nil || slug == "" || (known != nil && known(slug)) {
		return slug
	}
	if key, ok := r.Resolve(slug); ok {
		return key
	}
	return slug
}
