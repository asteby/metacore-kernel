package dynamic

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/manifest/computeexpr"
	"github.com/asteby/metacore-kernel/modelbase"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// stamp.go: the CREATE-time "stamp" phase shared by every write path.
//
// Two declarative primitives run here, in this order, before the model's
// formulas and guards:
//
//  1. Snapshots (manifest Model.snapshots): copy columns from a related row
//     (the product's floor price and cost at the moment of sale, the parent
//     order's branch and seller). The targets are protected: whatever the
//     caller sent is discarded and the server value wins.
//  2. Grants (manifest Column.grant): a column carrying a supervisor
//     authorization id is redeemed (single use) when the authorization is
//     actually needed, and a protected flag column is stamped 1.
//
// Formulas that read those columns (price_shortfall = f(min_price_at_sale, ...))
// are re-evaluated here for the paths that do not run the compute hooks (the
// wasm data_mutate / data_batch tier); on Service.Create the compute hooks run
// right after and produce the same values.

// StampGrant binds a grant to the column that carries the approval id.
type StampGrant struct {
	Column string
	Def    manifest.GrantDef
}

// ModelStamps is the resolved stamp configuration of one model.
type ModelStamps struct {
	Snapshots []manifest.SnapshotDef
	// RefTables maps a snapshot's From column to the related table.
	RefTables map[string]string
	Grants    []StampGrant
	// Derived are the Tier-2 formulas that (transitively) read a snapshot target
	// or a grant flag, in declared order: re-run by the stamp phase.
	Derived []manifest.Formula
}

// StampResolver returns the stamp configuration of a model; (nil,false) = none.
// Hosts wire it from their installed manifests.
type StampResolver func(ctx context.Context, model string) (*ModelStamps, bool)

// NewModelStamps builds a model's stamp configuration from its projected
// columns, snapshots and formulas. It returns nil when the model declares no
// snapshot and no grant.
func NewModelStamps(cols []manifest.ColumnDef, snaps []manifest.SnapshotDef, formulas []manifest.Formula) *ModelStamps {
	st := &ModelStamps{Snapshots: snaps, RefTables: map[string]string{}}
	refByCol := map[string]string{}
	for _, c := range cols {
		refByCol[c.Name] = c.Ref
		if c.Grant != nil {
			st.Grants = append(st.Grants, StampGrant{Column: c.Name, Def: *c.Grant})
		}
	}
	for _, sn := range snaps {
		if t, ok := refTable(refByCol[sn.From]); ok {
			st.RefTables[sn.From] = t
		}
	}
	if len(st.Snapshots) == 0 && len(st.Grants) == 0 {
		return nil
	}
	// Derived formulas: those reading a stamped column, directly or through
	// another derived formula. Single pass in declared order (the same rule as
	// the formula engine itself).
	stamped := map[string]struct{}{}
	for _, sn := range snaps {
		for _, dst := range sn.Fields {
			stamped[dst] = struct{}{}
		}
	}
	for _, g := range st.Grants {
		stamped[g.Def.Sets] = struct{}{}
	}
	for _, f := range formulas {
		if f.Tier == 3 || f.Expr == "" {
			continue
		}
		ids, err := computeexpr.Idents(f.Expr)
		if err != nil {
			continue
		}
		for _, id := range ids {
			if _, ok := stamped[id]; ok {
				st.Derived = append(st.Derived, f)
				stamped[f.Target] = struct{}{}
				break
			}
		}
	}
	return st
}

func (s *Service) resolveStamps(ctx context.Context, model string) *ModelStamps {
	if s.stamps == nil {
		return nil
	}
	st, ok := s.stamps(ctx, model)
	if !ok || st == nil {
		return nil
	}
	return st
}

// stampNeedsAmount is the tolerance under which a covered amount counts as zero.
const stampNeedsAmount = 0.005

// StampCreate runs the stamp phase on a create row for `model`, on db (the open
// transaction on the wasm path), for the given org and acting user. Safe to call
// for a model without stamps (no-op). The row is mutated in place.
func (s *Service) StampCreate(ctx context.Context, db *gorm.DB, orgID, actor uuid.UUID, model string, row map[string]any) error {
	st := s.resolveStamps(ctx, model)
	if st == nil {
		return nil
	}
	return st.apply(ctx, db, orgID, actor, row, s.ConsumeApproval)
}

