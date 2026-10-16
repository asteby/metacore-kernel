package v3

import (
	"fmt"
	"regexp"
	"strings"
)

// apiRouteMethods is the closed set of HTTP methods an APIRoute may declare.
// HEAD is served by GET routes; OPTIONS (CORS) belongs to the host.
var apiRouteMethods = map[string]struct{}{
	"GET": {}, "POST": {}, "PUT": {}, "PATCH": {}, "DELETE": {},
}

// apiRouteAuthKinds is the closed set of APIRoute.Auth values.
var apiRouteAuthKinds = map[string]struct{}{
	"none": {}, "token": {}, "customer": {}, "staff": {},
}

// apiRouteDoPrefixes is the set of dispatch prefixes an APIRoute.Do may use.
// "webhook:" is excluded: an outbound webhook has no response to relay.
var apiRouteDoPrefixes = map[string]struct{}{
	"wasm": {}, "compiled": {},
}

var (
	apiRouteKeyRe  = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	apiScopeRe     = regexp.MustCompile(`^[a-z][a-z0-9_]*:[a-z][a-z0-9_]*$`)
	apiStaticSegRe = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)
	apiParamNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
)

// apiRouteMaxPathLn bounds an APIRoute path pattern.
const apiRouteMaxPathLn = 512

// APIPathSegmentKind classifies one segment of an APIRoute path pattern.
type APIPathSegmentKind int

const (
	// APISegmentStatic matches the literal Value.
	APISegmentStatic APIPathSegmentKind = iota
	// APISegmentParam matches any single non-empty segment, captured as Value.
	APISegmentParam
	// APISegmentWildcard matches the rest of the path (zero or more
	// segments), captured as Value ("*" when the wildcard is unnamed).
	APISegmentWildcard
)

// APIPathSegment is one parsed segment of an APIRoute path pattern. Value is
// the literal for static segments and the capture name otherwise.
type APIPathSegment struct {
	Kind  APIPathSegmentKind
	Value string
}

// ParseAPIPath parses an APIRoute path pattern into segments. The grammar is
// shared with the runtime/apiroute router so a manifest that validates always
// registers:
//
//   - the pattern starts with "/"; "/" alone is the root (zero segments);
//   - no empty segments and no trailing slash;
//   - static segments use [A-Za-z0-9._~-];
//   - ":name" captures one segment (name is snake_case, unique per path);
//   - "*name" or "*" captures the rest and may only be the last segment.
func ParseAPIPath(p string) ([]APIPathSegment, error) {
	if p == "" {
		return nil, fmt.Errorf("path is empty")
	}
	if len(p) > apiRouteMaxPathLn {
		return nil, fmt.Errorf("path exceeds %d characters", apiRouteMaxPathLn)
	}
	if !strings.HasPrefix(p, "/") {
		return nil, fmt.Errorf("path %q must start with \"/\"", p)
	}
	if p == "/" {
		return nil, nil
	}
	raw := strings.Split(p[1:], "/")
	segs := make([]APIPathSegment, 0, len(raw))
	names := map[string]struct{}{}
	for i, s := range raw {
		switch {
		case s == "":
			return nil, fmt.Errorf("path %q has an empty segment or a trailing slash", p)
		case strings.HasPrefix(s, ":"):
			name := s[1:]
			if !apiParamNameRe.MatchString(name) {
				return nil, fmt.Errorf("path %q: parameter %q must be :snake_case", p, s)
			}
			if _, dup := names[name]; dup {
				return nil, fmt.Errorf("path %q: parameter %q is repeated", p, name)
			}
			names[name] = struct{}{}
			segs = append(segs, APIPathSegment{Kind: APISegmentParam, Value: name})
		case strings.HasPrefix(s, "*"):
			if i != len(raw)-1 {
				return nil, fmt.Errorf("path %q: wildcard %q must be the last segment", p, s)
			}
			name := s[1:]
			if name == "" {
				name = "*"
			} else if !apiParamNameRe.MatchString(name) {
				return nil, fmt.Errorf("path %q: wildcard %q must be * or *snake_case", p, s)
			}
			if _, dup := names[name]; dup {
				return nil, fmt.Errorf("path %q: parameter %q is repeated", p, name)
			}
			names[name] = struct{}{}
			segs = append(segs, APIPathSegment{Kind: APISegmentWildcard, Value: name})
		default:
			if !apiStaticSegRe.MatchString(s) {
				return nil, fmt.Errorf("path %q: segment %q has characters outside [A-Za-z0-9._~-]", p, s)
			}
			segs = append(segs, APIPathSegment{Kind: APISegmentStatic, Value: s})
		}
	}
	return segs, nil
}

