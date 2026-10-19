package dynamic

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/manifest/computeexpr"
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
	// Relevant are the columns whose change on an UPDATE invalidates what a
	// grant authorized: snapshot sources (`from`) and every column a grant's
	// context_eq / context_gte reads, directly or through a derived formula.
	Relevant map[string]struct{}
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
	st.Relevant = map[string]struct{}{}
	for _, sn := range snaps {
		st.Relevant[sn.From] = struct{}{}
	}
	for _, g := range st.Grants {
		for _, col := range g.Def.ContextEq {
			st.Relevant[col] = struct{}{}
		}
		for _, expr := range g.Def.ContextGte {
			if ids, err := computeexpr.Idents(expr); err == nil {
				for _, id := range ids {
					st.Relevant[id] = struct{}{}
				}
			}
		}
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
	// A relevant column computed by a formula depends on that formula's inputs.
	for changed := true; changed; {
		changed = false
		for _, f := range formulas {
			if _, ok := st.Relevant[f.Target]; !ok || f.Expr == "" {
				continue
			}
			ids, err := computeexpr.Idents(f.Expr)
			if err != nil {
				continue
			}
			for _, id := range ids {
				if _, ok := st.Relevant[id]; !ok {
					st.Relevant[id] = struct{}{}
					changed = true
				}
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

// ErrSnapshotSource marks a write whose snapshot source row does not exist in
// the writer's organization (unknown id, other tenant, soft-deleted). The write
// is refused instead of leaving the frozen columns empty.
var ErrSnapshotSource = fmt.Errorf("%w: snapshot source row not found", ErrInvalidInput)

// StampPending are the grant redemptions a stamp validated but did not spend
// yet. They are redeemed with Redeem on the transaction of the write, AFTER the
// guards pass, so a refused / parked / rolled-back write burns nothing.
type StampPending []ConsumeInput

// Redeem spends every pending grant on db (the open transaction of the write).
func (p StampPending) Redeem(ctx context.Context, db *gorm.DB, consume func(context.Context, ConsumeInput) error) error {
	for _, in := range p {
		in.DB, in.DryRun = db, false
		if err := consume(ctx, in); err != nil {
			return err
		}
	}
	return nil
}

// StampCreate runs the whole stamp phase of a create on db, redeeming the
// grants right away. Use it where db IS the transaction of the INSERT (the
// wasm tier: a later failure rolls the redemption back with the write). Safe to
// call for a model without stamps (no-op). The row is mutated in place.
func (s *Service) StampCreate(ctx context.Context, db *gorm.DB, orgID, actor uuid.UUID, model string, row map[string]any) error {
	pend, err := s.StampCreatePrepare(ctx, db, orgID, actor, model, row)
	if err != nil {
		return err
	}
	return pend.Redeem(ctx, db, s.ConsumeApproval)
}

// StampCreatePrepare is StampCreate without the redemption: snapshots are
// copied, derived columns recomputed and each needed grant is VALIDATED (not
// spent). The caller redeems the result after its guards pass.
func (s *Service) StampCreatePrepare(ctx context.Context, db *gorm.DB, orgID, actor uuid.UUID, model string, row map[string]any) (StampPending, error) {
	st := s.resolveStamps(ctx, model)
	if st == nil {
		return nil, nil
	}
	return st.prepare(ctx, db, orgID, actor, model, row, "", s.ConsumeApproval)
}

// StampUpdate re-stamps an UPDATE of record `id` of `model`. before is the
// persisted row, changes the incoming patch (mutated in place; inc holds the
// additive deltas, folded into changes for the columns that matter). Server-
// owned columns never come from the client. When the patch touches a snapshot
// source the snapshot is re-taken; when it touches anything a grant covered
// (unit_price, product_id, ...) the authorization is dropped and a NEW grant id
// in the patch is required: the old one is spent and covered the old values.
func (s *Service) StampUpdate(ctx context.Context, db *gorm.DB, orgID, actor uuid.UUID, model, id string, before, changes, inc map[string]any) (StampPending, error) {
	st := s.resolveStamps(ctx, model)
	if st == nil {
		return nil, nil
	}
	return st.prepareUpdate(ctx, db, orgID, actor, model, id, before, changes, inc, s.ConsumeApproval)
}

// UpdateStamper returns StampUpdate shaped for wasm.Host.WithUpdateStamp. The
// grants are redeemed on tx, the transaction of the UPDATE.
func (s *Service) UpdateStamper() func(ctx context.Context, tx *gorm.DB, orgID uuid.UUID, model, id string, before, changes, inc map[string]any) error {
	return func(ctx context.Context, tx *gorm.DB, orgID uuid.UUID, model, id string, before, changes, inc map[string]any) error {
		actor, _ := uuid.Parse(ActorIDFromContext(ctx))
		pend, err := s.StampUpdate(ctx, tx, orgID, actor, model, id, before, changes, inc)
		if err != nil {
			return err
		}
		return pend.Redeem(ctx, tx, s.ConsumeApproval)
	}
}

// CreateStamper returns StampCreate shaped for wasm.Host.WithCreateStamp; the
// acting user is read from the invocation context.
func (s *Service) CreateStamper() func(ctx context.Context, tx *gorm.DB, orgID uuid.UUID, model string, row map[string]any) error {
	return func(ctx context.Context, tx *gorm.DB, orgID uuid.UUID, model string, row map[string]any) error {
		actor, _ := uuid.Parse(ActorIDFromContext(ctx))
		return s.StampCreate(ctx, tx, orgID, actor, model, row)
	}
}

func (st *ModelStamps) prepare(ctx context.Context, db *gorm.DB, orgID, actor uuid.UUID, model string, row map[string]any, recordID string, consume func(context.Context, ConsumeInput) error) (StampPending, error) {
	return st.stampRow(ctx, db, orgID, actor, model, row, recordID, true, nil, consume)
}

// apply is prepare + immediate redemption on db (kept for callers that stamp
// and write on the same handle).
func (st *ModelStamps) apply(ctx context.Context, db *gorm.DB, orgID, actor uuid.UUID, row map[string]any, consume func(context.Context, ConsumeInput) error) error {
	pend, err := st.prepare(ctx, db, orgID, actor, "", row, "", consume)
	if err != nil {
		return err
	}
	return pend.Redeem(ctx, db, consume)
}

// stampRow is the shared core. snapshots=true copies every snapshot (create, or
// an update whose source changed); otherwise only the listed ones in `only`.
func (st *ModelStamps) stampRow(ctx context.Context, db *gorm.DB, orgID, actor uuid.UUID, model string, row map[string]any, recordID string, allSnapshots bool, only map[string]bool, consume func(context.Context, ConsumeInput) error) (StampPending, error) {
	if err := st.applySnapshots(ctx, db, orgID, row, allSnapshots, only); err != nil {
		return nil, err
	}
	// Flags start closed; Derived is computed with them closed so context_gte
	// sees the real (uncovered) shortfall of the final row.
	for _, g := range st.Grants {
		row[g.Def.Sets] = 0.0
	}
	if err := st.applyDerived(ctx, row); err != nil {
		return nil, err
	}
	pend, err := st.applyGrants(ctx, db, orgID, actor, model, recordID, row, consume)
	if err != nil {
		return nil, err
	}
	if len(pend) > 0 {
		if err := st.applyDerived(ctx, row); err != nil {
			return nil, err
		}
	}
	return pend, nil
}

func (st *ModelStamps) applyDerived(ctx context.Context, row map[string]any) error {
	if len(st.Derived) == 0 {
		return nil
	}
	return applyFormulas(ctx, nil, formulaBinding{formulas: st.Derived}, row, nil)
}

func (st *ModelStamps) prepareUpdate(ctx context.Context, db *gorm.DB, orgID, actor uuid.UUID, model, id string, before, changes, inc map[string]any, consume func(context.Context, ConsumeInput) error) (StampPending, error) {
	// Server-owned columns never come from the client on an update either.
	for _, sn := range st.Snapshots {
		for _, dst := range sn.Fields {
			delete(changes, dst)
			delete(inc, dst)
		}
	}
	for _, g := range st.Grants {
		delete(changes, g.Def.Sets)
		delete(inc, g.Def.Sets)
	}
	// An additive delta on a relevant column is a change of that column.
	for col, d := range inc {
		if _, rel := st.Relevant[col]; rel {
			changes[col] = computeexpr.ToFloat(before[col]) + computeexpr.ToFloat(d)
			delete(inc, col)
		}
	}
	touched := false
	for col := range st.Relevant {
		if v, ok := changes[col]; ok && !sameStampValue(before[col], v) {
			touched = true
			break
		}
	}
	newGrant := false
	for _, g := range st.Grants {
		if grantPresented(before, changes, g.Column) {
			newGrant = true
		}
	}
	if !touched && !newGrant {
		return nil, nil
	}
	merged := make(map[string]any, len(before)+len(changes))
	for k, v := range before {
		merged[k] = v
	}
	for k, v := range changes {
		merged[k] = v
	}
	// Only a grant id presented in THIS patch can authorize: the one stored on
	// the row was spent by the write that created it.
	for _, g := range st.Grants {
		if !grantPresented(before, changes, g.Column) {
			// nil, not "": the column may be a uuid and "" is not a valid one
			// (invalid UUID length: 0 / Postgres 22P02). A spent grant is NULL.
			merged[g.Column] = nil
		}
	}
	// Snapshots are re-taken only for sources the patch changed; the rest stay
	// frozen at the value of their own moment.
	retake := map[string]bool{}
	for _, sn := range st.Snapshots {
		if v, ok := changes[sn.From]; ok && !sameStampValue(before[sn.From], v) {
			retake[sn.From] = true
		}
	}
	pend, err := st.stampRow(ctx, db, orgID, actor, model, merged, id, false, retake, consume)
	if err != nil {
		return nil, err
	}
	// Fold the server-computed columns back onto the patch.
	for _, sn := range st.Snapshots {
		if !retake[sn.From] {
			continue
		}
		for _, dst := range sn.Fields {
			if v, ok := merged[dst]; ok {
				changes[dst] = v
			}
		}
	}
	for _, g := range st.Grants {
		changes[g.Def.Sets] = merged[g.Def.Sets]
		changes[g.Column] = merged[g.Column]
	}
	for _, f := range st.Derived {
		if v, ok := merged[f.Target]; ok {
			changes[f.Target] = v
		}
	}
	return pend, nil
}

// grantPresented reports whether the patch presents a NEW grant id in col: a
// non-empty, non-zero id different from the one stored on the row. A full form
// that resends the stored (already spent) id is not a new grant: it is neither
// redeemed again nor does it keep an authorization whose covered values changed.
func grantPresented(before, changes map[string]any, col string) bool {
	v := valueToString(changes[col])
	if v == "" || v == uuid.Nil.String() {
		return false
	}
	return !sameStampValue(before[col], changes[col])
}

// sameStampValue compares a persisted value with an incoming one (numbers as
// numbers, everything else as trimmed text).
func sameStampValue(a, b any) bool {
	sa, sb := valueToString(a), valueToString(b)
	if sa == sb {
		return true
	}
	fa, ea := strconv.ParseFloat(sa, 64)
	fb, eb := strconv.ParseFloat(sb, 64)
	return ea == nil && eb == nil && fa == fb
}

func (st *ModelStamps) applySnapshots(ctx context.Context, db *gorm.DB, orgID uuid.UUID, row map[string]any, all bool, only map[string]bool) error {
	for _, sn := range st.Snapshots {
		if !all && !only[sn.From] {
			continue
		}
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
		// Tenant isolation is not optional: a snapshot NEVER reads without
		// organization_id = <writer's org>. A missing org or a source table that
		// cannot be scoped fails the write instead of copying a foreign row.
		if orgID == uuid.Nil {
			return ErrTenantScopeUnavailable
		}
		if !tableHasOrgID(db, table) {
			return fmt.Errorf("dynamic: snapshot source %s has no organization_id: refusing an unscoped copy", table)
		}
		q := db.WithContext(ctx).Table(table).Select(strings.Join(cols, ", ")).
			Where("id = ? AND organization_id = ?", id, orgID)
		// A soft-deleted source is not a source: its values are not current.
		if tableHasDeletedAt(db, table) {
			q = q.Where("deleted_at IS NULL")
		}
		var out map[string]any
		if err := q.Take(&out).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				// Fail closed: leaving the targets empty would let a forged or
				// foreign id skip every control that reads them (the price floor).
				return fmt.Errorf("%w: %s %q (%s)", ErrSnapshotSource, sn.From, id, table)
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

// applyGrants validates (does not spend) the grant of every column that needs
// one and opens its flag. The flags are 0 on entry.
func (st *ModelStamps) applyGrants(ctx context.Context, db *gorm.DB, orgID, actor uuid.UUID, model, recordID string, row map[string]any, consume func(context.Context, ConsumeInput) error) (StampPending, error) {
	var pend StampPending
	for _, g := range st.Grants {
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
				return nil, fmt.Errorf("dynamic: grant %q context_gte %q: %w", g.Column, k, err)
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
			return nil, fmt.Errorf("%w: %s is not an approval id", ErrApprovalConsume, g.Column)
		}
		eq := make(map[string]string, len(g.Def.ContextEq))
		for k, col := range g.Def.ContextEq {
			eq[k] = valueToString(row[col])
		}
		in := ConsumeInput{
			OrgID: orgID, ApprovalID: id, Policy: g.Def.Policy, RequestedBy: actor,
			ContextEq: eq, ContextGte: gte, ModelKey: model, RecordID: recordID,
			Declarative: true,
		}
		// Validate now (the guards read the flag); spend later, with the write.
		probe := in
		probe.DB, probe.DryRun = db, true
		if err := consume(ctx, probe); err != nil {
			return nil, err
		}
		row[g.Def.Sets] = 1.0
		pend = append(pend, in)
	}
	return pend, nil
}
