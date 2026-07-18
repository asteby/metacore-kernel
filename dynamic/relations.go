package dynamic

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// relIdentRe matches a safe SQL identifier (a single unqualified segment).
// Pivot table / column names originate in trusted host config but are still
// validated so a malformed registry entry degrades to a skipped relation
// rather than an interpolation hazard.
var relIdentRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

func isSafeIdent(s string) bool { return relIdentRe.MatchString(s) }

// Relation is a resolved many-to-many association the dynamic engine acts on
// as a side effect of Create/Update/Delete. Hosts translate their addon
// metadata (manifest.RelationDef + the addon Postgres schema) into these
// already-qualified, ready-to-execute descriptors and wire them through
// Config.RelationResolver — the same pattern as TableNameResolver, which
// already turns a model name into a schema-qualified table.
//
// The kernel deliberately does NOT reconstruct the pivot table name, its
// schema, or the two join columns from the manifest itself: the served
// modelbase.TableMetadata.Relations (RelationMeta) intentionally drops Pivot
// and References, and the target-side join column is not declared on
// manifest.RelationDef at all. Pushing the (schema, pivot, columns) resolution
// to the host — which owns the addon registry — keeps the kernel free of
// fragile naming conventions and makes the write-side association handling
// exact rather than guessed.
//
// Only many_to_many is honoured on the write side today. one_to_many /
// has_many children are intentionally left untouched (see the package gap note
// in Delete): deleting or re-parenting child rows is not an unambiguous
// contract, so the conservative choice is to clean up the join rows a
// many_to_many owns (which it exclusively owns) and never cascade into
// independently-owned child records.
type Relation struct {
	// Name is the input key that carries the child IDs for this relation.
	// Create/Update read input[Name] (and input[Name+"_ids"] as an alias) as
	// the desired set of related IDs. Absent from the input map = the relation
	// is left untouched (PATCH semantics); present-but-empty = cleared.
	Name string

	// Kind discriminates the relation shape. Only "many_to_many" is acted on
	// by the write side; any other value is ignored (the relation still serves
	// read/metadata purposes elsewhere).
	Kind string

	// PivotTable is the join table, already schema-qualified by the host
	// (e.g. "addon_crm.customer_tags"). Each dotted segment must be a safe SQL
	// identifier or the relation is skipped.
	PivotTable string

	// OwnerColumn is the pivot column pointing at THIS model's row
	// (manifest.RelationDef.ForeignKey), e.g. "customer_id".
	OwnerColumn string

	// TargetColumn is the pivot column pointing at the related model's row,
	// e.g. "tag_id". Not derivable from manifest.RelationDef — the host supplies
	// it explicitly.
	TargetColumn string
}

func (r Relation) isM2M() bool { return strings.EqualFold(r.Kind, "many_to_many") }

// valid reports whether the descriptor is safe to build raw SQL from. Table
// and column identifiers originate in host config (trusted, like
// TableNameResolver output) but are still validated so a malformed registry
// entry degrades to a skipped relation instead of a broken statement.
func (r Relation) valid() bool {
	if r.OwnerColumn == "" || r.TargetColumn == "" || r.PivotTable == "" {
		return false
	}
	if !isSafeIdent(r.OwnerColumn) || !isSafeIdent(r.TargetColumn) {
		return false
	}
	for _, part := range strings.Split(r.PivotTable, ".") {
		if !isSafeIdent(part) {
			return false
		}
	}
	return true
}

// RelationResolver returns the many-to-many relations declared for a model,
// resolved to executable pivot descriptors. Host-wired from the addon
// registry, like StageMachineResolver / ConstraintResolver. Returning
// (nil, false) — or leaving the resolver nil — disables association handling
// for the model (full back-compat: flat models are entirely unaffected).
type RelationResolver func(ctx context.Context, model string) ([]Relation, bool)

// resolveRelations returns the model's write-side relations, or nil when none
// are wired / declared.
func (s *Service) resolveRelations(ctx context.Context, model string) []Relation {
	if s.relations == nil {
		return nil
	}
	rels, ok := s.relations(ctx, model)
	if !ok {
		return nil
	}
	return rels
}

// relationInputPresent reports whether the input map carries a value for the
// relation (under Name or Name+"_ids"). Used by Update to preserve PATCH
// semantics: a relation whose key is absent is left untouched.
func relationInputPresent(rel Relation, input map[string]any) (any, bool) {
	if v, ok := input[rel.Name]; ok {
		return v, true
	}
	if v, ok := input[rel.Name+"_ids"]; ok {
		return v, true
	}
	return nil, false
}

