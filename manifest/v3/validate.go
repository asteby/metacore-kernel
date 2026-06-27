package v3

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

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

// Dashboard widget enums, shared with the JSON schema so the struct-level
// checks and the schema agree byte-for-byte (the "dual validation" contract).
var (
	dashKinds = map[string]struct{}{
		"stat": {}, "bar": {}, "line": {}, "area": {}, "pie": {},
		"donut": {}, "list": {}, "progress": {}, "custom": {},
	}
	dashAggregates = map[string]struct{}{
		"count": {}, "sum": {}, "avg": {}, "min": {}, "max": {},
	}
	dashSizes   = map[string]struct{}{"sm": {}, "md": {}, "lg": {}, "full": {}}
	dashFormats = map[string]struct{}{
		"number": {}, "currency": {}, "percent": {}, "compact": {},
	}
)

// validateDashboard enforces the §1 cross-field rules of the dashboard-widget
// contract that the JSON schema cannot express (kind-conditional requirements).
// Each error is prefixed with the offending widget's key so an author can find
// it immediately. Returns a slice (possibly empty) so the caller can append.
func validateDashboard(m *Manifest) []string {
	if m.Contributions == nil || len(m.Contributions.Dashboard) == 0 {
		return nil
	}
	var errs []string
	seen := make(map[string]struct{}, len(m.Contributions.Dashboard))
	for i, w := range m.Contributions.Dashboard {
		id := w.Key
		if id == "" {
			id = fmt.Sprintf("#%d", i)
		}
		where := fmt.Sprintf("contributions.dashboard[%s]", id)

		if w.Key == "" {
			errs = append(errs, fmt.Sprintf("%s.key is required", where))
		} else if _, dup := seen[w.Key]; dup {
			errs = append(errs, fmt.Sprintf("%s.key %q is duplicated within the addon", where, w.Key))
		} else {
			seen[w.Key] = struct{}{}
		}
		if w.Title == "" {
			errs = append(errs, fmt.Sprintf("%s.title is required", where))
		}
		if w.Kind == "" {
			errs = append(errs, fmt.Sprintf("%s.kind is required", where))
			// Without a kind the conditional rules below are meaningless.
			continue
		}
		if _, ok := dashKinds[w.Kind]; !ok {
			errs = append(errs, fmt.Sprintf("%s.kind %q is not one of stat|bar|line|area|pie|donut|list|progress|custom", where, w.Kind))
		}
		if w.Size != "" {
			if _, ok := dashSizes[w.Size]; !ok {
				errs = append(errs, fmt.Sprintf("%s.size %q is not one of sm|md|lg|full", where, w.Size))
			}
		}
		if w.Format != "" {
			if _, ok := dashFormats[w.Format]; !ok {
				errs = append(errs, fmt.Sprintf("%s.format %q is not one of number|currency|percent|compact", where, w.Format))
			}
		}

		if w.Kind == "custom" {
			// Federated escape hatch: needs an expose + a frontend bundle.
			if w.Expose == "" {
				errs = append(errs, fmt.Sprintf("%s is kind=custom and must declare expose", where))
			}
			if m.Frontend == nil {
				errs = append(errs, fmt.Sprintf("%s is kind=custom and requires a top-level frontend block (federation)", where))
			}
			continue
		}

		// Declarative kinds: a query (with a model) is mandatory.
		if w.Query == nil {
			errs = append(errs, fmt.Sprintf("%s.query is required for kind=%s", where, w.Kind))
			continue
		}
		q := w.Query
		if q.Model == "" {
			errs = append(errs, fmt.Sprintf("%s.query.model is required", where))
		}
		agg := q.Aggregate
		if agg == "" {
			agg = "count"
		}
		if _, ok := dashAggregates[agg]; !ok {
			errs = append(errs, fmt.Sprintf("%s.query.aggregate %q is not one of count|sum|avg|min|max", where, q.Aggregate))
		}
		if agg != "count" && q.Field == "" {
			errs = append(errs, fmt.Sprintf("%s.query.field is required when aggregate=%s", where, agg))
		}
		switch w.Kind {
		case "bar", "pie", "donut", "list":
			if q.GroupBy == "" {
				errs = append(errs, fmt.Sprintf("%s.query.group_by is required for kind=%s", where, w.Kind))
			}
		case "line", "area":
			if q.DateField == "" {
				errs = append(errs, fmt.Sprintf("%s.query.date_field is required for kind=%s", where, w.Kind))
			}
			if q.Interval == "" {
				errs = append(errs, fmt.Sprintf("%s.query.interval is required for kind=%s", where, w.Kind))
			}
		}
		if w.Compare != nil && (q.DateField == "" || q.Range == "") {
			errs = append(errs, fmt.Sprintf("%s.compare requires query.date_field and query.range", where))
		}
	}
	return errs
}

