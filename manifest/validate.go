package manifest

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/Masterminds/semver/v3"
)

var (
	keyRe    = regexp.MustCompile(`^[a-z][a-z0-9_]{1,63}$`)
	modelRe  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	columnRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)
	// customValidatorRe matches "<namespace>.<symbol>" identifiers used by
	// ValidationRule.Custom — keeps it injection-safe for log lines and
	// future router lookups.
	customValidatorRe = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)
	// validVisibility is the closed set of ColumnDef.Visibility values.
	// Empty string is also accepted at the call site as the legacy default.
	validVisibility = map[string]struct{}{
		"all":   {},
		"table": {},
		"modal": {},
		"list":  {},
	}
	// validRelationKinds lists the shapes RelationDef supports today.
	// New cardinalities (one_to_one, polymorphic) extend this map without
	// touching the surrounding loop — the discriminator stays stable.
	validRelationKinds = map[string]struct{}{
		"one_to_many":  {},
		"many_to_many": {},
	}
	// validTriggerTypes lists the dispatch shapes ActionTrigger supports.
	// The set is closed: addon authors that need a custom dispatcher pick
	// "wasm" (and ship the implementation as an exported function) rather
	// than minting a new type.
	validTriggerTypes = map[string]struct{}{
		"wasm":    {},
		"webhook": {},
		"noop":    {},
	}
	// triggerExportRe matches a wasm export symbol. Same alphabet as a Go
	// identifier (lower/upper letters, digits, underscore) so the validator
	// can be used identically against TinyGo, Rust and AssemblyScript
	// outputs. Re-declared (instead of reusing columnRe) because export
	// names commonly start with uppercase or are CamelCase.
	triggerExportRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	// relationNameRe and pivotRe match the same alphabet as columnRe /
	// modelRe respectively. Re-declared as aliases so the relation
	// validator reads cleanly and a future tweak to the relation alphabet
	// does not silently widen unrelated identifiers.
	relationNameRe = columnRe
	pivotRe        = modelRe
	// validWidgets enumerates the widget slugs the UI knows how to render.
	// Kept as a map so adding entries is cheap; addons that need a custom
	// widget can ship it via a federated module and pick a slug we extend
	// this list with — it is not meant to gate addon innovation, just to
	// catch typos and reject undefined values at install time.
	validWidgets = map[string]struct{}{
		"text":         {},
		"textarea":     {},
		"select":       {},
		"multi_select": {},
		"search":       {},
		"number":       {},
		"date":         {},
		"datetime":     {},
		"email":        {},
		"url":          {},
		"boolean":      {},
		"image":        {},
		"file":         {},
		"richtext":     {},
		"json":         {},
		"relation":     {},
		"password":     {},
		"slider":       {},
		"rating":       {},
	}
	// defaultRe allows only safe DDL DEFAULT expressions:
	//   numeric literal:   42 | 42.5 | -3
	//   quoted string:     'pending' (no embedded quotes or semicolons)
	//   builtin function:  now() | gen_random_uuid() | uuid_generate_v4() | true | false | null
	defaultRe = regexp.MustCompile(
		`^(` +
			`-?\d+(\.\d+)?` + // numeric
			`|'[^'";\\]*'` + // simple quoted string
			`|now\(\)|gen_random_uuid\(\)|uuid_generate_v4\(\)|current_timestamp` +
			`|true|false|null` +
			`)$`)
)

