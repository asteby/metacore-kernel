package v3

import (
	"fmt"
	"regexp"
	"strings"
)

// Enable ref kinds accepted in Requirement.Enables.
const (
	EnableKindAction = "action"
	EnableKindNav    = "nav"
	EnableKindWidget = "widget"
)

// enableRefRe is the strict grammar of a Requirement.Enables item. It mirrors
// the pattern in the embedded JSON schema; keep both in sync.
var enableRefRe = regexp.MustCompile(`^(action:[A-Za-z][A-Za-z0-9_]*\.[a-z][a-z0-9_]*|(nav|widget):[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*)$`)

// EnableRef is a parsed Requirement.Enables item. Model and Action are set only
// for Kind "action"; Key is set only for "nav" and "widget".
type EnableRef struct {
	Kind   string
	Model  string
	Action string
	Key    string
}

// ParseEnableRef parses "action:<ModelKey>.<action_key>", "nav:<key>" or
// "widget:<key>". It does not check that the target exists.
func ParseEnableRef(s string) (EnableRef, error) {
	if !enableRefRe.MatchString(s) {
		return EnableRef{}, fmt.Errorf("%q must be action:<ModelKey>.<action_key>, nav:<key> or widget:<key>", s)
	}
	kind, rest, _ := strings.Cut(s, ":")
	if kind != EnableKindAction {
		return EnableRef{Kind: kind, Key: rest}, nil
	}
	model, action, _ := strings.Cut(rest, ".")
	return EnableRef{Kind: kind, Model: model, Action: action}, nil
}

// String renders the ref back to its manifest form.
func (r EnableRef) String() string {
	if r.Kind == EnableKindAction {
		return r.Kind + ":" + r.Model + "." + r.Action
	}
	return r.Kind + ":" + r.Key
}

// OptionalRequirement is an optional peer addon and the capabilities of this
// addon that depend on it (see Requirement.Enables).
type OptionalRequirement struct {
	Key     string
	Version string
	Reason  string
	Enables []string
}

// OptionalRequirements returns the optional peer addons this manifest declares
// under compatibility.requires, in declaration order, with Enables copied.
// Hosts use it to gate the dependent capabilities without re-reading requires.
func (m *Manifest) OptionalRequirements() []OptionalRequirement {
	if m == nil {
		return nil
	}
	var out []OptionalRequirement
	for _, r := range m.Compatibility.Requires {
		if !r.Optional || strings.TrimSpace(r.Key) == "" {
			continue
		}
		out = append(out, OptionalRequirement{
			Key:     r.Key,
			Version: r.Version,
			Reason:  r.Reason,
			Enables: append([]string(nil), r.Enables...),
		})
	}
	return out
}

// validateRequiresEnables checks compatibility.requires[].enables: only on
// optional requires, strict item grammar, no duplicates, and every action ref
// must name a contributions.actions entry of this manifest (by target_model
// and key).
func validateRequiresEnables(m *Manifest) []string {
	var errs []string
	actions := map[string]struct{}{}
	if m.Contributions != nil {
		for _, a := range m.Contributions.Actions {
			actions[a.TargetModel+"."+a.Key] = struct{}{}
		}
	}
	for i, r := range m.Compatibility.Requires {
		if len(r.Enables) == 0 {
			continue
		}
		where := fmt.Sprintf("compatibility.requires[%d].enables", i)
		if !r.Optional {
			errs = append(errs, fmt.Sprintf("%s is only valid with optional: true (a mandatory require %q already blocks the install when missing)", where, r.Key))
		}
		seen := map[string]struct{}{}
		for j, e := range r.Enables {
			ref, err := ParseEnableRef(e)
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s[%d]: %v", where, j, err))
				continue
			}
			if _, dup := seen[e]; dup {
				errs = append(errs, fmt.Sprintf("%s[%d]: %q is listed twice", where, j, e))
			}
			seen[e] = struct{}{}
			if ref.Kind == EnableKindAction {
				if _, ok := actions[ref.Model+"."+ref.Action]; !ok {
					errs = append(errs, fmt.Sprintf("%s[%d]: %q names no contributions.actions entry with target_model %q and key %q", where, j, e, ref.Model, ref.Action))
				}
			}
		}
	}
	return errs
}
