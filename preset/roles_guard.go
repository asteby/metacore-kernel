package preset

import (
	"errors"
	"fmt"
	"strings"

	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// ErrRoleEscalation is returned by Flatten when an overlay's roles would grant
// more than the public base allows: a wildcard permission, a permission no
// addon of the resolved set declares (FlattenOptions.KnownPermission), a role
// field outside the role contract (system/admin flags), or touching a system
// role (removing it, changing its permissions, or adding a role with a system
// name).
var ErrRoleEscalation = errors.New("preset: overlay role escalates privileges")

// SystemRoleNames are the role identities an overlay can never create, remove
// or re-permission. A base role holding a wildcard permission, or flagged
// system/is_system/admin/is_admin, counts as a system role too.
var SystemRoleNames = map[string]bool{"owner": true, "admin": true, "superadmin": true}

// overlayRoleFields are the only fields an overlay role entry may carry.
var overlayRoleFields = map[string]bool{
	"key": true, "name": true, "slug": true, "label": true, "description": true,
	"color": true, "icon": true, "permissions": true, "_remove": true,
}

var privilegedRoleFlags = []string{"system", "is_system", "admin", "is_admin", "reserved"}

// FlattenOptions tunes Flatten's overlay checks.
type FlattenOptions struct {
	// KnownPermission reports whether perm is declared by an addon of the
	// resolved set (or by the host's platform catalog). With it, an overlay
	// role may only add known permissions. Nil skips that one check; the
	// structural ones (wildcards, privileged fields, system roles) always run.
	KnownPermission func(perm string) bool
}

func roleNames(r map[string]interface{}) []string {
	var out []string
	for _, f := range []string{"key", "name", "slug"} {
		if s, ok := r[f].(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, strings.ToLower(strings.TrimSpace(s)))
		}
	}
	return out
}

func isSystemRole(r map[string]interface{}) bool {
	for _, n := range roleNames(r) {
		if SystemRoleNames[n] {
			return true
		}
	}
	for _, f := range privilegedRoleFlags {
		if b, ok := r[f].(bool); ok && b {
			return true
		}
	}
	perms, _ := r["permissions"].([]interface{})
	for _, p := range perms {
		if s, ok := p.(string); ok && strings.Contains(s, "*") {
			return true
		}
	}
	return false
}

// checkOverlayRoles applies the ErrRoleEscalation rules to the overlay's
// (inlined) roles against the base's.
func checkOverlayRoles(baseRoles, overlayRoles interface{}, known func(string) bool) error {
	ol, ok := overlayRoles.([]interface{})
	if !ok {
		return nil
	}
	st := MergeStrategies["roles"]
	system := map[string]bool{}
	if bl, ok := baseRoles.([]interface{}); ok {
		for _, item := range bl {
			if r, ok := item.(map[string]interface{}); ok && isSystemRole(r) {
				for _, id := range identities(r, st) {
					system[id] = true
				}
			}
		}
	}
	for _, item := range ol {
		r, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		label := strings.Join(roleNames(r), "/")
		for f := range r {
			if !overlayRoleFields[f] {
				return fmt.Errorf("%w: role %q sets %q, which is not an overlay role field", ErrRoleEscalation, label, f)
			}
		}
		targetsSystem := false
		for _, id := range identities(r, st) {
			if system[id] {
				targetsSystem = true
			}
		}
		for _, n := range roleNames(r) {
			if SystemRoleNames[n] {
				targetsSystem = true
			}
		}
		if remove, _ := r["_remove"].(bool); remove {
			if targetsSystem {
				return fmt.Errorf("%w: cannot remove system role %q", ErrRoleEscalation, label)
			}
			continue
		}
		perms, hasPerms := r["permissions"].([]interface{})
		if targetsSystem && hasPerms {
			return fmt.Errorf("%w: cannot change the permissions of system role %q", ErrRoleEscalation, label)
		}
		if targetsSystem && len(identities(r, st)) > 0 && !matchesAny(r, baseRoles, st) {
			return fmt.Errorf("%w: cannot add a role named %q", ErrRoleEscalation, label)
		}
		for _, p := range perms {
			s, ok := p.(string)
			if !ok || strings.HasPrefix(s, "-") {
				continue // revoking is always allowed
			}
			if strings.Contains(s, "*") {
				return fmt.Errorf("%w: role %q grants wildcard %q", ErrRoleEscalation, label, s)
			}
			if known != nil && !known(s) {
				return fmt.Errorf("%w: role %q grants %q, which no addon of the preset declares", ErrRoleEscalation, label, s)
			}
		}
	}
	return nil
}

func matchesAny(r map[string]interface{}, baseRoles interface{}, st Strategy) bool {
	bl, _ := baseRoles.([]interface{})
	ids := identities(r, st)
	for _, item := range bl {
		for _, bid := range identities(item, st) {
			for _, id := range ids {
				if id == bid {
					return true
				}
			}
		}
	}
	return false
}

// DeclaredPermissions builds a FlattenOptions.KnownPermission from the
// manifests of the resolved addon set: every rbac.permissions key and every
// non-wildcard permission an rbac role lists. Hosts with platform-level
// permissions (e.g. "general.approve_discount") OR it with their own catalog.
func DeclaredPermissions(addons ...*v3.Manifest) func(string) bool {
	set := map[string]bool{}
	for _, m := range addons {
		if m == nil || m.RBAC == nil {
			continue
		}
		for _, p := range m.RBAC.Permissions {
			if p.Key != "" {
				set[p.Key] = true
			}
		}
		for _, r := range m.RBAC.Roles {
			for _, p := range r.Permissions {
				if !strings.Contains(p, "*") {
					set[p] = true
				}
			}
		}
	}
	return func(p string) bool { return set[p] }
}
