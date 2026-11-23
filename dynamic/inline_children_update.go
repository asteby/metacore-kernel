package dynamic

// inline_children_update.go — the Update side of `_children` (inline_children.go).
//
// An Update that carries `_children` SYNCHRONISES each relation it names with
// the posted rows, the same contract Create reads (`{relation: [rows]}`):
//
//   - a row with the `id` of an existing child of this parent is UPDATED;
//   - a row without `id` is CREATED;
//   - an existing child that is not in the list is DELETED (soft delete);
//   - an empty list removes every child (subject to the relation's `min`);
//   - a relation that is not named is left untouched (PATCH semantics, like the
//     many-to-many pivots).
//
// Each child goes through the regular Create / Update / Delete of ITS model
// (resolved by name, so a child of another addon — relation.addon — keeps its
// own validation, hooks, permissions and organization scope); the foreign key,
// the relation's Scope and the id of a new child are runtime-owned.
//
// Everything that can be refused is refused BEFORE the parent is written: the
// shape, `min`, unknown ids, a missing child model (addon not installed) and the
// caller's permission on each child model (create / update / delete as needed).
// What can only fail while writing (a child's own validation, a hook) is
// compensated: the children already written are undone (created rows hard
// deleted, updated rows restored from their snapshot, deleted rows revived) and
// the parent's columns are put back. The compensation is best effort, like the
// one of Create: it writes straight to the tables (no hooks, no events), skips
// columns that hold JSON values, and does not undo the side effects of other
// post-commit steps (pivots, extensions, document lines).

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/modelbase"
)

// inlineRowUpdate is a posted row that updates an existing child.
type inlineRowUpdate struct {
	id     string
	index  int
	row    map[string]any
	before map[string]any
}

// inlineRelSync is the plan of one relation: what to create, update and remove.
type inlineRelSync struct {
	rel    InlineRelation
	create []inlineRowCreate
	update []inlineRowUpdate
	remove []map[string]any
}

type inlineRowCreate struct {
	index int
	row   map[string]any
}

// inlineSyncPlan is what planInlineChildrenUpdate extracted from an update input.
type inlineSyncPlan struct {
	rels []inlineRelSync
}

// inlineRestore is an updated child to put back: the columns the update wrote,
// with their previous values.
type inlineRestore struct {
	model string
	id    string
	set   map[string]any
}

// inlineSyncUndo is what applyInlineChildrenUpdate wrote, for compensation.
type inlineSyncUndo struct {
	created []inlineWritten
	updated []inlineRestore
	removed []inlineWritten
}