// Validate performs a full structural + semantic check of the manifest.
// It is cheap and side-effect free; callers should run it before install.
func (m *Manifest) Validate(kernelVersion string) error {
	if m == nil {
		return fmt.Errorf("manifest: nil")
	}
	if !keyRe.MatchString(m.Key) {
		return fmt.Errorf("manifest: invalid key %q", m.Key)
	}
	if strings.TrimSpace(m.Name) == "" {
		return fmt.Errorf("manifest: name required")
	}
	if _, err := semver.NewVersion(m.Version); err != nil {
		return fmt.Errorf("manifest: version %q is not semver: %w", m.Version, err)
	}
	if err := m.checkKernelRange(kernelVersion); err != nil {
		return err
	}
	for i, md := range m.ModelDefinitions {
		if !modelRe.MatchString(md.TableName) {
			return fmt.Errorf("manifest.model_definitions[%d]: invalid table_name %q", i, md.TableName)
		}
		if md.ModelKey == "" {
			return fmt.Errorf("manifest.model_definitions[%d]: model_key required", i)
		}
		if len(md.Columns) == 0 {
			return fmt.Errorf("manifest.model_definitions[%d]: columns required", i)
		}
		for j, col := range md.Columns {
			// Block SQL injection via column name — the DDL generator uses
			// %q which does not escape embedded quotes per Postgres rules.
			if !columnRe.MatchString(col.Name) {
				return fmt.Errorf("manifest.model_definitions[%d].columns[%d]: invalid name %q", i, j, col.Name)
			}
			// Default goes raw into `DEFAULT <expr>` — whitelist literals
			// across the union type (string | number | bool | nil).
			if _, ok := DefaultLiteral(col.Default); !ok {
				return fmt.Errorf("manifest.model_definitions[%d].columns[%d].default: %v not allowed (use numeric, 'quoted' literal, now(), gen_random_uuid(), true, false, null)", i, j, col.Default)
			}
			if err := validateColumnExtensions(col); err != nil {
				return fmt.Errorf("manifest.model_definitions[%d].columns[%d]: %w", i, j, err)
			}
		}
		if err := validateRelations(md.Relations); err != nil {
			return fmt.Errorf("manifest.model_definitions[%d].%w", i, err)
		}
	}
	for i, c := range m.Capabilities {
		if !strings.Contains(c.Kind, ":") {
			return fmt.Errorf("manifest.capabilities[%d]: kind must be namespaced (e.g. db:read)", i)
		}
		if c.Target == "" {
			return fmt.Errorf("manifest.capabilities[%d]: target required", i)
		}
		// Bare `*` would grant access to everything — including link-local
		// metadata addresses (169.254.169.254), loopback, and private
		// ranges. Require a concrete host segment for egress permissions.
		if c.Kind == "http:fetch" {
			if c.Target == "*" || c.Target == "*.*" || strings.HasPrefix(c.Target, "*.") && !strings.Contains(strings.TrimPrefix(c.Target, "*."), ".") {
				return fmt.Errorf("manifest.capabilities[%d].target: %q is too broad for http:fetch (require a concrete TLD like api.example.com or *.example.com)", i, c.Target)
			}
		}
		if c.Target == "*" && (c.Kind == "db:read" || c.Kind == "db:write") {
			return fmt.Errorf("manifest.capabilities[%d].target: wildcard %q not allowed for %s — declare explicit model names", i, c.Target, c.Kind)
		}
	}
	if err := m.validateBackend(); err != nil {
		return err
	}
	if err := m.validateActionTriggers(); err != nil {
		return err
	}
	if m.Frontend != nil {
		switch m.Frontend.Format {
		case "federation", "script", "":
			// ok
		default:
			return fmt.Errorf("manifest.frontend.format: unknown %q", m.Frontend.Format)
		}
	}
	return nil
}

// validateBackend enforces the runtime whitelist and — for wasm — that each
// dispatchable hook resolves to an exported function name. Keeping the check
// here (not in the wasm runtime) means validation stays side-effect free and
// catches misconfigured manifests before we even load any bytes.
func (m *Manifest) validateBackend() error {
	if m.Backend == nil {
		return nil
	}
	switch m.Backend.Runtime {
	case "webhook", "wasm", "binary":
		// ok
	default:
		return fmt.Errorf("manifest.backend.runtime: unknown %q (want webhook|wasm|binary)", m.Backend.Runtime)
	}
	if m.Backend.Runtime == "wasm" {
		if strings.TrimSpace(m.Backend.Entry) == "" {
			return fmt.Errorf("manifest.backend.entry: required when runtime=wasm")
		}
		exports := make(map[string]struct{}, len(m.Backend.Exports))
		for _, e := range m.Backend.Exports {
			exports[e] = struct{}{}
		}
		for hookKey := range m.Hooks {
			// hookKey format: "<model>::<action>" — the action half must be
			// exported so the wasm host can resolve it at dispatch time.
			parts := strings.SplitN(hookKey, "::", 2)
			if len(parts) != 2 {
				continue
			}
			action := parts[1]
			if _, ok := exports[action]; !ok {
				return fmt.Errorf("manifest.hooks[%q]: action %q is not listed in backend.exports", hookKey, action)
			}
		}
	}
	return nil
}

