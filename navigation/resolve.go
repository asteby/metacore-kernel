package navigation

import (
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// Resolver v2 (RFC SDK-0001 "owner + plugs", part B).
//
// Resolve is a PURE function from the addons installed and enabled for an
// organization (their nav anchors and navigation groups), the user's grants
// and the org's installation state to a sidebar tree:
//
//	section (closed list, fixed order) -> entries (anchors and own groups) -> items
//
// The same input yields the same tree, byte for byte, whatever the order the
// addons are given in. It never fails: inconsistent input (two definitive
// owners of one anchor, an `as` the anchor does not accept…) is resolved
// deterministically and reported in Tree.Warnings; CheckInstalled reports the
// same findings as errors for an install-time gate.
//
// Build / BuildFor (the v1 merger) are unchanged for manifests that do not use
// anchors; hosts move from one to the other behind their own switch.

// Addon is one installed addon as the resolver sees it.
type Addon struct {
	Key string
	// FirstParty ranks the addon above third parties in the dedupe (A5). The
	// host decides it (publisher, signature); the kernel does not.
	FirstParty bool
	Anchors    []v3.NavAnchor
	Groups     []manifest.NavGroup
	// Role, Requires, RequiresContracts and OwnedContracts feed the successor check of
	// CheckInstalled (R-D'): a provider/extension only plugs into anchors of an
	// addon it requires (by key, or by a contract the owner declares). All optional;
	// an empty Role skips the check.
	Role              string
	Requires          []string
	RequiresContracts []string
	OwnedContracts    []string
}

// AddonFromManifest builds the resolver view of a host manifest.
func AddonFromManifest(m manifest.Manifest, firstParty bool) Addon {
	a := Addon{Key: m.Key, FirstParty: firstParty, Anchors: m.NavAnchors, Groups: m.Navigation, Role: m.Role}
	for _, r := range m.AddonRequires {
		a.Requires = append(a.Requires, r.Key)
	}
	for _, r := range m.ContractRequires {
		a.RequiresContracts = append(a.RequiresContracts, r.Contract)
	}
	for _, c := range m.Contracts {
		a.OwnedContracts = append(a.OwnedContracts, c.Key)
	}
	return a
}

// Input is everything Resolve reads.
type Input struct {
	Addons []Addon
	// Installed evaluates contribution conditions (nil keeps every entry).
	Installed InstalledFn
	// Allowed answers whether the user holds a permission (nil = all). RBAC
	// is per leaf: an attached item needs its own permission AND a visible
	// anchor; the anchor's grant is not inherited.
	Allowed func(permission string) bool
}

// Tree is the resolved navigation.
type Tree struct {
	// Sections are the sidebar sections that have something to show, in the
	// fixed order of v3.NavSections.
	Sections []Section `json:"sections"`
	// Settings is the settings menu (section "settings"), nil when empty.
	Settings *Section `json:"settings,omitempty"`
	// Dedupe lists the items dropped because another item resolved to the
	// same resource (the host emits nav.dedupe{winner, loser}).
	Dedupe []DedupeEvent `json:"dedupe,omitempty"`
	// Warnings are inconsistencies resolved deterministically.
	Warnings []string `json:"warnings,omitempty"`
}

// Section is one sidebar section.
type Section struct {
	Key string `json:"key"`
	// Title is the i18n key "nav.section.<key>" (the vertical localizes it).
	Title   string  `json:"title"`
	Entries []Entry `json:"entries"`
}

// Entry kinds.
const (
	EntryAnchor = "anchor"
	EntryGroup  = "group"
)

// Entry is an anchor or an addon's own group inside a section.
type Entry struct {
	Kind string `json:"kind"`
	// Key is the anchor key or the group key (empty for a legacy group).
	Key         string `json:"key,omitempty"`
	Owner       string `json:"owner"`
	Title       string `json:"title"`
	Icon        string `json:"icon,omitempty"`
	Order       int    `json:"order"`
	Model       string `json:"model,omitempty"`
	Page        string `json:"page,omitempty"`
	URL         string `json:"url,omitempty"`
	Permission  string `json:"permission,omitempty"`
	Provisional bool   `json:"provisional,omitempty"`
	// Items are the plug-ins of an anchor (As set) or the entries of a group.
	Items []ResolvedItem `json:"items"`
}

// ResolvedItem is one item of an entry.
type ResolvedItem struct {
	Item
	// ID is "<addon>.<item key>", or "<addon>#<group>.<index>" without key.
	ID string `json:"id"`
	// As is set on an anchor's plug-ins (tab|view|action|panel|filter|mode|setting).
	As    string `json:"as,omitempty"`
	Order int    `json:"order"`
	// AnchorMissing names the anchor an attached item could not reach; the
	// item is shown as an entry of its group by fallback "section".
	AnchorMissing string `json:"anchor_missing,omitempty"`
}

// DedupeEvent records one item dropped by the dedupe.
type DedupeEvent struct {
	Winner   string `json:"winner"`
	Loser    string `json:"loser"`
	Resource string `json:"resource"`
	Scope    string `json:"scope"`
}

type anchorRec struct {
	a          v3.NavAnchor
	owner      string
	firstParty bool
}

type itemRec struct {
	ri         ResolvedItem
	owner      string
	firstParty bool
	resource   string
	leaf       bool
}

type entryRec struct {
	e       Entry
	sortKey string
	section string
	scope   string
	items   []*itemRec
}

// Resolve builds the navigation tree. See the package comment above.
func Resolve(in Input) Tree {
	var t Tree
	warn := func(format string, a ...any) { t.Warnings = append(t.Warnings, fmt.Sprintf(format, a...)) }
	allowed := func(p string) bool { return p == "" || in.Allowed == nil || in.Allowed(p) }

	addons := append([]Addon(nil), in.Addons...)
	sort.SliceStable(addons, func(i, j int) bool { return addons[i].Key < addons[j].Key })
	firstParty := map[string]bool{}
	uniq := addons[:0]
	for _, a := range addons {
		if _, dup := firstParty[a.Key]; dup {
			warn("addon %q is listed twice; only its first entry is used", a.Key)
			continue
		}
		firstParty[a.Key] = a.FirstParty
		uniq = append(uniq, a)
	}
	addons = uniq

	anchors := chooseAnchors(addons, warn)

	entries := map[string]*entryRec{}
	var order []string
	entryFor := func(id string, mk func() *entryRec) *entryRec {
		if e, ok := entries[id]; ok {
			return e
		}
		e := mk()
		entries[id] = e
		order = append(order, id)
		return e
	}
	visible := map[string]*entryRec{}
	for _, k := range sortedKeys(anchors) {
		r := anchors[k]
		if !allowed(r.a.Permission) {
			continue
		}
		sec := r.a.Section
		visible[k] = entryFor("a:"+k, func() *entryRec {
			return &entryRec{section: sec, sortKey: k, scope: "anchor:" + k, e: Entry{
				Kind: EntryAnchor, Key: k, Owner: r.owner, Title: r.a.Title, Icon: r.a.Icon,
				Order: v3.EffectiveNavOrder(r.a.Order), Model: r.a.Model, Page: r.a.Page, URL: r.a.URL,
				Permission: r.a.Permission, Provisional: r.a.Provisional,
			}}
		})
	}

	for _, ad := range addons {
		for gi, g := range ad.Groups {
			if !g.Condition.Satisfied(in.Installed) {
				continue
			}
			sec := g.Section
			if sec != "" && !v3.ValidNavSection(sec) {
				warn("addon %q navigation[%d]: section %q is not in the registry; using its target", ad.Key, gi, sec)
				sec = ""
			}
			if sec == "" {
				var known bool
				sec, known = v3.NavSectionForTarget(g.Target)
				if !known {
					warn("[%s] addon %q navigation[%d]: target %q maps to no section; placed in %q", v3.RuleNavTargetUnknown, ad.Key, gi, g.Target, sec)
				}
			}
			groupSort := g.Key
			if groupSort == "" {
				// NUL sorts before any key character, so legacy groups keep the
				// v1 order (addon key, then declaration order).
				groupSort = fmt.Sprintf("%s\x00%04d", ad.Key, gi)
			}
			groupIn := func(section string) *entryRec {
				return entryFor("g:"+section+"\x00"+groupSort, func() *entryRec {
					return &entryRec{section: section, sortKey: groupSort, scope: "group:" + section + "/" + groupSort, e: Entry{
						Kind: EntryGroup, Key: g.Key, Owner: ad.Key, Title: g.Title, Icon: g.Icon, Order: v3.EffectiveNavOrder(g.Order),
					}}
				})
			}
			for ii, it := range filterByCondition(g.Items, in.Installed) {
				id := ad.Key + "." + it.Key
				if it.Key == "" {
					id = fmt.Sprintf("%s#%04d.%04d", ad.Key, gi, ii)
				}
				rec := &itemRec{owner: ad.Key, firstParty: ad.FirstParty, resource: resourceKey(it), leaf: len(it.Items) == 0,
					ri: ResolvedItem{Item: toItem(it, ad.Key, in.Allowed), ID: id, Order: v3.EffectiveNavOrder(it.Order)}}
				if it.AttachTo == "" {
					if allowed(it.Permission) {
						e := groupIn(sec)
						e.items = append(e.items, rec)
					}
					continue
				}
				if !v3.ValidNavAs(it.As) {
					warn("item %s: attach_to %q without a valid as (%q); dropped", id, it.AttachTo, it.As)
					continue
				}
				if e, ok := visible[it.AttachTo]; ok {
					if !anchors[it.AttachTo].a.AcceptsAs(it.As) {
						warn("item %s: anchor %q does not accept as %q; dropped", id, it.AttachTo, it.As)
						continue
					}
					if allowed(it.Permission) {
						rec.ri.As = it.As
						e.items = append(e.items, rec)
					}
					continue
				}
				fb := v3.EffectiveNavFallback(it.As, it.Fallback)
				if fb != v3.NavFallbackSection || !v3.NavFallbackAllowed(it.As) || !allowed(it.Permission) {
					continue
				}
				rec.ri.AnchorMissing = it.AttachTo
				e := groupIn(v3.NavAnchorSection(it.AttachTo))
				e.items = append(e.items, rec)
			}
		}
	}

	all := make([]*entryRec, 0, len(order))
	for _, id := range order {
		all = append(all, entries[id])
	}
	t.Dedupe = dedupe(all)

	bySection := map[string][]*entryRec{}
	for _, er := range all {
		sortItems(er)
		if er.e.Kind == EntryGroup && len(er.items) == 0 {
			continue // ADR 0003 rule 5: no empty groups (incl. RFC §4.4)
		}
		er.e.Items = make([]ResolvedItem, 0, len(er.items))
		for _, r := range er.items {
			er.e.Items = append(er.e.Items, r.ri)
		}
		bySection[er.section] = append(bySection[er.section], er)
	}
	for _, sec := range append(append([]string(nil), v3.NavSections...), v3.NavSectionSettings) {
		rs := bySection[sec]
		if len(rs) == 0 {
			continue
		}
		sort.SliceStable(rs, func(i, j int) bool {
			if rs[i].e.Order != rs[j].e.Order {
				return rs[i].e.Order < rs[j].e.Order
			}
			return rs[i].sortKey < rs[j].sortKey
		})
		s := Section{Key: sec, Title: "nav.section." + sec, Entries: make([]Entry, len(rs))}
		for i, r := range rs {
			s.Entries[i] = r.e
		}
		if sec == v3.NavSectionSettings {
			t.Settings = &s
			continue
		}
		t.Sections = append(t.Sections, s)
	}
	if t.Sections == nil {
		t.Sections = []Section{}
	}
	return t
}

// chooseAnchors keeps one owner per anchor key: the definitive one over
// provisional ones (RFC §10); between equals, first-party then the lower addon
// key, with a warning (CheckInstalled turns two definitive owners into an
// install error).
func chooseAnchors(addons []Addon, warn func(string, ...any)) map[string]anchorRec {
	cands := map[string][]anchorRec{}
	for _, ad := range addons {
		seen := map[string]bool{}
		for _, a := range ad.Anchors {
			if !v3.ValidNavAnchorKey(a.Key) || v3.NavAnchorSection(a.Key) != a.Section {
				warn("addon %q: nav anchor %q is malformed (key/section); ignored", ad.Key, a.Key)
				continue
			}
			if seen[a.Key] {
				continue
			}
			seen[a.Key] = true
			cands[a.Key] = append(cands[a.Key], anchorRec{a: a, owner: ad.Key, firstParty: ad.FirstParty})
		}
	}
	out := map[string]anchorRec{}
	for _, k := range sortedKeys(cands) {
		cs := cands[k]
		var def, prov []anchorRec
		for _, c := range cs {
			if c.a.Provisional {
				prov = append(prov, c)
			} else {
				def = append(def, c)
			}
		}
		pool := def
		if len(pool) == 0 {
			pool = prov
		}
		sort.SliceStable(pool, func(i, j int) bool {
			if pool[i].firstParty != pool[j].firstParty {
				return pool[i].firstParty
			}
			return pool[i].owner < pool[j].owner
		})
		if len(pool) > 1 {
			warn("nav anchor %q has %d owners (%s); %q is used", k, len(pool), ownersOf(pool), pool[0].owner)
		}
		if len(def) > 0 && len(prov) > 0 {
			warn("nav anchor %q: definitive owner %q replaces provisional %s", k, pool[0].owner, ownersOf(prov))
		}
		out[k] = pool[0]
	}
	return out
}

func ownersOf(rs []anchorRec) string {
	s := make([]string, len(rs))
	for i, r := range rs {
		s[i] = r.owner
	}
	return strings.Join(s, ", ")
}

// dedupe drops items that resolve to the same resource (RFC §5.2): plug-ins
// of one anchor with the same as and resource, items of one group with the
// same resource, and own group leaves that repeat a visible anchor's own
// resource in the same section (the anchor wins). Ranking (A5): owner of the
// anchor > first-party > lower order > lower id. Parents with children do not
// take part.
func dedupe(all []*entryRec) []DedupeEvent {
	var events []DedupeEvent
	anchorRes := map[string]string{} // section+resource -> anchor key
	for _, er := range all {
		if er.e.Kind == EntryAnchor {
			if r := resourceOf(er.e.Model, er.e.Page, er.e.URL, "", nil); r != "" {
				anchorRes[er.section+"\x00"+r] = er.e.Key
			}
		}
	}
	for _, er := range all {
		anchorOwner := ""
		if er.e.Kind == EntryAnchor {
			anchorOwner = er.e.Owner
		}
		best := map[string]*itemRec{}
		kept := er.items[:0]
		for _, r := range er.items {
			if !r.leaf || r.resource == "" {
				kept = append(kept, r)
				continue
			}
			if er.e.Kind == EntryGroup && r.ri.AnchorMissing == "" {
				if ak, ok := anchorRes[er.section+"\x00"+r.resource]; ok {
					events = append(events, DedupeEvent{Winner: ak, Loser: r.ri.ID, Resource: r.resource, Scope: "anchor:" + ak})
					continue
				}
			}
			k := r.ri.As + "\x00" + r.resource
			prev, ok := best[k]
			if !ok {
				best[k] = r
				kept = append(kept, r)
				continue
			}
			win, lose := prev, r
			if beats(r, prev, anchorOwner) {
				win, lose = r, prev
				for i := range kept {
					if kept[i] == prev {
						kept[i] = r
					}
				}
				best[k] = r
			}
			events = append(events, DedupeEvent{Winner: win.ri.ID, Loser: lose.ri.ID, Resource: r.resource, Scope: er.scope})
		}
		er.items = kept
	}
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].Scope != events[j].Scope {
			return events[i].Scope < events[j].Scope
		}
		return events[i].Loser < events[j].Loser
	})
	return events
}

