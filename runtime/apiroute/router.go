package apiroute

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"

	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// Registration errors.
var (
	// ErrInvalidRoute — a route fails the contributions.api[] rules.
	ErrInvalidRoute = errors.New("apiroute: invalid route")
	// ErrConflict — a method + path shape is already owned by another addon
	// (or declared twice in the same set).
	ErrConflict = errors.New("apiroute: route conflict")
)

// Segment ranks for precedence: lower wins.
const (
	rankStatic = iota
	rankParam
	rankWildcard
)

type compiledRoute struct {
	route Route
	segs  []v3.APIPathSegment
	key   string // method + " " + shape
}

// Router is the API route table shared by every installed addon. Safe for
// concurrent use. Construct it with NewRouter.
type Router struct {
	mu      sync.RWMutex
	byAddon map[string][]compiledRoute
	owners  map[string]string // method+shape → addon
}

// NewRouter returns an empty router.
func NewRouter() *Router {
	return &Router{byAddon: map[string][]compiledRoute{}, owners: map[string]string{}}
}

// Register replaces the routes of addonKey with routes, atomically: when any
// route is invalid or conflicts — with another route of the set or with a
// method + path shape another addon owns — nothing changes and the error
// (wrapping ErrInvalidRoute and/or ErrConflict) lists every finding. Capture
// names do not matter for conflicts: "/p/:id" and "/p/:slug" are the same
// shape. An empty routes slice is equivalent to Unregister.
func (r *Router) Register(addonKey string, routes []Route) error {
	if addonKey == "" {
		return fmt.Errorf("%w: addon key is empty", ErrInvalidRoute)
	}
	compiled := make([]compiledRoute, 0, len(routes))
	var errs []error
	seenKey := map[string]struct{}{}
	seenShape := map[string]string{}
	for i, rt := range routes {
		rt.Addon = addonKey
		rt.Scopes = slices.Clone(rt.Scopes)
		if rt.Cache != nil {
			c := *rt.Cache
			rt.Cache = &c
		}
		id := rt.Key
		if id == "" {
			id = fmt.Sprintf("#%d", i)
		}
		if msgs := v3.ValidateAPIRoute(rt.toManifest()); len(msgs) > 0 {
			errs = append(errs, fmt.Errorf("%w: %s.%s: %s", ErrInvalidRoute, addonKey, id, strings.Join(msgs, "; ")))
			continue
		}
		if _, dup := seenKey[rt.Key]; dup {
			errs = append(errs, fmt.Errorf("%w: %s.%s: key is duplicated", ErrInvalidRoute, addonKey, id))
			continue
		}
		seenKey[rt.Key] = struct{}{}
		segs, _ := v3.ParseAPIPath(rt.Path) // validated above
		key := rt.Method + " " + v3.APIPathShape(segs)
		if other, dup := seenShape[key]; dup {
			errs = append(errs, fmt.Errorf("%w: %s.%s: %s %s duplicates route %q", ErrConflict, addonKey, id, rt.Method, rt.Path, other))
			continue
		}
		seenShape[key] = rt.Key
		compiled = append(compiled, compiledRoute{route: rt, segs: segs, key: key})
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range compiled {
		if owner, taken := r.owners[c.key]; taken && owner != addonKey {
			errs = append(errs, fmt.Errorf("%w: %s.%s: %s %s is already served by addon %q", ErrConflict, addonKey, c.route.Key, c.route.Method, c.route.Path, owner))
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	r.unregisterLocked(addonKey)
	if len(compiled) == 0 {
		return nil
	}
	r.byAddon[addonKey] = compiled
	for _, c := range compiled {
		r.owners[c.key] = addonKey
	}
	return nil
}

// Unregister drops every route of addonKey. Unknown keys are a no-op.
func (r *Router) Unregister(addonKey string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.unregisterLocked(addonKey)
}

func (r *Router) unregisterLocked(addonKey string) {
	for _, c := range r.byAddon[addonKey] {
		if r.owners[c.key] == addonKey {
			delete(r.owners, c.key)
		}
	}
	delete(r.byAddon, addonKey)
}

// Routes returns a snapshot of every registered route, sorted by addon and
// key.
func (r *Router) Routes() []Route {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []Route
	for _, cs := range r.byAddon {
		for _, c := range cs {
			out = append(out, c.route)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Addon != out[j].Addon {
			return out[i].Addon < out[j].Addon
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// Match resolves method + path to a route and its captured parameters. path
// is the (percent-escaped) request path relative to the gateway mount, e.g.
// "/storefront/products/llanta-205"; a single trailing slash is ignored and
// each segment is unescaped before matching. HEAD matches GET routes.
//
// Among matching routes the most specific wins, segment by segment from the
// left: a static segment beats a :param, which beats the *wildcard; on equal
// prefixes the shorter pattern wins ("/files" over "/files/*rest" for
// "/files"). Conflicting shapes are refused at Register, so the winner is
// unique.
func (r *Router) Match(method, path string) (Route, map[string]string, bool) {
	parts, ok := splitPath(path)
	if !ok {
		return Route{}, nil, false
	}
	method = normalizeMethod(method)
	r.mu.RLock()
	defer r.mu.RUnlock()
	var (
		best       *compiledRoute
		bestRank   []int
		bestParams map[string]string
	)
	for addon := range r.byAddon {
		cs := r.byAddon[addon]
		for i := range cs {
			c := &cs[i]
			if c.route.Method != method {
				continue
			}
			params, rank, ok := matchSegments(c.segs, parts)
			if !ok {
				continue
			}
			if best == nil || compareRank(rank, bestRank) < 0 ||
				(compareRank(rank, bestRank) == 0 && tieBreak(c.route, best.route)) {
				best, bestRank, bestParams = c, rank, params
			}
		}
	}
	if best == nil {
		return Route{}, nil, false
	}
	return best.route, bestParams, true
}

// Allowed returns the sorted methods some route accepts for path (with HEAD
// added when GET is there). Empty means no route matches the path at all.
func (r *Router) Allowed(path string) []string {
	parts, ok := splitPath(path)
	if !ok {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	set := map[string]struct{}{}
	for _, cs := range r.byAddon {
		for i := range cs {
			if _, _, ok := matchSegments(cs[i].segs, parts); ok {
				set[cs[i].route.Method] = struct{}{}
			}
		}
	}
	if _, ok := set["GET"]; ok {
		set["HEAD"] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for m := range set {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

func normalizeMethod(m string) string {
	m = strings.ToUpper(m)
	if m == "HEAD" {
		return "GET"
	}
	return m
}

// splitPath splits and unescapes a request path. "/" yields no parts. A
// segment that fails to unescape makes the path unmatchable.
func splitPath(p string) ([]string, bool) {
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if len(p) > 1 {
		p = strings.TrimSuffix(p, "/")
	}
	if p == "/" {
		return nil, true
	}
	raw := strings.Split(p[1:], "/")
	parts := make([]string, len(raw))
	for i, s := range raw {
		u, err := url.PathUnescape(s)
		if err != nil {
			return nil, false
		}
		parts[i] = u
	}
	return parts, true
}

// matchSegments matches a compiled pattern against path parts, returning the
// captures and the per-segment rank vector used for precedence.
func matchSegments(segs []v3.APIPathSegment, parts []string) (map[string]string, []int, bool) {
	params := map[string]string{}
	rank := make([]int, 0, len(segs))
	for i, s := range segs {
		if s.Kind == v3.APISegmentWildcard {
			params[s.Value] = strings.Join(parts[i:], "/")
			rank = append(rank, rankWildcard)
			return params, rank, true
		}
		if i >= len(parts) {
			return nil, nil, false
		}
		switch s.Kind {
		case v3.APISegmentStatic:
			if parts[i] != s.Value {
				return nil, nil, false
			}
			rank = append(rank, rankStatic)
		case v3.APISegmentParam:
			if parts[i] == "" {
				return nil, nil, false
			}
			params[s.Value] = parts[i]
			rank = append(rank, rankParam)
		}
	}
	if len(segs) != len(parts) {
		return nil, nil, false
	}
	return params, rank, true
}

// compareRank orders rank vectors lexicographically; on a common prefix the
// shorter vector is more specific.
func compareRank(a, b []int) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return 0
}

// tieBreak keeps Match deterministic should two equally-ranked routes ever
// match (not reachable with conflict detection, kept as a guard).
func tieBreak(a, b Route) bool {
	if a.Addon != b.Addon {
		return a.Addon < b.Addon
	}
	return a.Key < b.Key
}
