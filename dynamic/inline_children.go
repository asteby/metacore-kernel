package dynamic

// inline_children.go — create a parent record together with its child rows in
// ONE call, for one_to_many relations that declare `create_inline`
// (manifest RelationDef.CreateInline, docs/inline-relations.md).
//
// The client posts the children under the reserved key `_children`:
//
//	{ "name": "Regla", ..., "_children": { "tiers": [ {...}, {...} ] } }
//
// Create takes the key out of the input BEFORE validating/writing the parent
// (it is not a column), checks the shape and each relation's `min`, creates the
// parent, then writes every child through the regular Create of the child model
// (formulas, rollups, hooks, events and the child's create permission all run),
// injecting the relation's foreign key and Scope. It is the sibling of the
// document lines of document_lines.go and reuses its error placement
// (lineError) and its compensation (undoDocument): when a child fails, the
// parent and the children already written are removed and the error comes back
// keyed `_children.<relation>.<index>.<field>`.
//
// Without `_children` Create behaves exactly as before.

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/modelbase"
)

// InlineChildrenKey is the reserved input key that carries the child rows of
// the create_inline relations of a model.
const InlineChildrenKey = "_children"

// Field-error codes of the `_children` checks (the SDK localizes them).
const (
	// CodeInlineMin: fewer child rows than the relation's `min`
	// (params: min, count).
	CodeInlineMin = "min"
	// CodeInlineUnknownRelation: the relation does not exist or does not
	// declare create_inline.
	CodeInlineUnknownRelation = "invalid_relation"
	// CodeInlineInvalidType: `_children`, a relation's value or a row has the
	// wrong shape (not an object / not a list of objects).
	CodeInlineInvalidType = "invalid_type"
	// CodeInlineChildUnavailable: the relation's child model is not registered
	// (its addon — relation.addon — is not installed or not loaded). Nothing is
	// written; params carry {model, addon}.
	CodeInlineChildUnavailable = "child_unavailable"
)

// InlineRelation is the executable form of a create_inline relation.
type InlineRelation struct {
	// Name is the relation name, the key under `_children`.
	Name string
	// Model is the child model (the relation's `through`).
	Model string
	// Addon is the addon that owns Model when it is not the parent's own (the
	// relation's `addon`); empty = same addon. Only used to name the missing
	// addon when Model is not registered: the child is written through the same
	// Create as any other model, resolved by name, with its own validation,
	// hooks, permissions and organization scope.
	Addon string
	// ForeignKey is the child column that receives the parent id.
	ForeignKey string
	// Scope is the static equality filter of the relation (polymorphic
	// children); its values are stamped on every child row.
	Scope map[string]string
	// Min is the minimum number of child rows when `_children` is sent.
	Min int
}

// InlineRelationsResolver returns a model's create_inline relations. Host-wired
// from the addon registry (see DeriveInlineRelations), like DocumentLinesResolver.
// nil / ok=false = the model declares none: a `_children` key is rejected.
type InlineRelationsResolver func(ctx context.Context, model string) ([]InlineRelation, bool)

// DeriveInlineRelations lists the create_inline relations of def, in
// declaration order. A relation that is not one_to_many, is readonly or has an
// unsafe foreign key is skipped (the manifest validator rejects them anyway).
func DeriveInlineRelations(def manifest.ModelDefinition) []InlineRelation {
	var out []InlineRelation
	for _, r := range def.Relations {
		if !r.CreateInline || r.Readonly || r.Name == "" || r.Through == "" || !isSafeIdent(r.ForeignKey) {
			continue
		}
		if r.Kind != "" && !strings.EqualFold(r.Kind, "one_to_many") {
			continue
		}
		var scope map[string]string
		if len(r.Scope) > 0 {
			scope = make(map[string]string, len(r.Scope))
			for k, v := range r.Scope {
				scope[k] = v
			}
		}
		mn := r.Min
		if mn < 0 {
			mn = 0
		}
		out = append(out, InlineRelation{Name: r.Name, Model: r.Through, Addon: r.Addon, ForeignKey: r.ForeignKey, Scope: scope, Min: mn})
	}
	return out
}

func (s *Service) resolveInlineRelations(ctx context.Context, model string) []InlineRelation {
	if s.inlineRelations == nil {
		return nil
	}
	rels, ok := s.inlineRelations(ctx, model)
	if !ok {
		return nil
	}
	return rels
}

// inlineBatch is the rows posted for one relation.
type inlineBatch struct {
	rel  InlineRelation
	rows []map[string]any
}

// inlineChildren is what takeInlineChildren extracted from a create input.
type inlineChildren struct {
	batches []inlineBatch
}

// inlineWritten are the ids written into one child model, for compensation.
type inlineWritten struct {
	model string
	ids   []string
}