func beats(a, b *itemRec, anchorOwner string) bool {
	ao, bo := a.owner == anchorOwner && anchorOwner != "", b.owner == anchorOwner && anchorOwner != ""
	if ao != bo {
		return ao
	}
	if a.firstParty != b.firstParty {
		return a.firstParty
	}
	if a.ri.Order != b.ri.Order {
		return a.ri.Order < b.ri.Order
	}
	return a.ri.ID < b.ri.ID
}

// sortItems orders an entry's items: order asc, the anchor owner's items
// first on equal order (RFC §5.1.4), then id.
func sortItems(er *entryRec) {
	owner := ""
	if er.e.Kind == EntryAnchor {
		owner = er.e.Owner
	}
	sort.SliceStable(er.items, func(i, j int) bool {
		a, b := er.items[i], er.items[j]
		if a.ri.Order != b.ri.Order {
			return a.ri.Order < b.ri.Order
		}
		if ao, bo := owner != "" && a.owner == owner, owner != "" && b.owner == owner; ao != bo {
			return ao
		}
		return a.ri.ID < b.ri.ID
	})
}

func toItem(it manifest.NavItem, addonKey string, allowedFn func(string) bool) Item {
	out := toItems([]manifest.NavItem{it}, addonKey)[0]
	if allowedFn != nil {
		out.Items = filterAllowed(out.Items, allowedFn)
	}
	return out
}

