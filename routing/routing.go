// Package routing resolves declarative routing tables: given a decision domain
// and the attributes of the record being routed, which named handler wins for
// this organization.
//
// It exists so a FAMILY of addons can agree on a decision none of them owns
// alone — "who fulfils this order line?" is answered by inventory when it is
// just stock, by a warehouse addon when allocation is involved, by workshop
// when the line is labour — without any of them importing the others, and
// without the kernel learning what any of those words mean. Each addon
// contributes routes (manifest contributions.routes[]); the host builds a Table
// from the routes of every INSTALLED addon and resolves per record.
//
// The kernel treats Domain and Handler as opaque strings. Precedence is
// mechanical: gate on installation, keep the routes whose Match is satisfied,
// then take the most specific one (highest Priority, then most match keys, then
// the lexicographically smallest addon key so the winner never depends on map
// iteration or install order).
package routing

import (
	"sort"

	"github.com/asteby/metacore-kernel/manifest"
)

// Contribution is one addon's routes paired with its key. The key is both the
// installation gate's subject and the final tie-breaker.
type Contribution struct {
	AddonKey string
	Routes   []manifest.RouteDef
}

// InstalledFn answers "is this addon key installed for the organization whose
// table we are building?" — the resolver for a route's Condition. In the kernel
// it is backed by installer.Installer.InstalledSet. Nil means installation
// state is unknown, in which case conditional routes are kept.
type InstalledFn func(addonKey string) bool

// Table is an org-resolved routing table: the routes of every installed addon,
// indexed by domain and pre-sorted by precedence. Build one per request (or per
// cached metadata projection) — it is a snapshot, not a live view.
type Table struct {
	byDomain map[string][]entry
}

type entry struct {
	addonKey string
	route    manifest.RouteDef
}

// Build folds every contribution into a Table, dropping the routes whose
// Condition the org does not satisfy. Within a domain the entries are sorted by
// descending precedence so Resolve can return the first match it finds.
func Build(contributions []Contribution, installed InstalledFn) *Table {
	t := &Table{byDomain: map[string][]entry{}}
	for _, c := range contributions {
		for _, r := range c.Routes {
			if !r.Condition.Satisfied(installed) {
				continue
			}
			if r.Domain == "" || r.Handler == "" {
				continue
			}
			t.byDomain[r.Domain] = append(t.byDomain[r.Domain], entry{addonKey: c.AddonKey, route: r})
		}
	}
	for domain := range t.byDomain {
		es := t.byDomain[domain]
		sort.SliceStable(es, func(i, j int) bool {
			if es[i].route.Priority != es[j].route.Priority {
				return es[i].route.Priority > es[j].route.Priority
			}
			// A more specific match outranks a broader one at equal priority,
			// so a catch-all never shadows the rule that was written to
			// override it.
			if len(es[i].route.Match) != len(es[j].route.Match) {
				return len(es[i].route.Match) > len(es[j].route.Match)
			}
			return es[i].addonKey < es[j].addonKey
		})
		t.byDomain[domain] = es
	}
	return t
}

// Resolve returns the handler that wins for a record with these attributes in
// the given domain, and the key of the addon that contributed the winning
// route. ok is false when no route matches — the caller decides whether that is
// a no-op or an error; the kernel does not invent a default it does not own.
//
// A route matches when EVERY key of its Match equals the corresponding
// attribute. A route with no Match is the domain's catch-all and matches
// anything (it still loses to any more specific route, per Build's ordering).
func (t *Table) Resolve(domain string, attrs map[string]string) (handler string, addonKey string, ok bool) {
	if t == nil {
		return "", "", false
	}
	for _, e := range t.byDomain[domain] {
		if matches(e.route.Match, attrs) {
			return e.route.Handler, e.addonKey, true
		}
	}
	return "", "", false
}

// Handlers lists every handler reachable in a domain, in precedence order and
// without duplicates. It answers "what can this org do here?" — for diagnostics
// and for hosts that surface the available strategies.
func (t *Table) Handlers(domain string) []string {
	if t == nil {
		return nil
	}
	es := t.byDomain[domain]
	out := make([]string, 0, len(es))
	seen := make(map[string]struct{}, len(es))
	for _, e := range es {
		if _, dup := seen[e.route.Handler]; dup {
			continue
		}
		seen[e.route.Handler] = struct{}{}
		out = append(out, e.route.Handler)
	}
	return out
}

func matches(match, attrs map[string]string) bool {
	for k, want := range match {
		if attrs[k] != want {
			return false
		}
	}
	return true
}