// validateActionTriggers walks every ActionDef carried by the manifest
// (the Actions map keyed by model and the Actions slice on each
// ModelExtension) and enforces ActionTrigger.validate against the union of
// exports declared by Backend.Exports. The Backend exports set is hoisted
// once so the per-trigger checks stay O(triggers) instead of O(triggers *
// exports). Manifests without any Trigger field set are a no-op so the
// legacy authoring style keeps validating.
func (m *Manifest) validateActionTriggers() error {
	exports := m.backendExportSet()
	for model, defs := range m.Actions {
		for i := range defs {
			if err := validateActionTrigger(defs[i].Trigger, exports); err != nil {
				return fmt.Errorf("manifest.actions[%q][%d].%w", model, i, err)
			}
		}
	}
	for i, ext := range m.Extensions {
		for j := range ext.Actions {
			if err := validateActionTrigger(ext.Actions[j].Trigger, exports); err != nil {
				return fmt.Errorf("manifest.extensions[%d].actions[%d].%w", i, j, err)
			}
		}
	}
	return nil
}

// backendExportSet hoists Backend.Exports into a lookup-friendly map. A nil
// Backend or empty Exports list both yield an empty (non-nil) map so callers
// can probe with a single membership check.
func (m *Manifest) backendExportSet() map[string]struct{} {
	if m.Backend == nil || len(m.Backend.Exports) == 0 {
		return map[string]struct{}{}
	}
	out := make(map[string]struct{}, len(m.Backend.Exports))
	for _, e := range m.Backend.Exports {
		out[e] = struct{}{}
	}
	return out
}

// validateActionTrigger enforces the ActionTrigger contract. The exports
// argument is the Backend.Exports lookup hoisted by the caller so wasm
// triggers can be cross-checked without re-walking the slice. A nil trigger
// is a no-op (legacy ActionDefs validate exactly as before).
func validateActionTrigger(t *ActionTrigger, exports map[string]struct{}) error {
	if t == nil {
		return nil
	}
	if _, ok := validTriggerTypes[t.Type]; !ok {
		return fmt.Errorf("trigger.type: unknown %q (want wasm|webhook|noop)", t.Type)
	}
	switch t.Type {
	case "wasm":
		if strings.TrimSpace(t.Export) == "" {
			return fmt.Errorf("trigger.export: required when type=wasm")
		}
		if !triggerExportRe.MatchString(t.Export) {
			return fmt.Errorf("trigger.export: invalid symbol %q", t.Export)
		}
		if _, ok := exports[t.Export]; !ok {
			return fmt.Errorf("trigger.export: %q not declared in backend.exports", t.Export)
		}
	case "webhook":
		// Webhook triggers cannot honour RunInTx — the network hop escapes
		// the request transaction, so the kernel would silently drop the
		// guarantee. Reject the combination at authoring time.
		if t.Export != "" {
			return fmt.Errorf("trigger.export: not allowed when type=webhook")
		}
		if t.RunInTx {
			return fmt.Errorf("trigger.run_in_tx: not allowed when type=webhook")
		}
	case "noop":
		// noop is a UI-only marker; addon code does not run, so neither
		// Export nor RunInTx make sense.
		if t.Export != "" {
			return fmt.Errorf("trigger.export: not allowed when type=noop")
		}
		if t.RunInTx {
			return fmt.Errorf("trigger.run_in_tx: not allowed when type=noop")
		}
	}
	return nil
}

