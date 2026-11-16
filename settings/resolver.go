package settings

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/manifest"
)

// Source says where an effective value came from.
type Source string

// Value sources, lowest precedence first.
const (
	SourceDefault Source = "default"
	SourceOrg     Source = "org"
	SourceEntity  Source = "entity"
	SourceBranch  Source = "branch"
	SourceMarket  Source = "market"
)

// Scope is the resolution context. An empty BranchID / MarketID means "no
// branch" / "no market": the org value applies.
type Scope struct {
	BranchID string
	MarketID string
	// EntityID is the entity (razón social, RFC 0001) the call acts for. Empty
	// = no entity layer: the resolution is exactly what it was before entities.
	EntityID string
}

// Effective is the resolved settings of one addon for one scope.
type Effective struct {
	// Values holds only keys that have a value (a default, an org value or a
	// branch override). Secret / write-only settings are never included.
	Values Values
	// Sources maps each key in Values to the layer it came from.
	Sources map[string]Source
	// OrgRev / BranchRev are the stored revisions of the layers read (0 = no
	// row).
	OrgRev    int64
	BranchRev int64
	MarketRev int64
	EntityRev int64
}

// Rev is a combined etag: it changes whenever either layer changes (each
// layer's revision only ever grows).
func (e Effective) Rev() int64 { return e.OrgRev + e.BranchRev + e.MarketRev + e.EntityRev }

// Resolver computes effective settings.
//
// Precedence (lowest to highest):
//
//	manifest default < org value < entity override < branch override < market override
//
// A setting has one scope, so for a given key only its own override layers are
// consulted: the branch layer applies only to keys the INSTALLED manifest
// declares with scope "branch" and only when Scope.BranchID is set; the market
// layer only to scope "market" keys and only when Scope.MarketID is set; the
// entity layer to scope "entity" keys and, as the inherited default of every
// branch of the entity, to scope "branch" keys, only when Scope.EntityID is set. A stored key the
// manifest no longer declares is ignored (logged), never an error. Defaults
// come from the manifest at resolve time and are never stored.
type Resolver struct {
	DB   *gorm.DB
	Defs DefSource
}

// NewResolver builds a Resolver.
func NewResolver(db *gorm.DB, defs DefSource) *Resolver {
	return &Resolver{DB: db, Defs: defs}
}

// Resolve returns the effective settings and their combined revision.
func (r *Resolver) Resolve(ctx context.Context, org uuid.UUID, addon string, scope Scope) (Effective, int64, error) {
	defs, err := r.Defs.Defs(ctx, org, addon)
	if err != nil {
		return Effective{}, 0, err
	}
	db := r.DB.WithContext(ctx)
	orgVals, orgRev, err := readRow(db, org, addon, ScopeOrg, "")
	if err != nil {
		return Effective{}, 0, err
	}
	var brVals Values
	var brRev int64
	if scope.BranchID != "" {
		brVals, brRev, err = readRow(db, org, addon, ScopeBranch, scope.BranchID)
		if err != nil {
			return Effective{}, 0, err
		}
	}

	var entVals Values
	var entRev int64
	if scope.EntityID != "" {
		entVals, entRev, err = readRow(db, org, addon, ScopeEntity, scope.EntityID)
		if err != nil {
			return Effective{}, 0, err
		}
	}

	var mkVals Values
	var mkRev int64
	if scope.MarketID != "" {
		mkVals, mkRev, err = readRow(db, org, addon, ScopeMarket, scope.MarketID)
		if err != nil {
			return Effective{}, 0, err
		}
	}

	declared := make(map[string]struct{}, len(defs))
	eff := Effective{Values: Values{}, Sources: map[string]Source{}, OrgRev: orgRev, BranchRev: brRev, MarketRev: mkRev, EntityRev: entRev}
	for _, d := range defs {
		declared[d.Key] = struct{}{}
		if sensitive(d) {
			continue
		}
		// Reuse the manifest's precedence rule instead of re-implementing it.
		v := manifest.ResolveSettingScopedEntity(d, orgVals, entVals, brVals, mkVals)
		if v == nil {
			continue
		}
		eff.Values[d.Key] = v
		switch {
		case d.Scope == manifest.SettingScopeMarket && present(mkVals, d.Key):
			eff.Sources[d.Key] = SourceMarket
		case d.Scope == manifest.SettingScopeBranch && present(brVals, d.Key):
			eff.Sources[d.Key] = SourceBranch
		case (d.Scope == manifest.SettingScopeBranch || d.Scope == manifest.SettingScopeEntity) && present(entVals, d.Key):
			eff.Sources[d.Key] = SourceEntity
		case present(orgVals, d.Key):
			eff.Sources[d.Key] = SourceOrg
		default:
			eff.Sources[d.Key] = SourceDefault
		}
	}
	warnUndeclared(addon, "org", orgVals, declared)
	warnUndeclared(addon, "entity", entVals, declared)
	warnUndeclared(addon, "branch", brVals, declared)
	warnUndeclared(addon, "market", mkVals, declared)
	return eff, eff.Rev(), nil
}