// takeInlineChildren removes `_children` from input (it is not a column) and
// returns its rows. nil, nil, nil when the payload does not carry the key.
//
// A malformed payload comes back as a *ValidationError (merged by Create with
// the parent's own validation, so the form sees every problem at once); the
// error result is for a refusal that is not a field problem — the caller may
// not create the child model (the parent is not written without its children).
func (s *Service) takeInlineChildren(ctx context.Context, model string, user modelbase.AuthUser, input map[string]any) (*inlineChildren, *ValidationError, error) {
	raw, ok := input[InlineChildrenKey]
	if !ok {
		return nil, nil, nil
	}
	delete(input, InlineChildrenKey)

	ve := NewValidationError()
	posted, ok := childrenByRelation(raw)
	if !ok {
		return nil, ve.Add(InlineChildrenKey, CodeInlineInvalidType, nil), nil
	}

	rels := s.resolveInlineRelations(ctx, model)
	declared := make(map[string]struct{}, len(rels))
	for _, r := range rels {
		declared[r.Name] = struct{}{}
	}
	names := make([]string, 0, len(posted))
	for name := range posted {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic error order
	for _, name := range names {
		if _, ok := declared[name]; !ok {
			ve.Add(InlineChildrenKey+"."+name, CodeInlineUnknownRelation, nil)
		}
	}

	ic := &inlineChildren{}
	for _, rel := range rels {
		key := InlineChildrenKey + "." + rel.Name
		rows, bad, isList := inlineRows(posted[rel.Name])
		if !isList {
			ve.Add(key, CodeInlineInvalidType, nil)
			continue
		}
		for _, i := range bad {
			ve.Add(fmt.Sprintf("%s.%d", key, i), CodeInlineInvalidType, nil)
		}
		for i, r := range rows {
			if len(bad) > 0 {
				break // indexes would not match the posted array; already rejected
			}
			if _, nested := r[InlineChildrenKey]; nested {
				// Children of children are not written (no compensation path).
				ve.Add(fmt.Sprintf("%s.%d.%s", key, i, InlineChildrenKey), CodeInlineInvalidType, nil)
			}
		}
		if len(rows)+len(bad) < rel.Min {
			ve.Add(key, CodeInlineMin, map[string]any{"min": rel.Min, "count": len(rows) + len(bad)})
		}
		if len(rows) > 0 {
			ic.batches = append(ic.batches, inlineBatch{rel: rel, rows: rows})
		}
	}
	if !ve.Empty() {
		return nil, ve, nil
	}

	// The parent is not written when the caller cannot create its children.
	for _, b := range ic.batches {
		instance, _, err := s.resolveModel(ctx, b.rel.Model)
		if errors.Is(err, ErrModelNotFound) {
			// The child's addon is not installed: a clear field error, and the
			// parent is not created (nothing was written yet).
			ve.Add(InlineChildrenKey+"."+b.rel.Name, CodeInlineChildUnavailable, map[string]any{"model": b.rel.Model, "addon": b.rel.Addon})
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		if err := s.authorize(ctx, user, b.rel.Model, instance, modelbase.AccessCreate); err != nil {
			return nil, nil, err
		}
	}
	if !ve.Empty() {
		return nil, ve, nil
	}
	if len(ic.batches) == 0 {
		return nil, nil, nil
	}
	return ic, nil, nil
}

// childrenByRelation reads the `_children` value: an object keyed by relation
// name. nil is an empty object.
func childrenByRelation(raw any) (map[string]any, bool) {
	if raw == nil {
		return map[string]any{}, true
	}
	if m, ok := raw.(map[string]any); ok {
		return m, true
	}
	v := reflect.ValueOf(raw)
	if v.Kind() != reflect.Map || v.Type().Key().Kind() != reflect.String {
		return nil, false
	}
	out := make(map[string]any, v.Len())
	for _, k := range v.MapKeys() {
		out[k.String()] = v.MapIndex(k).Interface()
	}
	return out, true
}

// inlineRows reads one relation's value as a list of objects. bad lists the
// indexes that are not objects (kept out of rows); isList=false when the value
// is not a list at all. nil is an empty list.
func inlineRows(raw any) (rows []map[string]any, bad []int, isList bool) {
	if raw == nil {
		return nil, nil, true
	}
	v := reflect.ValueOf(raw)
	if v.Kind() != reflect.Slice && v.Kind() != reflect.Array {
		return nil, nil, false
	}
	rows = make([]map[string]any, 0, v.Len())
	for i := 0; i < v.Len(); i++ {
		m, ok := v.Index(i).Interface().(map[string]any)
		if !ok {
			bad = append(bad, i)
			continue
		}
		rows = append(rows, m)
	}
	return rows, bad, true
}

// writeInlineChildren creates the children of a parent through the regular
// Create of the child model. The foreign key and the relation's Scope come from
// the runtime: whatever the client sent for them (or for `id`) is overwritten.
// Returns what was written (the failing relation's partial batch included) so
// the caller can undo it, and a child's failure keyed under
// `_children.<relation>.<index>.<field>`.
func (s *Service) writeInlineChildren(ctx context.Context, user modelbase.AuthUser, ic *inlineChildren, parentID string) ([]inlineWritten, error) {
	if ic == nil {
		return nil, nil
	}
	var done []inlineWritten
	for _, b := range ic.batches {
		w := inlineWritten{model: b.rel.Model}
		for i, r := range b.rows {
			row := make(map[string]any, len(r)+len(b.rel.Scope)+2)
			for k, v := range r {
				row[k] = v
			}
			for k, v := range b.rel.Scope {
				row[k] = v
			}
			id := uuid.NewString()
			row["id"] = id
			row[b.rel.ForeignKey] = parentID
			if _, err := s.Create(ctx, b.rel.Model, user, row); err != nil {
				done = append(done, w)
				return done, lineError(InlineChildrenKey+"."+b.rel.Name, i, i, err)
			}
			w.ids = append(w.ids, id)
		}
		done = append(done, w)
	}
	return done, nil
}

// undoInlineChildren hard-deletes the children already written (the parent is
// removed by undoDocument right after).
func (s *Service) undoInlineChildren(ctx context.Context, written []inlineWritten) {
	for _, w := range written {
		s.hardDeleteRows(ctx, w.model, w.ids)
	}
}