// validateColumnExtensions enforces the optional metadata fields on
// ColumnDef (Visibility, Searchable, Validation, Widget). The function is a
// no-op for zero-valued columns so legacy manifests keep validating.
func validateColumnExtensions(col ColumnDef) error {
	if col.Visibility != "" {
		if _, ok := validVisibility[col.Visibility]; !ok {
			return fmt.Errorf("visibility %q not allowed (want table|modal|list|all)", col.Visibility)
		}
	}
	if col.Widget != "" {
		if _, ok := validWidgets[col.Widget]; !ok {
			return fmt.Errorf("widget %q not allowed", col.Widget)
		}
	}
	if col.Validation != nil {
		if err := col.Validation.validate(); err != nil {
			return fmt.Errorf("validation: %w", err)
		}
	}
	return nil
}

// validateRelations enforces the RelationDef contract on a model. The slice
// is optional — an empty / nil input is a no-op so manifests authored before
// the relation field landed keep validating. Errors are returned with a
// `relations[i]` prefix the caller stitches onto the model index for a
// fully-qualified path the operator can grep.
func validateRelations(rels []RelationDef) error {
	if len(rels) == 0 {
		return nil
	}
	seen := make(map[string]int, len(rels))
	for i, r := range rels {
		if !relationNameRe.MatchString(r.Name) {
			return fmt.Errorf("relations[%d]: invalid name %q", i, r.Name)
		}
		if prev, dup := seen[r.Name]; dup {
			return fmt.Errorf("relations[%d]: duplicate name %q (also at relations[%d])", i, r.Name, prev)
		}
		seen[r.Name] = i
		if _, ok := validRelationKinds[r.Kind]; !ok {
			return fmt.Errorf("relations[%d]: unknown kind %q (want one_to_many|many_to_many)", i, r.Kind)
		}
		if !modelRe.MatchString(r.Through) {
			return fmt.Errorf("relations[%d]: invalid through %q", i, r.Through)
		}
		if !columnRe.MatchString(r.ForeignKey) {
			return fmt.Errorf("relations[%d]: invalid foreign_key %q", i, r.ForeignKey)
		}
		if r.References != "" && !columnRe.MatchString(r.References) {
			return fmt.Errorf("relations[%d]: invalid references %q", i, r.References)
		}
		switch r.Kind {
		case "one_to_many":
			if r.Pivot != "" {
				return fmt.Errorf("relations[%d]: pivot %q not allowed for one_to_many", i, r.Pivot)
			}
		case "many_to_many":
			if !pivotRe.MatchString(r.Pivot) {
				return fmt.Errorf("relations[%d]: many_to_many requires a valid pivot, got %q", i, r.Pivot)
			}
		}
	}
	return nil
}

// validate checks a ValidationRule's internal consistency. The kernel does
// NOT execute the rule here — that happens at write time — it only catches
// authoring mistakes (bad regex, swapped bounds, malformed custom symbol).
func (v *ValidationRule) validate() error {
	if v == nil {
		return nil
	}
	if v.Regex != "" {
		if _, err := regexp.Compile(v.Regex); err != nil {
			return fmt.Errorf("regex %q does not compile: %w", v.Regex, err)
		}
	}
	if v.Min != nil && v.Max != nil && *v.Min > *v.Max {
		return fmt.Errorf("min %g greater than max %g", *v.Min, *v.Max)
	}
	if v.Custom != "" && !customValidatorRe.MatchString(v.Custom) {
		return fmt.Errorf("custom %q must be a dotted identifier (e.g. \"rfc.tax_id\")", v.Custom)
	}
	return nil
}

func (m *Manifest) checkKernelRange(kernelVersion string) error {
	if m.Kernel == "" {
		return nil // legacy addon, no constraint
	}
	constraint, err := semver.NewConstraint(m.Kernel)
	if err != nil {
		return fmt.Errorf("manifest.kernel: invalid range %q: %w", m.Kernel, err)
	}
	kv, err := semver.NewVersion(kernelVersion)
	if err != nil {
		return fmt.Errorf("kernel version %q is not semver: %w", kernelVersion, err)
	}
	if !constraint.Check(kv) {
		return fmt.Errorf("manifest.kernel: host %s does not satisfy %s", kernelVersion, m.Kernel)
	}
	return nil
}