// APIPathShape renders the parameter-name-independent shape of a parsed path
// ("/products/:/*"). Two routes with the same method and shape are ambiguous
// whatever their capture names, so the shape is the conflict key.
func APIPathShape(segs []APIPathSegment) string {
	if len(segs) == 0 {
		return "/"
	}
	var b strings.Builder
	for _, s := range segs {
		b.WriteByte('/')
		switch s.Kind {
		case APISegmentParam:
			b.WriteByte(':')
		case APISegmentWildcard:
			b.WriteByte('*')
		default:
			b.WriteString(s.Value)
		}
	}
	return b.String()
}

// ValidateAPIRoute checks one APIRoute in isolation (everything but key
// uniqueness and shape conflicts, which need the whole set). It returns the
// findings as messages relative to the route (e.g. "method \"GET \" is not
// one of ..."). Exported so runtime/apiroute applies the exact same rules at
// registration time.
func ValidateAPIRoute(r APIRoute) []string {
	var errs []string
	if r.Key == "" {
		errs = append(errs, "key is required")
	} else if !apiRouteKeyRe.MatchString(r.Key) {
		errs = append(errs, fmt.Sprintf("key %q must be snake_case", r.Key))
	}
	if _, ok := apiRouteMethods[r.Method]; !ok {
		errs = append(errs, fmt.Sprintf("method %q is not one of GET|POST|PUT|PATCH|DELETE", r.Method))
	}
	if _, err := ParseAPIPath(r.Path); err != nil {
		errs = append(errs, err.Error())
	}
	if _, ok := apiRouteAuthKinds[r.Auth]; !ok {
		errs = append(errs, fmt.Sprintf("auth %q is not one of none|token|customer|staff", r.Auth))
	}
	if r.Auth == "none" && len(r.Scopes) > 0 {
		errs = append(errs, "scopes are not allowed when auth is \"none\"")
	}
	seenScope := map[string]struct{}{}
	for _, s := range r.Scopes {
		if !apiScopeRe.MatchString(s) {
			errs = append(errs, fmt.Sprintf("scope %q must be <area>:<verb> (snake_case)", s))
			continue
		}
		if _, dup := seenScope[s]; dup {
			errs = append(errs, fmt.Sprintf("scope %q is repeated", s))
		}
		seenScope[s] = struct{}{}
	}
	if c := r.Cache; c != nil {
		if r.Method != "GET" {
			errs = append(errs, "cache is only allowed on GET routes")
		}
		if c.MaxAge < 0 || c.SWR < 0 {
			errs = append(errs, "cache.max_age and cache.swr must be >= 0")
		}
		if c.Private && (c.MaxAge > 0 || c.SWR > 0) {
			errs = append(errs, "cache.private excludes max_age and swr")
		}
	}
	prefix, target, found := strings.Cut(r.Do, ":")
	if _, ok := apiRouteDoPrefixes[prefix]; !found || !ok || target == "" {
		errs = append(errs, fmt.Sprintf("do %q must be wasm:<export> | compiled:<fn>", r.Do))
	}
	return errs
}

// validateAPIRoutes enforces the contributions.api[] contract: every route
// valid in isolation (ValidateAPIRoute), keys unique within the addon and no
// two routes sharing a method + path shape (an ambiguity the router would
// refuse at registration).
func validateAPIRoutes(m *Manifest) []string {
	if m.Contributions == nil || len(m.Contributions.API) == 0 {
		return nil
	}
	var errs []string
	seenKey := map[string]struct{}{}
	seenShape := map[string]string{}
	for i, r := range m.Contributions.API {
		id := r.Key
		if id == "" {
			id = fmt.Sprintf("#%d", i)
		}
		where := fmt.Sprintf("contributions.api[%s]", id)
		for _, e := range ValidateAPIRoute(r) {
			errs = append(errs, where+": "+e)
		}
		if r.Key != "" {
			if _, dup := seenKey[r.Key]; dup {
				errs = append(errs, fmt.Sprintf("%s.key %q is duplicated within the addon", where, r.Key))
			}
			seenKey[r.Key] = struct{}{}
		}
		if segs, err := ParseAPIPath(r.Path); err == nil {
			k := r.Method + " " + APIPathShape(segs)
			if other, dup := seenShape[k]; dup {
				errs = append(errs, fmt.Sprintf("%s: %s %s conflicts with route %q", where, r.Method, r.Path, other))
			} else {
				seenShape[k] = id
			}
		}
	}
	return errs
}
