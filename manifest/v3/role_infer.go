package v3

import (
	"fmt"
	"strings"
)

// Role inference (X-R1, part 1): the types and projection the role rules look
// at, and the warning-only inference of a role that a manifest does not
// declare. The enforcing rules (R-A..R-D) live in role_validate.go.
//
// Compatibility window (one release): a manifest WITHOUT metadata.role keeps
// installing; its role is inferred (InferRole) and reported as the
// warning-only rule role_missing.
const (
	// RuleRoleMissing: metadata.role is not declared but the manifest points to
	// one (the inferred role is named in the message). Warning-only.
	RuleRoleMissing = "role_missing"
)

// RoleFacts is the slice of a manifest the role rules look at.
type RoleFacts struct {
	Key  string
	Role string
	// Preset is true for kind Preset (infers role preset).
	Preset bool
	// Requires are the addon keys of compatibility.requires.
	Requires     []RoleRequire
	Contracts    []Contract
	Implements   []Implements
	ExtendsAddon *ExtendsAddon
	Replaces     []string
	// DBWrites are the targets of the db:write capabilities.
	DBWrites []string
	// OwnModelKeys / OwnTables are the models the addon declares.
	OwnModelKeys []string
	OwnTables    []string
	// ProvidedCapabilities are the keys of provides_capabilities[].
	ProvidedCapabilities []string
	// NavigationGroups counts contributions.navigation entries.
	NavigationGroups int
}

// RoleRequire is one compatibility.requires entry.
type RoleRequire struct {
	Key      string
	Optional bool
}

// RoleFacts projects the typed v3 manifest.
func (m *Manifest) RoleFacts() RoleFacts {
	f := RoleFacts{
		Key:          m.Metadata.Key,
		Role:         m.Metadata.Role,
		Preset:       m.Kind == KindPreset,
		Contracts:    m.Contracts,
		Implements:   m.Implements,
		ExtendsAddon: m.ExtendsAddon,
		Replaces:     m.Replaces,
	}
	for _, r := range m.Compatibility.Requires {
		k := strings.TrimSpace(r.Key)
		opt := r.Optional
		if strings.HasSuffix(k, "?") {
			k, opt = strings.TrimSuffix(k, "?"), true
		}
		f.Requires = append(f.Requires, RoleRequire{Key: k, Optional: opt})
	}
	for _, c := range m.Capabilities {
		if c.Kind == "db:write" {
			f.DBWrites = append(f.DBWrites, c.Target)
		}
	}
	for _, mod := range m.Models {
		f.OwnModelKeys = append(f.OwnModelKeys, mod.Key)
		f.OwnTables = append(f.OwnTables, mod.Table)
	}
	for _, p := range m.ProvidesCapabilities {
		f.ProvidedCapabilities = append(f.ProvidedCapabilities, p.Key)
	}
	if m.Contributions != nil {
		f.NavigationGroups = len(m.Contributions.Navigation)
	}
	return f
}

// ValidRole reports whether r is a metadata.role value.
func ValidRole(r string) bool {
	for _, v := range Roles {
		if v == r {
			return true
		}
	}
	return false
}

// InferRole guesses the role of a manifest that does not declare one. It
// returns "" when nothing in the manifest points to a role.
func InferRole(f RoleFacts) string {
	switch {
	case f.Preset:
		return RolePreset
	case len(f.Contracts) > 0:
		return RoleContract
	case f.ExtendsAddon != nil:
		return RoleExtension
	case len(f.Implements) > 0 || len(f.ProvidedCapabilities) > 0:
		return RoleProvider
	}
	return ""
}

// RoleMissingWarnings returns the role_missing warning of a manifest that does
// not declare metadata.role but points to one. A manifest with no signal at all
// (a plain foundation or app addon) returns nothing, so this release adds no
// noise to every existing manifest; hosts can call InferRole themselves.
func RoleMissingWarnings(f RoleFacts) []string {
	if f.Role != "" {
		return nil
	}
	if role := InferRole(f); role != "" {
		return []string{fmt.Sprintf("[%s] metadata.role is not declared; inferred %q — declare it explicitly", RuleRoleMissing, role)}
	}
	return nil
}