func present(m Values, k string) bool { v, ok := m[k]; return ok && v != nil }

var warnedUndeclared sync.Map

func warnUndeclared(addon, layer string, vals Values, declared map[string]struct{}) {
	for k := range vals {
		if _, ok := declared[k]; !ok && !strings.HasPrefix(k, "_") {
			// Once per (addon, layer, key) per process: Resolve runs on hot paths.
			if _, seen := warnedUndeclared.LoadOrStore(addon+"\x00"+layer+"\x00"+k, struct{}{}); !seen {
				slog.Warn("settings.resolve.undeclared_key_ignored", "addon_key", addon, "layer", layer, "key", k)
			}
		}
	}
}

// StringMap renders Values as the string map the wasm env_get import serves:
// strings verbatim, bools and numbers in canonical text, everything else as
// compact JSON.
func StringMap(v Values) map[string]string {
	out := make(map[string]string, len(v))
	for k, x := range v {
		switch t := x.(type) {
		case string:
			out[k] = t
		case bool:
			out[k] = strconv.FormatBool(t)
		case float64:
			out[k] = strconv.FormatFloat(t, 'f', -1, 64)
		case int, int32, int64:
			out[k] = fmt.Sprint(t)
		default:
			if b, err := json.Marshal(t); err == nil {
				out[k] = string(b)
			}
		}
	}
	return out
}

// entityOf is the entity the call acts for: the scope the host resolved, else
// the entity id it was asked for. "" when the call is not entity-scoped (the
// legacy path, where the resolution is unchanged).
func entityOf(ctx context.Context) string {
	if s, ok := dynamic.EntityScopeFromContext(ctx); ok && s.EntityID != "" {
		return s.EntityID
	}
	return dynamic.EntityIDFromContext(ctx)
}

// WasmResolver adapts a Resolver to the runtime/wasm SettingsResolver hook.
// Layering over the legacy per-invocation map (base):
//
//   - values that were explicitly STORED (org / branch layer) override base;
//   - manifest DEFAULTS only fill keys base does not have;
//   - keys only in base (secrets, system keys, not-yet-migrated values) pass
//     through untouched.
//
// That keeps F1/F2 safe while the legacy rows still hold the truth.
type WasmResolver struct{ R *Resolver }

// ResolveSettings implements wasm.SettingsResolver.
func (w WasmResolver) ResolveSettings(ctx context.Context, org uuid.UUID, addon, branchID string, base map[string]string) (map[string]string, error) {
	return w.ResolveSettingsInMarket(ctx, org, addon, branchID, "", base)
}

// ResolveSettingsInMarket implements wasm.MarketSettingsResolver: the same
// layering as ResolveSettings with the call's market override on top.
func (w WasmResolver) ResolveSettingsInMarket(ctx context.Context, org uuid.UUID, addon, branchID, marketID string, base map[string]string) (map[string]string, error) {
	eff, _, err := w.R.Resolve(ctx, org, addon, Scope{BranchID: branchID, MarketID: marketID, EntityID: entityOf(ctx)})
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(base)+len(eff.Values))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range StringMap(eff.Values) {
		if eff.Sources[k] == SourceDefault {
			if _, has := out[k]; has {
				continue
			}
		}
		out[k] = v
	}
	return out, nil
}