// validHookPrefixes is the allowlist of TransitionHook.Do dispatch targets,
// shared with the JSON schema pattern so the struct-level checks and the schema
// agree (the "dual validation" contract).
var validHookPrefixes = map[string]struct{}{
	"wasm": {}, "webhook": {}, "compiled": {},
}

// validateStageMachine enforces the cross-field invariants of a model's stage
// machine that the JSON schema cannot express: stage_field names a real column,
// stage keys are unique, transitions reference declared stages, and each
// on_transition hook matches declared stages (or "*") and carries a valid
// wasm:/webhook:/compiled: `do`. Returns a slice (possibly empty) prefixed with
// the offending model's index so the caller can append. Empty stages is a
// no-op (the model has no stage machine).
func validateStageMachine(mi int, mod Model, ownCols map[string]struct{}) []string {
	if len(mod.Stages) == 0 && mod.StageField == "" && len(mod.Transitions) == 0 && len(mod.OnTransition) == 0 {
		return nil
	}
	var errs []string
	where := fmt.Sprintf("models[%d]", mi)

	// stage_field is mandatory once any stage-machine field is declared, and
	// must name a declared column.
	if mod.StageField == "" {
		errs = append(errs, fmt.Sprintf("%s declares stages/transitions/on_transition but no stage_field", where))
	} else if _, ok := ownCols[mod.StageField]; !ok {
		errs = append(errs, fmt.Sprintf("%s.stage_field %q is not a declared column on the model", where, mod.StageField))
	}

	// Stage keys: non-empty and unique.
	stageKeys := make(map[string]struct{}, len(mod.Stages))
	for si, st := range mod.Stages {
		if st.Key == "" {
			errs = append(errs, fmt.Sprintf("%s.stages[%d].key is empty", where, si))
			continue
		}
		if _, dup := stageKeys[st.Key]; dup {
			errs = append(errs, fmt.Sprintf("%s.stages[%d].key %q is duplicated", where, si, st.Key))
		}
		stageKeys[st.Key] = struct{}{}
	}
	if len(mod.Stages) == 0 && mod.StageField != "" {
		errs = append(errs, fmt.Sprintf("%s declares stage_field %q but no stages", where, mod.StageField))
	}

	// Transitions: from/to must reference declared stage keys.
	for ti, t := range mod.Transitions {
		if _, ok := stageKeys[t.From]; !ok {
			errs = append(errs, fmt.Sprintf("%s.transitions[%d].from %q is not a declared stage key", where, ti, t.From))
		}
		if _, ok := stageKeys[t.To]; !ok {
			errs = append(errs, fmt.Sprintf("%s.transitions[%d].to %q is not a declared stage key", where, ti, t.To))
		}
	}

	// Hooks: from/to must be "*" or a declared stage key; `do` must carry a
	// known prefix (the schema pattern enforces the shape, this enforces the set).
	for hi, h := range mod.OnTransition {
		if h.From != "*" && h.From != "" {
			if _, ok := stageKeys[h.From]; !ok {
				errs = append(errs, fmt.Sprintf("%s.on_transition[%d].from %q is not a declared stage key (or \"*\")", where, hi, h.From))
			}
		}
		if h.To != "*" {
			if _, ok := stageKeys[h.To]; !ok {
				errs = append(errs, fmt.Sprintf("%s.on_transition[%d].to %q is not a declared stage key (or \"*\")", where, hi, h.To))
			}
		}
		prefix, _, found := strings.Cut(h.Do, ":")
		if !found {
			errs = append(errs, fmt.Sprintf("%s.on_transition[%d].do %q must be wasm:<export> | webhook:<key> | compiled:<fn>", where, hi, h.Do))
		} else if _, ok := validHookPrefixes[prefix]; !ok {
			errs = append(errs, fmt.Sprintf("%s.on_transition[%d].do %q has an unknown prefix (want wasm|webhook|compiled)", where, hi, prefix))
		}
	}
	return errs
}