// planInlineChildrenUpdate takes `_children` out of an update input and turns it
// into a plan. nil, nil, nil when the payload does not carry the key. A shape,
// `min`, unknown-id or missing-model problem comes back as a *ValidationError
// (the parent is not written); the error result is for a refusal that is not a
// field problem (the caller may not write a child model).
func (s *Service) planInlineChildrenUpdate(ctx context.Context, model string, user modelbase.AuthUser, parentID string, input map[string]any) (*inlineSyncPlan, *ValidationError, error) {
	ic, ve := s.parseInlineChildren(ctx, model, input, true)
	if ve != nil {
		return nil, ve, nil
	}
	if ic == nil || len(ic.batches) == 0 {
		return nil, nil, nil
	}

	ve = NewValidationError()
	plan := &inlineSyncPlan{}
	for _, b := range ic.batches {
		key := InlineChildrenKey + "." + b.rel.Name
		instance, _, err := s.resolveModel(ctx, b.rel.Model)
		if errors.Is(err, ErrModelNotFound) {
			ve.Add(key, CodeInlineChildUnavailable, map[string]any{"model": b.rel.Model, "addon": b.rel.Addon})
			continue
		}
		if err != nil {
			return nil, nil, err
		}

		existing, err := s.fetchRows(ctx, user, b.rel.Model, b.rel.ForeignKey, []any{parentID}, false)
		if err != nil {
			return nil, nil, err
		}
		byID := make(map[string]map[string]any, len(existing))
		for _, r := range existing {
			if !matchesScope(r, b.rel.Scope) {
				continue // another discriminator of a shared table: not this relation's child
			}
			byID[fmt.Sprint(r["id"])] = r
		}

		sync := inlineRelSync{rel: b.rel}
		seen := make(map[string]struct{}, len(b.rows))
		for i, r := range b.rows {
			id := childRowID(r)
			if id == "" {
				sync.create = append(sync.create, inlineRowCreate{index: i, row: r})
				continue
			}
			if _, dup := seen[id]; dup {
				ve.Add(fmt.Sprintf("%s.%d.id", key, i), CodeInlineInvalidType, nil)
				continue
			}
			seen[id] = struct{}{}
			old, ok := byID[id]
			if !ok {
				ve.Add(fmt.Sprintf("%s.%d.id", key, i), codeNotFound, nil)
				continue
			}
			sync.update = append(sync.update, inlineRowUpdate{id: id, index: i, row: r, before: old})
		}
		ids := make([]string, 0, len(byID))
		for id := range byID {
			if _, kept := seen[id]; !kept {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids) // deterministic order
		for _, id := range ids {
			sync.remove = append(sync.remove, byID[id])
		}

		// The parent is not written when the caller cannot do any of it.
		for _, need := range []struct {
			n  int
			ac modelbase.AccessAction
		}{
			{len(sync.create), modelbase.AccessCreate},
			{len(sync.update), modelbase.AccessUpdate},
			{len(sync.remove), modelbase.AccessDelete},
		} {
			if need.n == 0 {
				continue
			}
			if err := s.authorize(ctx, user, b.rel.Model, instance, need.ac); err != nil {
				return nil, nil, err
			}
		}
		plan.rels = append(plan.rels, sync)
	}
	if !ve.Empty() {
		return nil, ve, nil
	}
	if len(plan.rels) == 0 {
		return nil, nil, nil
	}
	return plan, nil, nil
}

// applyInlineChildrenUpdate writes the plan through the regular Create / Update /
// Delete of each child model. It always returns what it wrote so far, the failing
// relation's partial work included, so the caller can undo it; the error is the
// child's, keyed under `_children.<relation>.<index>.<field>` when it is a
// validation one.
func (s *Service) applyInlineChildrenUpdate(ctx context.Context, user modelbase.AuthUser, plan *inlineSyncPlan, parentID string) (*inlineSyncUndo, error) {
	undo := &inlineSyncUndo{}
	if plan == nil {
		return undo, nil
	}
	for _, rp := range plan.rels {
		rel := rp.rel
		field := InlineChildrenKey + "." + rel.Name

		for _, u := range rp.update {
			row := stampChild(u.row, rel, parentID)
			delete(row, "id")
			set := make(map[string]any, len(row)+2)
			for k := range row {
				if v, ok := u.before[k]; ok && !isJSONValue(v) {
					set[k] = v
				}
			}
			for _, k := range []string{ColUpdatedAt, ColUpdatedByID} {
				if v, ok := u.before[k]; ok && !isJSONValue(v) {
					set[k] = v
				}
			}
			uid, err := uuid.Parse(u.id)
			if err != nil {
				return undo, lineError(field, u.index, u.index, fmt.Errorf("%w: id %q", ErrInvalidInput, u.id))
			}
			if _, err := s.Update(ctx, rel.Model, user, uid, row); err != nil {
				// The failed update may have written before failing: restore it too.
				undo.updated = append(undo.updated, inlineRestore{model: rel.Model, id: u.id, set: set})
				return undo, lineError(field, u.index, u.index, err)
			}
			undo.updated = append(undo.updated, inlineRestore{model: rel.Model, id: u.id, set: set})
		}

		w := inlineWritten{model: rel.Model}
		for _, c := range rp.create {
			row := stampChild(c.row, rel, parentID)
			id := uuid.NewString()
			row["id"] = id
			if _, err := s.Create(ctx, rel.Model, user, row); err != nil {
				undo.created = append(undo.created, w)
				return undo, lineError(field, c.index, c.index, err)
			}
			w.ids = append(w.ids, id)
		}
		undo.created = append(undo.created, w)

		rm := inlineWritten{model: rel.Model}
		for i, old := range rp.remove {
			id := fmt.Sprint(old["id"])
			uid, err := uuid.Parse(id)
			if err != nil {
				return undo, lineError(field, i, i, fmt.Errorf("%w: id %q", ErrInvalidInput, id))
			}
			if err := s.Delete(ctx, rel.Model, user, uid); err != nil {
				undo.removed = append(undo.removed, rm)
				return undo, lineError(field, i, i, err)
			}
			rm.ids = append(rm.ids, id)
		}
		undo.removed = append(undo.removed, rm)
	}
	return undo, nil
}

// undoInlineChildrenUpdate puts the children back as they were: created rows are
// hard deleted, updated rows get their previous column values and deleted rows
// are revived. Best effort, straight to the tables (no hooks, no events).
func (s *Service) undoInlineChildrenUpdate(ctx context.Context, undo *inlineSyncUndo) {
	if undo == nil {
		return
	}
	s.undoInlineChildren(ctx, undo.created)
	for _, r := range undo.updated {
		if len(r.set) == 0 {
			continue
		}
		inst, ok := s.lookupModel(ctx, r.model)
		if !ok {
			continue
		}
		table, err := s.tableNameFor(ctx, r.model, inst)
		if err != nil {
			continue
		}
		_ = s.db.WithContext(ctx).Table(table).Where("id = ?", r.id).Updates(r.set).Error
	}
	for _, w := range undo.removed {
		if len(w.ids) == 0 {
			continue
		}
		inst, ok := s.lookupModel(ctx, w.model)
		if !ok {
			continue
		}
		table, err := s.tableNameFor(ctx, w.model, inst)
		if err != nil {
			continue
		}
		revive := map[string]any{ColDeletedAt: nil}
		if s.hasLiveColumn(ctx, table, ColDeletedByID) {
			revive[ColDeletedByID] = nil
		}
		_ = s.db.WithContext(ctx).Table(table).Where("id IN ?", w.ids).Updates(revive).Error
	}
}

// revertParentUpdate puts back the parent columns an update wrote, from the row
// as it was loaded: the parent must not keep a change whose children could not
// be written. Only the keys of the input (plus the audit stamps) are restored.
func (s *Service) revertParentUpdate(ctx context.Context, table, id string, before, input map[string]any) {
	if len(before) == 0 {
		return
	}
	restore := make(map[string]any, len(input)+2)
	for k := range input {
		if v, ok := before[k]; ok && k != "id" && !isJSONValue(v) {
			restore[k] = v
		}
	}
	for _, k := range []string{ColUpdatedAt, ColUpdatedByID} {
		if v, ok := before[k]; ok && !isJSONValue(v) {
			restore[k] = v
		}
	}
	if len(restore) == 0 {
		return
	}
	_ = s.db.WithContext(ctx).Table(table).Where("id = ?", id).Updates(restore).Error
}

// stampChild copies a posted child row and stamps what the runtime owns: the
// foreign key to the parent and the relation's Scope (whatever the client sent
// for them is overwritten).
func stampChild(src map[string]any, rel InlineRelation, parentID string) map[string]any {
	row := make(map[string]any, len(src)+len(rel.Scope)+2)
	for k, v := range src {
		row[k] = v
	}
	for k, v := range rel.Scope {
		row[k] = v
	}
	row[rel.ForeignKey] = parentID
	return row
}

// childRowID reads a posted row's `id` as a trimmed string ("" when absent or null).
func childRowID(r map[string]any) string {
	v, ok := r["id"]
	if !ok || v == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(v))
}

// matchesScope reports whether a child row carries the relation's static scope
// values (polymorphic children). An empty scope matches every row.
func matchesScope(row map[string]any, scope map[string]string) bool {
	for k, want := range scope {
		if fmt.Sprint(row[k]) != want {
			return false
		}
	}
	return true
}

// isJSONValue reports whether v is a decoded JSON object or array: those columns
// are not restored by the compensation (a nested value cannot be written back
// through a plain column update).
func isJSONValue(v any) bool {
	switch v.(type) {
	case map[string]any, []any:
		return true
	}
	return false
}