func filterAllowed(items []Item, allowed func(string) bool) []Item {
	if len(items) == 0 {
		return items
	}
	out := make([]Item, 0, len(items))
	for _, it := range items {
		if it.Permission != "" && !allowed(it.Permission) {
			continue
		}
		it.Items = filterAllowed(it.Items, allowed)
		out = append(out, it)
	}
	return out
}

// resourceKey is the resolved resource of a nav item: page, or model +
// view_type + canonical filter, or normalized url.
func resourceKey(it manifest.NavItem) string {
	return resourceOf(it.Model, it.Page, it.URL, it.ViewType, it.Filter)
}

func resourceOf(model, page, rawURL, viewType string, filter map[string]string) string {
	switch {
	case page != "":
		return "page:" + page
	case model != "":
		vt := viewType
		if vt == "table" {
			vt = ""
		}
		keys := make([]string, 0, len(filter))
		for k := range filter {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var f []string
		for _, k := range keys {
			f = append(f, url.QueryEscape(k)+"="+url.QueryEscape(filter[k]))
		}
		return "model:" + model + "?view=" + vt + "&filter=" + strings.Join(f, "&")
	case rawURL != "":
		return "url:" + NormalizeURL(rawURL)
	}
	return ""
}

// NormalizeURL drops a trailing slash and sorts the query, so URLs that only
// differ in those count as the same resource (RFC §5.2).
func NormalizeURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return strings.TrimSpace(raw)
	}
	if len(u.Path) > 1 {
		u.Path = strings.TrimRight(u.Path, "/")
	}
	u.RawQuery = u.Query().Encode()
	u.Fragment = ""
	return u.String()
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// CheckInstalled is the install-time gate over the addons of one org (the
// installed ones plus the candidate): errors for two definitive owners of one
// anchor and for an item whose `as` a PRESENT anchor does not accept;
// warnings for a provisional anchor replaced by a definitive one. An
// attach_to to an anchor nobody publishes is fine (the fallback applies).
func CheckInstalled(addons []Addon) (errs, warns []string) {
	sorted := append([]Addon(nil), addons...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Key < sorted[j].Key })
	byKey := map[string][]anchorRec{}
	for _, ad := range sorted {
		for _, a := range ad.Anchors {
			byKey[a.Key] = append(byKey[a.Key], anchorRec{a: a, owner: ad.Key, firstParty: ad.FirstParty})
		}
	}
	for _, k := range sortedKeys(byKey) {
		var def, prov []anchorRec
		for _, r := range byKey[k] {
			if r.a.Provisional {
				prov = append(prov, r)
			} else {
				def = append(def, r)
			}
		}
		if len(def) > 1 {
			errs = append(errs, fmt.Sprintf("nav anchor %q is published as definitive by %s; only one owner may publish it", k, ownersOf(def)))
		}
		if len(def) == 1 && len(prov) > 0 {
			warns = append(warns, fmt.Sprintf("nav anchor %q: definitive owner %q replaces provisional %s", k, def[0].owner, ownersOf(prov)))
		}
		if len(def) == 0 && len(prov) > 1 {
			warns = append(warns, fmt.Sprintf("nav anchor %q has several provisional owners (%s)", k, ownersOf(prov)))
		}
	}
	chosen := chooseAnchors(sorted, func(string, ...any) {})
	for _, ad := range sorted {
		for gi, g := range ad.Groups {
			for ii, it := range g.Items {
				if it.AttachTo == "" || it.As == "" {
					continue
				}
				if a, ok := chosen[it.AttachTo]; ok && (ad.Role == v3.RoleProvider || ad.Role == v3.RoleExtension) && a.owner != ad.Key && !mayPlugInto(ad, a.owner, sorted) {
					errs = append(errs, fmt.Sprintf("addon %q navigation[%d].items[%d]: a %s only plugs into anchors of an addon or contract it requires; anchor %q belongs to %q", ad.Key, gi, ii, ad.Role, it.AttachTo, a.owner))
				}
				if a, ok := chosen[it.AttachTo]; ok && !a.a.AcceptsAs(it.As) {
					errs = append(errs, fmt.Sprintf("addon %q navigation[%d].items[%d]: anchor %q (owner %q) does not accept as %q (accepts %s)",
						ad.Key, gi, ii, it.AttachTo, a.owner, it.As, strings.Join(a.a.Accepts, "|")))
				}
			}
		}
	}
	return errs, warns
}

// mayPlugInto reports whether a successor may attach to an anchor owned by
// owner: the owner is one of its required addons, or declares a contract the
// successor requires.
func mayPlugInto(ad Addon, owner string, all []Addon) bool {
	for _, k := range ad.Requires {
		if k == owner {
			return true
		}
	}
	for _, o := range all {
		if o.Key != owner {
			continue
		}
		for _, oc := range o.OwnedContracts {
			for _, rc := range ad.RequiresContracts {
				if oc == rc {
					return true
				}
			}
		}
	}
	return false
}