// validateNavViewTypes enforces that a kanban nav entry declares the group_by
// column it groups its board by. view_type is otherwise free (hosts fall back
// to a table for an unknown value). Walks the nested NavItem tree.
func validateNavViewTypes(m *Manifest) []string {
	if m.Contributions == nil {
		return nil
	}
	var errs []string
	var walk func(items []NavItem, path string)
	walk = func(items []NavItem, path string) {
		for i, it := range items {
			where := fmt.Sprintf("%s[%d]", path, i)
			if it.ViewType == "kanban" && it.GroupBy == "" {
				errs = append(errs, fmt.Sprintf("%s declares view_type=kanban but no group_by", where))
			}
			if len(it.Items) > 0 {
				walk(it.Items, where+".items")
			}
		}
	}
	for gi, g := range m.Contributions.Navigation {
		walk(g.Items, fmt.Sprintf("contributions.navigation[%d].items", gi))
	}
	return errs
}

// validateDoRef checks a Schedule/InboundWebhook `do` carries a known
// wasm:/webhook:/compiled: dispatch prefix (the schema pattern enforces the
// shape; this enforces the set, mirroring validateStageMachine). Returns an
// error string suffix or "" when valid.
func validateDoRef(do string) string {
	prefix, _, found := strings.Cut(do, ":")
	if !found {
		return fmt.Sprintf("%q must be wasm:<export> | webhook:<key> | compiled:<fn>", do)
	}
	if _, ok := validHookPrefixes[prefix]; !ok {
		return fmt.Sprintf("%q has an unknown prefix (want wasm|webhook|compiled)", do)
	}
	return ""
}

