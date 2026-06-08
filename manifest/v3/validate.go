package v3

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Masterminds/semver/v3"
	"github.com/asteby/metacore-kernel/manifest/computeexpr"
	"github.com/santhosh-tekuri/jsonschema/v5"
)

// validFns is the rollup aggregate-function allowlist, shared with the legacy
// validator via the schema enum. count ignores from/expr.
var validFns = map[string]struct{}{
	"sum": {}, "count": {}, "avg": {}, "min": {}, "max": {},
}

// validateArithExpr parses a Tier-2 formula / Tier-1 rollup expression under
// the strict arithmetic allowlist and verifies every identifier is a column in
// `cols`. Returns nil on success.
func validateArithExpr(expr string, cols map[string]struct{}) error {
	return computeexpr.Validate(expr, cols)
}

// validateRollupSpec checks one Tier-1 rollup: target on the PARENT (ownCols),
// fn in the enum, from on the CHILD (childCols), exactly one of from/expr
// (count may omit both), expr under the arithmetic allowlist against childCols.
// Returns a slice of human-readable errors prefixed with `where`.
func validateRollupSpec(where string, r Rollup, ownCols, childCols map[string]struct{}) []string {
	var errs []string
	if r.Target == "" {
		errs = append(errs, fmt.Sprintf("%s.target is empty", where))
	} else if _, ok := ownCols[r.Target]; !ok {
		errs = append(errs, fmt.Sprintf("%s.target %q is not a declared column on the parent model", where, r.Target))
	}
	fn := strings.ToLower(strings.TrimSpace(r.Fn))
	if fn == "" {
		fn = "sum"
	}
	if _, ok := validFns[fn]; !ok {
		errs = append(errs, fmt.Sprintf("%s.fn %q is not one of sum|count|avg|min|max", where, r.Fn))
	}
	hasFrom := strings.TrimSpace(r.From) != ""
	hasExpr := strings.TrimSpace(r.Expr) != ""
	if hasFrom && hasExpr {
		errs = append(errs, fmt.Sprintf("%s declares both from and expr (use exactly one)", where))
	}
	if fn != "count" && !hasFrom && !hasExpr {
		errs = append(errs, fmt.Sprintf("%s.fn=%s requires either from or expr", where, fn))
	}
	if hasFrom {
		if !computeexpr.IdentRe.MatchString(r.From) {
			errs = append(errs, fmt.Sprintf("%s.from %q is not a valid column identifier", where, r.From))
		} else if _, ok := childCols[r.From]; !ok {
			errs = append(errs, fmt.Sprintf("%s.from %q is not a declared column on the child model", where, r.From))
		}
	}
	if hasExpr {
		if err := validateArithExpr(r.Expr, childCols); err != nil {
			errs = append(errs, fmt.Sprintf("%s.expr %q: %v", where, r.Expr, err))
		}
	}
	return errs
}

//go:embed schema/manifest-v3.schema.json
var schemaBytes []byte

var compiledSchema *jsonschema.Schema

func init() {
	c := jsonschema.NewCompiler()
	c.Draft = jsonschema.Draft2020
	if err := c.AddResource("manifest-v3.schema.json", strings.NewReader(string(schemaBytes))); err != nil {
		panic(fmt.Errorf("v3: embed schema add resource: %w", err))
	}
	s, err := c.Compile("manifest-v3.schema.json")
	if err != nil {
		panic(fmt.Errorf("v3: compile embedded schema: %w", err))
	}
	compiledSchema = s
}

// SchemaJSON returns the embedded JSON schema bytes. Useful for tools that
// want to surface the schema (CLI, docs site, IDE plugins).
func SchemaJSON() []byte {
	out := make([]byte, len(schemaBytes))
	copy(out, schemaBytes)
	return out
}