// extractRelationIDs coerces a relation input value into the set of related
// UUIDs. It accepts a JSON array of id strings/uuids ([]any, []string,
// []uuid.UUID) — the shape a form sends for a multi-select — and silently
// drops entries that are not parseable UUIDs (validation of the related set is
// the host's concern; here we only guard the SQL). Duplicates are collapsed so
// the pivot never gets a doubled row.
func extractRelationIDs(v any) []uuid.UUID {
	if v == nil {
		return nil
	}
	seen := make(map[uuid.UUID]struct{})
	out := make([]uuid.UUID, 0)
	add := func(id uuid.UUID) {
		if id == uuid.Nil {
			return
		}
		if _, dup := seen[id]; dup {
			return
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	switch arr := v.(type) {
	case []uuid.UUID:
		for _, id := range arr {
			add(id)
		}
	case []string:
		for _, s := range arr {
			if id, err := uuid.Parse(strings.TrimSpace(s)); err == nil {
				add(id)
			}
		}
	case []any:
		for _, e := range arr {
			switch ev := e.(type) {
			case string:
				if id, err := uuid.Parse(strings.TrimSpace(ev)); err == nil {
					add(id)
				}
			case uuid.UUID:
				add(ev)
			}
		}
	}
	return out
}

// syncPivotOnWrite is the shared Create/Update body. For each many-to-many
// relation whose IDs are present in input it replaces the owner's join rows:
// delete the current set, then insert the desired set. On Create `replace` is
// effectively an insert (there are no prior rows). Runs on execDB so a caller
// that owns a transaction (the row-locking Update path) keeps the pivot writes
// in the same transaction. onCreate skips the DELETE (no prior rows) as a
// small optimisation and to avoid a redundant statement on the hot path.
func (s *Service) syncPivotOnWrite(ctx context.Context, execDB *gorm.DB, rels []Relation, ownerID uuid.UUID, input map[string]any, onCreate bool) error {
	for _, rel := range rels {
		if !rel.isM2M() || !rel.valid() {
			continue
		}
		raw, present := relationInputPresent(rel, input)
		if !present {
			continue
		}
		ids := extractRelationIDs(raw)
		if !onCreate {
			if err := s.deletePivotForOwner(ctx, execDB, rel, ownerID); err != nil {
				return err
			}
		}
		if err := s.insertPivotRows(ctx, execDB, rel, ownerID, ids); err != nil {
			return err
		}
	}
	return nil
}

// insertPivotRows writes one join row per related id. It is a no-op for an
// empty set (a cleared relation). GORM's map-slice Create emits a single
// multi-row INSERT.
func (s *Service) insertPivotRows(ctx context.Context, execDB *gorm.DB, rel Relation, ownerID uuid.UUID, ids []uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	rows := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		rows = append(rows, map[string]any{
			rel.OwnerColumn:  ownerID,
			rel.TargetColumn: id,
		})
	}
	if err := execDB.WithContext(ctx).Table(rel.PivotTable).Create(rows).Error; err != nil {
		return fmt.Errorf("dynamic: pivot insert (%s): %w", rel.Name, err)
	}
	return nil
}

// deletePivotForOwner removes every join row owned by ownerID. Used both by
// Update (before re-inserting the new set) and Delete (cleanup so a removed
// owner never leaves orphan join rows). The delete is inherently tenant-scoped:
// ownerID was resolved through the tenant-scoped owner load, so only rows for a
// row the caller can see are touched.
func (s *Service) deletePivotForOwner(ctx context.Context, execDB *gorm.DB, rel Relation, ownerID uuid.UUID) error {
	// Raw Exec (not GORM's Delete) because a soft-delete-less join table has no
	// model/dest for GORM to reflect on, and .Table(...).Delete(nil) is not a
	// portable way to express an unconditional-by-column delete. Identifiers are
	// validated by rel.valid() before this runs.
	sql := fmt.Sprintf("DELETE FROM %s WHERE %s = ?", quoteQualified(rel.PivotTable), quoteIdent(rel.OwnerColumn))
	if err := execDB.WithContext(ctx).Exec(sql, ownerID).Error; err != nil {
		return fmt.Errorf("dynamic: pivot delete (%s): %w", rel.Name, err)
	}
	return nil
}

// quoteQualified double-quotes each dotted segment of a (possibly
// schema-qualified) table name so "addon_crm.customer_tags" becomes
// "addon_crm"."customer_tags". Segments are pre-validated by Relation.valid().
func quoteQualified(table string) string {
	parts := strings.Split(table, ".")
	for i, p := range parts {
		parts[i] = quoteIdent(p)
	}
	return strings.Join(parts, ".")
}

// cleanupPivotsOnDelete drops the join rows for every many-to-many relation of
// a deleted owner. Best-effort in spirit but returns the first error so the
// caller can surface it; called after the owner delete has succeeded.
func (s *Service) cleanupPivotsOnDelete(ctx context.Context, rels []Relation, ownerID uuid.UUID) error {
	for _, rel := range rels {
		if !rel.isM2M() || !rel.valid() {
			continue
		}
		if err := s.deletePivotForOwner(ctx, s.db, rel, ownerID); err != nil {
			return err
		}
	}
	return nil
}