// validatePipelineRuntime enforces the cross-field invariants of the addon-level
// pipeline-runtime primitives (connectors / schedules / webhooks) that the JSON
// schema cannot express: connector keys are unique; a schedule's `every` parses
// as a Go duration and its `do` carries a known prefix; a webhook's `do` carries
// a known prefix and, when it declares a `verify`, it supplies a `secret_ref`
// that resolves to a declared connector credential. Empty blocks are a no-op
// (the addon has no runtime primitives — the back-compat default).
func validatePipelineRuntime(m *Manifest) []string {
	if len(m.Connectors) == 0 && len(m.Schedules) == 0 && len(m.Webhooks) == 0 {
		return nil
	}
	var errs []string

	// Connectors: unique keys; index their credential keys for secret_ref checks.
	connectorCreds := make(map[string]map[string]struct{}, len(m.Connectors))
	seenConn := make(map[string]struct{}, len(m.Connectors))
	for ci, c := range m.Connectors {
		if c.Key == "" {
			errs = append(errs, fmt.Sprintf("connectors[%d].key is empty", ci))
			continue
		}
		if _, dup := seenConn[c.Key]; dup {
			errs = append(errs, fmt.Sprintf("connectors[%d].key %q is duplicated", ci, c.Key))
		}
		seenConn[c.Key] = struct{}{}
		creds := make(map[string]struct{}, len(c.Credentials))
		for _, cr := range c.Credentials {
			creds[cr.Key] = struct{}{}
		}
		connectorCreds[c.Key] = creds
	}

	// Schedules: unique keys; every parses; do prefix known.
	seenSched := make(map[string]struct{}, len(m.Schedules))
	for si, s := range m.Schedules {
		if s.Key == "" {
			errs = append(errs, fmt.Sprintf("schedules[%d].key is empty", si))
		} else {
			if _, dup := seenSched[s.Key]; dup {
				errs = append(errs, fmt.Sprintf("schedules[%d].key %q is duplicated", si, s.Key))
			}
			seenSched[s.Key] = struct{}{}
		}
		if d, err := time.ParseDuration(s.Every); err != nil || d <= 0 {
			errs = append(errs, fmt.Sprintf("schedules[%d].every %q is not a positive Go duration (e.g. \"30s\", \"5m\")", si, s.Every))
		}
		if msg := validateDoRef(s.Do); msg != "" {
			errs = append(errs, fmt.Sprintf("schedules[%d].do %s", si, msg))
		}
	}

	// Webhooks: unique keys + paths; do prefix known; verify ⇒ secret_ref → a
	// declared connector credential.
	seenHook := make(map[string]struct{}, len(m.Webhooks))
	seenPath := make(map[string]struct{}, len(m.Webhooks))
	for wi, w := range m.Webhooks {
		if w.Key == "" {
			errs = append(errs, fmt.Sprintf("webhooks[%d].key is empty", wi))
		} else {
			if _, dup := seenHook[w.Key]; dup {
				errs = append(errs, fmt.Sprintf("webhooks[%d].key %q is duplicated", wi, w.Key))
			}
			seenHook[w.Key] = struct{}{}
		}
		if w.Path == "" {
			errs = append(errs, fmt.Sprintf("webhooks[%d].path is empty", wi))
		} else {
			if _, dup := seenPath[w.Path]; dup {
				errs = append(errs, fmt.Sprintf("webhooks[%d].path %q is duplicated", wi, w.Path))
			}
			seenPath[w.Path] = struct{}{}
		}
		if msg := validateDoRef(w.Do); msg != "" {
			errs = append(errs, fmt.Sprintf("webhooks[%d].do %s", wi, msg))
		}
		if w.Verify != "" {
			if w.SecretRef == "" {
				errs = append(errs, fmt.Sprintf("webhooks[%d] declares verify=%q but no secret_ref", wi, w.Verify))
			} else {
				conn, cred, ok := strings.Cut(w.SecretRef, ".")
				if !ok {
					errs = append(errs, fmt.Sprintf("webhooks[%d].secret_ref %q must be \"<connector>.<credential>\"", wi, w.SecretRef))
				} else if creds, ok := connectorCreds[conn]; !ok {
					errs = append(errs, fmt.Sprintf("webhooks[%d].secret_ref %q references undeclared connector %q", wi, w.SecretRef, conn))
				} else if _, ok := creds[cred]; !ok {
					errs = append(errs, fmt.Sprintf("webhooks[%d].secret_ref %q references undeclared credential %q on connector %q", wi, w.SecretRef, cred, conn))
				}
			}
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
		// Stage machine: stage_field must name a declared column; stage keys
		// unique; transitions/hooks must reference declared stage keys (or "*"
		// for hooks); a hook's `do` carries a wasm:/webhook:/compiled: prefix
		// (the schema pattern already enforces the shape). Mirrors the legacy
		// validator so a manifest fails identically on both surfaces.
		errs = append(errs, validateStageMachine(mi, mod, ownCols)...)
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
		errs = append(errs, validateDashboard(&m)...)
		errs = append(errs, validateNavViewTypes(&m)...)
	}

	// Addon-level pipeline-runtime primitives (connectors / schedules / webhooks).
	errs = append(errs, validatePipelineRuntime(&m)...)

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
