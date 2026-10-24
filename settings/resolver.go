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

	"github.com/asteby/metacore-kernel/manifest"
)

// Source says where an effective value came from.
type Source string

// Value sources, lowest precedence first.
const (
	SourceDefault Source = "default"
	SourceOrg     Source = "org"
	SourceBranch  Source = "branch"
)

// Scope is the resolution context. An empty BranchID means "no branch": the
// org value applies.
type Scope struct {
	BranchID string
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
}

// Rev is a combined etag: it changes whenever either layer changes (each
// layer's revision only ever grows).
func (e Effective) Rev() int64 { return e.OrgRev + e.BranchRev }

// Resolver computes effective settings.
//
// Precedence (lowest to highest):
//
//	manifest default < org value < branch override
//
// The branch layer applies only to keys the INSTALLED manifest declares with
// scope "branch" and only when Scope.BranchID is set. A stored key the
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

	declared := make(map[string]struct{}, len(defs))
	eff := Effective{Values: Values{}, Sources: map[string]Source{}, OrgRev: orgRev, BranchRev: brRev}
	for _, d := range defs {
		declared[d.Key] = struct{}{}
		if sensitive(d) {
			continue
		}
		// Reuse the manifest's precedence rule instead of re-implementing it.
		v := manifest.ResolveSetting(d, orgVals, brVals)
		if v == nil {
			continue
		}
		eff.Values[d.Key] = v
		switch {
		case d.Scope == manifest.SettingScopeBranch && present(brVals, d.Key):
			eff.Sources[d.Key] = SourceBranch
		case present(orgVals, d.Key):
			eff.Sources[d.Key] = SourceOrg
		default:
			eff.Sources[d.Key] = SourceDefault
		}
	}
	warnUndeclared(addon, "org", orgVals, declared)
	warnUndeclared(addon, "branch", brVals, declared)
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
	eff, _, err := w.R.Resolve(ctx, org, addon, Scope{BranchID: branchID})
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
