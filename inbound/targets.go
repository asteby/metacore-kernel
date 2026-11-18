package inbound

import (
	"context"
	"sort"
	"strings"

	"github.com/asteby/metacore-kernel/modelbase"
	"github.com/google/uuid"
)

// SystemFields is the single source of truth of the columns an inbound entry
// never writes, even when the model declares them: identity, tenancy, scope,
// audit and soft-delete. The contract compiler, the executor check and the
// target catalogue all read it. It is the union of the kernel's historical
// list and the OPS host gate (inboundSystemFields).
var SystemFields = map[string]bool{
	"id": true, "organization_id": true, "tenant_id": true, "branch_id": true, "entity_id": true,
	"created_by_id": true, "created_by": true, "updated_by": true,
	"created_at": true, "updated_at": true, "deleted_at": true,
}

// TargetColumn is a column an inbound entry may write.
type TargetColumn struct {
	Name     string                `json:"name"`
	Label    string                `json:"label,omitempty"`
	Type     string                `json:"type,omitempty"`
	Required bool                  `json:"required,omitempty"`
	Unique   bool                  `json:"unique,omitempty"`
	Ref      string                `json:"ref,omitempty"`
	Options  []modelbase.OptionDef `json:"options,omitempty"`
	Default  any                   `json:"default,omitempty"`
}

// TargetPermissions says which writes the organization's caller may perform.
type TargetPermissions struct {
	Create bool `json:"create"`
	Update bool `json:"update"`
	Delete bool `json:"delete"`
}

// TargetScope says which scope fences (branch/entity) the model honours.
type TargetScope struct {
	Branch bool `json:"branch"`
	Entity bool `json:"entity"`
}

// Target is the catalogue entry of one model an inbound chain can write; the
// builder UI consumes it. The host fills it (TargetSource); the helpers in
// this file derive Columns and MatchCandidates from the model metadata.
type Target struct {
	Addon           string            `json:"addon"`
	AddonName       string            `json:"addon_name,omitempty"`
	Model           string            `json:"model"`
	Label           string            `json:"label,omitempty"`
	Columns         []TargetColumn    `json:"columns"`
	MatchCandidates [][]string        `json:"match_candidates"`
	SoftDelete      bool              `json:"soft_delete"`
	Actions         []string          `json:"actions"`
	Permissions     TargetPermissions `json:"permissions"`
	Scope           TargetScope       `json:"scope"`
	// GuardCandidates are columns usable in an action guard (e.g. a status).
	GuardCandidates []string `json:"guard_candidates,omitempty"`
}

// TargetSource lists the targets an organization may write by inbound events.
type TargetSource interface {
	Targets(ctx context.Context, org uuid.UUID) ([]Target, error)
}

// writable reports whether an inbound entry may write the column.
func writable(c modelbase.ColumnDef) bool {
	return c.Key != "" && !SystemFields[c.Key] && !c.Readonly && c.Generated == "" && !c.Protected
}

// WritableColumns returns the columns of meta an entry may write, in
// declaration order: it excludes SystemFields and Readonly, Generated and
// Protected columns.
func WritableColumns(meta *modelbase.TableMetadata) []TargetColumn {
	if meta == nil {
		return nil
	}
	out := []TargetColumn{}
	for _, c := range meta.Columns {
		if !writable(c) {
			continue
		}
		out = append(out, TargetColumn{
			Name: c.Key, Label: c.Label, Type: c.Type, Required: c.Required,
			Unique: c.Unique, Ref: c.Ref, Options: c.Options, Default: c.Default,
		})
	}
	return out
}

// MatchCandidates returns the column sets that identify one row: each unique
// column and each unique index (composite and partial ones included, since a
// "WHERE deleted_at IS NULL" index still identifies the live row). The tenant
// column organization_id is ignored because the executor always scopes by it.
// Sets that still hold a non-writable column are dropped (a payload cannot
// supply them). The result is deterministic: sorted, without duplicates.
func MatchCandidates(meta *modelbase.TableMetadata) [][]string {
	out := [][]string{}
	if meta == nil {
		return out
	}
	byName := map[string]modelbase.ColumnDef{}
	for _, c := range meta.Columns {
		byName[c.Key] = c
	}
	seen := map[string]bool{}
	add := func(cols []string) {
		set := make([]string, 0, len(cols))
		for _, n := range cols {
			if n == "organization_id" {
				continue
			}
			if c, ok := byName[n]; !ok || !writable(c) {
				return
			}
			set = append(set, n)
		}
		if len(set) == 0 {
			return
		}
		if k := strings.Join(set, ","); !seen[k] {
			seen[k] = true
			out = append(out, set)
		}
	}
	for _, c := range meta.Columns {
		if c.Unique {
			add([]string{c.Key})
		}
	}
	for _, ix := range meta.UniqueIndexes {
		add(ix.Columns)
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i]) != len(out[j]) {
			return len(out[i]) < len(out[j])
		}
		return strings.Join(out[i], ",") < strings.Join(out[j], ",")
	})
	return out
}