// Validate parses raw as a v3 manifest, runs it through the JSON schema and
// then enforces the cross-field invariants the schema cannot express:
//
//   - apiVersion must equal APIVersion
//   - every compatibility.requires[].version must be a valid semver range
//   - every lifecycle.upgrade[].from must be a valid semver range
//   - kind=Preset must not declare models or own lifecycle
//   - kind=Addon must not declare a preset block
//
// On failure the returned error wraps a list of all violations so authors
// get the full picture in a single round trip.
func Validate(raw []byte) error {
	if len(raw) == 0 {
		return errors.New("v3: manifest is empty")
	}

	// Schema check first; if the doc is structurally broken the rest is
	// pointless.
	var asAny interface{}
	if err := json.Unmarshal(raw, &asAny); err != nil {
		return fmt.Errorf("v3: invalid JSON: %w", err)
	}
	if err := compiledSchema.Validate(asAny); err != nil {
		return fmt.Errorf("v3: schema validation failed: %w", err)
	}

	// Decode into the typed shape for cross-field checks.
	var m Manifest
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return fmt.Errorf("v3: decode into typed manifest: %w", err)
	}

	var errs []string

	if m.APIVersion != APIVersion {
		errs = append(errs, fmt.Sprintf("apiVersion must be %q, got %q", APIVersion, m.APIVersion))
	}

	switch m.Kind {
	case KindAddon, KindPreset, KindTheme, KindConnectorPack:
		// known
	default:
		errs = append(errs, fmt.Sprintf("kind %q is not one of Addon|Preset|Theme|ConnectorPack", m.Kind))
	}

	for i, r := range m.Compatibility.Requires {
		if _, err := semver.NewConstraint(r.Version); err != nil {
			errs = append(errs, fmt.Sprintf("compatibility.requires[%d].version %q is not a valid semver range: %v", i, r.Version, err))
		}
		if r.Key == "" {
			errs = append(errs, fmt.Sprintf("compatibility.requires[%d].key is empty", i))
		}
	}

	if m.Lifecycle != nil {
		for i, step := range m.Lifecycle.Upgrade {
			if _, err := semver.NewConstraint(step.From); err != nil {
				errs = append(errs, fmt.Sprintf("lifecycle.upgrade[%d].from %q is not a valid semver range: %v", i, step.From, err))
			}
		}
	}

	switch m.Kind {
	case KindAddon:
		if m.Preset != nil {
			errs = append(errs, "kind=Addon must not declare a preset block")
		}
	case KindPreset:
		if m.Preset == nil {
			errs = append(errs, "kind=Preset requires a preset block")
		}
		if len(m.Models) > 0 {
			errs = append(errs, "kind=Preset must not declare models[]")
		}
		if m.Lifecycle != nil {
			errs = append(errs, "kind=Preset must not declare a lifecycle block")
		}
	case KindTheme:
		if m.Theme == nil {
			errs = append(errs, "kind=Theme requires a theme block")
		}
	case KindConnectorPack:
		if m.ConnectorPack == nil {
			errs = append(errs, "kind=ConnectorPack requires a connector_pack block")
		}
	}

	// Index every model's declared columns by model KEY so rollup/formula
	// cross-field checks can resolve targets/identifiers on the PARENT (the
	// model owning the relation) and on the CHILD (relation.Through). Built
	// once for the whole manifest.
	colsByModel := make(map[string]map[string]struct{}, len(m.Models))
	for _, mod := range m.Models {
		set := make(map[string]struct{}, len(mod.Columns))
		for _, c := range mod.Columns {
			set[c.Name] = struct{}{}
		}
		colsByModel[mod.Key] = set
	}

	for mi, mod := range m.Models {
		ownCols := colsByModel[mod.Key]
		// Tier-2 formulas: target + every identifier in expr must be a column
		// on THIS model; expr must pass the strict arithmetic allowlist.
		for fi, f := range mod.Formulas {
			where := fmt.Sprintf("models[%d].formulas[%d]", mi, fi)
			if f.Target == "" {
				errs = append(errs, fmt.Sprintf("%s.target is empty", where))
			} else if _, ok := ownCols[f.Target]; !ok {
				errs = append(errs, fmt.Sprintf("%s.target %q is not a declared column on the model", where, f.Target))
			}
			if strings.TrimSpace(f.Expr) == "" {
				errs = append(errs, fmt.Sprintf("%s.expr is empty", where))
			} else if err := validateArithExpr(f.Expr, ownCols); err != nil {
				errs = append(errs, fmt.Sprintf("%s.expr %q: %v", where, f.Expr, err))
			}
		}
		if mod.Seed != nil {
			where := fmt.Sprintf("models[%d].seed", mi)
			if mod.Seed.Key == "" {
				errs = append(errs, fmt.Sprintf("%s.key is empty", where))
			} else {
				known := false
				for _, c := range mod.Columns {
					if c.Name == mod.Seed.Key {
						known = true
						break
					}
				}
				if !known {
					errs = append(errs, fmt.Sprintf("%s.key %q is not a declared column on the model", where, mod.Seed.Key))
				}
			}
			if len(mod.Seed.Rows) == 0 {
				errs = append(errs, fmt.Sprintf("%s.rows is empty", where))
			}
			for rri, row := range mod.Seed.Rows {
				if len(row) == 0 {
					errs = append(errs, fmt.Sprintf("%s.rows[%d] is an empty object", where, rri))
				}
			}
		}
		for ri, rel := range mod.Relations {
			where := fmt.Sprintf("models[%d].relations[%d]", mi, ri)
			switch rel.Kind {
			case "one_to_many", "many_to_many":
				// known
			default:
				errs = append(errs, fmt.Sprintf("%s.kind %q is not one of one_to_many|many_to_many", where, rel.Kind))
			}
			if rel.Name == "" {
				errs = append(errs, fmt.Sprintf("%s.name is empty", where))
			}
			if rel.Through == "" {
				errs = append(errs, fmt.Sprintf("%s.through is empty", where))
			}
			if rel.ForeignKey == "" {
				errs = append(errs, fmt.Sprintf("%s.foreign_key is empty", where))
			}
			// Tier-1 rollups: target must be a column on the PARENT (this
			// model); from (if present) must be a column on the CHILD
			// (relation.Through); fn must be in the enum; exactly one of
			// from/expr (count may omit both); expr must pass the strict
			// arithmetic allowlist against the CHILD's columns.
			childCols := colsByModel[rel.Through]
			for ki, rl := range rel.Rollups {
				rw := fmt.Sprintf("%s.rollups[%d]", where, ki)
				errs = append(errs, validateRollupSpec(rw, rl, ownCols, childCols)...)
			}
		}
	}

	if m.Contributions != nil {
		for ai, a := range m.Contributions.Actions {
			for fi, f := range a.Fields {
				if f.Balance == nil {
					continue
				}
				where := fmt.Sprintf("contributions.actions[%d].fields[%d]", ai, fi)
				if len(f.ItemFields) == 0 {
					errs = append(errs, fmt.Sprintf("%s declares a balance rule but has no item_fields (balance only applies to a line-items array field)", where))
					continue
				}
				cols := map[string]struct{}{}
				for _, it := range f.ItemFields {
					cols[it.Key] = struct{}{}
				}
				if f.Balance.DebitColumn == "" || f.Balance.CreditColumn == "" {
					errs = append(errs, fmt.Sprintf("%s.balance requires both debit_column and credit_column", where))
				}
				if f.Balance.DebitColumn != "" {
					if _, ok := cols[f.Balance.DebitColumn]; !ok {
						errs = append(errs, fmt.Sprintf("%s.balance.debit_column %q is not one of the field's item_fields", where, f.Balance.DebitColumn))
					}
				}
				if f.Balance.CreditColumn != "" {
					if _, ok := cols[f.Balance.CreditColumn]; !ok {
						errs = append(errs, fmt.Sprintf("%s.balance.credit_column %q is not one of the field's item_fields", where, f.Balance.CreditColumn))
					}
				}
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("v3: manifest validation failed:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

// Parse runs Validate and, on success, returns the typed manifest.
func Parse(raw []byte) (*Manifest, error) {
	if err := Validate(raw); err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("v3: decode: %w", err)
	}
	return &m, nil
}