// CreateStamper returns StampCreate shaped for wasm.Host.WithCreateStamp; the
// acting user is read from the invocation context.
func (s *Service) CreateStamper() func(ctx context.Context, tx *gorm.DB, orgID uuid.UUID, model string, row map[string]any) error {
	return func(ctx context.Context, tx *gorm.DB, orgID uuid.UUID, model string, row map[string]any) error {
		actor, _ := uuid.Parse(ActorIDFromContext(ctx))
		return s.StampCreate(ctx, tx, orgID, actor, model, row)
	}
}

// stampCreateRow is the Service.Create hook point.
func (s *Service) stampCreateRow(ctx context.Context, model string, user modelbase.AuthUser, input map[string]any) error {
	return s.StampCreate(ctx, s.db, orgIDFromUser(user), user.GetID(), model, input)
}

func (st *ModelStamps) apply(ctx context.Context, db *gorm.DB, orgID, actor uuid.UUID, row map[string]any, consume func(context.Context, ConsumeInput) error) error {
	if err := st.applySnapshots(ctx, db, orgID, row); err != nil {
		return err
	}
	if err := st.applyGrants(ctx, orgID, actor, row, consume); err != nil {
		return err
	}
	if len(st.Derived) > 0 {
		fb := formulaBinding{formulas: st.Derived}
		if err := applyFormulas(ctx, nil, fb, row, nil); err != nil {
			return err
		}
	}
	return nil
}

func (st *ModelStamps) applySnapshots(ctx context.Context, db *gorm.DB, orgID uuid.UUID, row map[string]any) error {
	for _, sn := range st.Snapshots {
		clear := func() {
			for _, dst := range sn.Fields {
				delete(row, dst)
			}
		}
		id := valueToString(row[sn.From])
		table := st.RefTables[sn.From]
		if id == "" || table == "" {
			clear()
			continue
		}
		cols := make([]string, 0, len(sn.Fields))
		for src := range sn.Fields {
			if !computeexpr.IdentRe.MatchString(src) {
				return fmt.Errorf("dynamic: snapshot source column %q is not a valid identifier", src)
			}
			cols = append(cols, quoteIdent(src))
		}
		q := db.WithContext(ctx).Table(table).Select(strings.Join(cols, ", ")).Where("id = ?", id)
		if orgID != uuid.Nil && tableHasOrgID(db, table) {
			q = q.Where("organization_id = ?", orgID)
		}
		var out map[string]any
		if err := q.Take(&out).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				clear()
				continue
			}
			return fmt.Errorf("dynamic: snapshot %s: %w", table, err)
		}
		for src, dst := range sn.Fields {
			v := out[src]
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			if v == nil {
				delete(row, dst)
				continue
			}
			row[dst] = v
		}
	}
	return nil
}

func (st *ModelStamps) applyGrants(ctx context.Context, orgID, actor uuid.UUID, row map[string]any, consume func(context.Context, ConsumeInput) error) error {
	for _, g := range st.Grants {
		row[g.Def.Sets] = 0.0
		raw := valueToString(row[g.Column])
		if raw == "" {
			continue
		}
		env := make(map[string]float64, len(row))
		for k, v := range row {
			env[k] = computeexpr.ToFloat(v)
		}
		gte := make(map[string]float64, len(g.Def.ContextGte))
		needed := false
		for k, expr := range g.Def.ContextGte {
			v, err := computeexpr.Eval(expr, env)
			if err != nil {
				return fmt.Errorf("dynamic: grant %q context_gte %q: %w", g.Column, k, err)
			}
			v = math.Round(v*10000) / 10000
			gte[k] = v
			if v > stampNeedsAmount {
				needed = true
			}
		}
		if !needed {
			// Nothing to authorize: the id is not redeemed (and not burned).
			continue
		}
		id, err := uuid.Parse(raw)
		if err != nil {
			return fmt.Errorf("%w: %s is not an approval id", ErrApprovalConsume, g.Column)
		}
		eq := make(map[string]string, len(g.Def.ContextEq))
		for k, col := range g.Def.ContextEq {
			eq[k] = valueToString(row[col])
		}
		if err := consume(ctx, ConsumeInput{
			OrgID: orgID, ApprovalID: id, Policy: g.Def.Policy, RequestedBy: actor,
			ContextEq: eq, ContextGte: gte,
		}); err != nil {
			return err
		}
		row[g.Def.Sets] = 1.0
	}
	return nil
}
