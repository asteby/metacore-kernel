package manifest

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/asteby/metacore-kernel/manifest/computeexpr"
)

var (
	keyRe    = regexp.MustCompile(`^[a-z][a-z0-9_]{1,63}$`)
	modelRe  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	columnRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)
	// modelKeyRe matches a model REFERENCE — a relation's `through`/`pivot` or
	// any cross-addon model handle. Unlike modelRe (snake_case table names) it
	// accepts PascalCase model keys ("Vehicle", "PurchaseOrder") because that is
	// how authors reference models everywhere else (dynamic_select `ref`,
	// foreign_keys.references.model), and a relation's target is RESOLVED AT
	// RUNTIME against the global model registry — it is NOT required to be a
	// local table. Case-insensitive first char so both "vehicles" and "Vehicle"
	// validate.
	modelKeyRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.]{0,63}$`)
	// customValidatorRe matches "<namespace>.<symbol>" identifiers used by
	// ValidationRule.Custom — keeps it injection-safe for log lines and
	// future router lookups. As of v0.9.0 the same field also accepts
	// `$org.<key>` references that the metadata service swaps for the
	// org-configured validator at request time, so the regex below is
	// matched against the *unprefixed* tail when the value starts with
	// "$org.". This keeps fiscal/regional rules out of core: kernel and
	// SDK only know how to apply a validator the org config picks.
	customValidatorRe = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)
	// orgRefRe matches the `$org.<key>` form that defers validator
	// resolution to org config. The key alphabet matches columnRe (snake
	// identifiers) so it stays grep-friendly across audits.
	orgRefRe = regexp.MustCompile(`^\$org\.[a-z][a-z0-9_]*$`)

	// optionsSourceRe matches a dynamic options-provider key on
	// ColumnDef.OptionsSource. Deliberately the SAME pattern as the v3 schema's
	// Column.options_source so the strict (jsonschema) and legacy validation
	// planes accept exactly the same keys. Open enum: format only — the actual
	// provider registry lives in the host.
	optionsSourceRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
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
	//
	// belongs_to lands in v0.9.0 to power ColumnDef.Ref auto-derivation:
	// a belongs_to declaration says "this model carries the FK column
	// `ForeignKey` pointing at `Through.References`", so the column with
	// that name reports Ref=Through automatically without per-column
	// wiring. Validation enforces Pivot-empty for belongs_to (same shape
	// as one_to_many in that regard).
	validRelationKinds = map[string]struct{}{
		"one_to_many":  {},
		"many_to_many": {},
		"belongs_to":   {},
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
	// validLifecycleHookEvents enumerates the manifest.LifecycleHooks map
	// keys the kernel knows how to fire. "install"/"uninstall"/"enable"/
	// "disable"/"upgrade" come from the installer; the "before_*"/"after_*"
	// family runs from dynamic.Service around model mutations.
	//
	// Authors who declare an event outside this set hit a validation error
	// at install time — manifests stay self-documenting and a typo never
	// silently turns into a hook that never fires.
	validLifecycleHookEvents = map[string]struct{}{
		"install":       {},
		"uninstall":     {},
		"enable":        {},
		"disable":       {},
		"upgrade":       {},
		"before_create": {},
		"after_create":  {},
		"before_update": {},
		"after_update":  {},
		"before_delete": {},
		"after_delete":  {},
	}
	// validLifecycleHookTargets enumerates the HookTarget.Type values an
	// addon can declare. The runner ships dispatchers for "wasm" and
	// "webhook"; "prompt" is reserved for a future LLM dispatcher and
	// validates but runs as a no-op until a host wires one in.
	validLifecycleHookTargets = map[string]struct{}{
		"wasm":    {},
		"webhook": {},
		"prompt":  {},
	}
	// lifecycleBeforeEvents lists the events that semantically veto the
	// operation on error. Used to reject `async: true` combined with a
	// before-hook — the runner needs to block on the result to honour the
	// veto, so async would silently degrade the contract.
	lifecycleBeforeEvents = map[string]struct{}{
		"install":       {},
		"uninstall":     {},
		"enable":        {},
		"disable":       {},
		"upgrade":       {},
		"before_create": {},
		"before_update": {},
		"before_delete": {},
	}
	// validFrontendLayouts is the closed set of FrontendSpec.Layout values.
	// Empty string is accepted at the call site and treated as "shell" for
	// backwards compatibility with manifests authored before the field
	// landed. "immersive" opts the addon into a full-viewport takeover
	// (POS terminals, kitchen-display screens, customer-facing kiosks).
	validFrontendLayouts = map[string]struct{}{
		"shell":     {},
		"immersive": {},
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
	pivotRe        = modelKeyRe
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

// ValidateAdvisory performs the same structural + semantic check as Validate
// and additionally returns a list of advisory warnings. Warnings are
// non-fatal — they communicate deprecated fields, reserved-but-not-yet
// implemented runtime values, and field-cross-check mismatches that an
// addon author should fix but do not break installation.
//
// Returned warnings are stable strings of the form
// `manifest.<path>: <message>` so consumers can grep them. The function
// keeps the same fail-fast semantics as Validate for errors: the first
// error short-circuits the rest of the walk. Warnings emitted before the
// error are still returned alongside it.
//
// Hosts that want to surface the warnings into operator logs should call
// ValidateAdvisory; everything else can keep calling Validate.
func (m *Manifest) ValidateAdvisory(kernelVersion string) ([]string, error) {
	if m == nil {
		return nil, fmt.Errorf("manifest: nil")
	}
	var warnings []string
	// Events at the manifest root were the v0.x mechanism to declare the
	// event names an addon could emit. As of v0.11.0 the gate is the set
	// of `event:emit` capabilities the addon requests — manifest.events
	// is read-only documentation and will be derived from capabilities in
	// v2. Emit a warning when both are present and the rooted Events slice
	// declares names the capabilities do not cover, so authors notice the
	// drift before the schema-level deprecation lands. See
	// docs/wasm-abi.md "Reserved / advisory fields".
	if len(m.Events) > 0 {
		emit := make(map[string]struct{}, len(m.Capabilities))
		for _, c := range m.Capabilities {
			if c.Kind == "event:emit" {
				emit[c.Target] = struct{}{}
			}
		}
		for _, ev := range m.Events {
			if _, ok := emit[ev]; ok {
				continue
			}
			warnings = append(warnings, fmt.Sprintf(
				"manifest.events: %q has no matching capability[event:emit] — "+
					"manifest.events is deprecated (derived from capabilities in v2); "+
					"declare a capability {kind:\"event:emit\", target:%q} instead",
				ev, ev))
		}
	}
	// Backend.Runtime=="binary" was reserved in the ABI v1.0 freeze for a
	// future native side-car runtime. The manifest validates but the kernel
	// has no installer or invoker for it — emit a warning so addon authors
	// know the field is accepted at authoring time but the addon will fail
	// at install/invoke time. host.LoadWASMFromBundle surfaces the install
	// error with code "runtime_not_implemented".
	if m.Backend != nil && m.Backend.Runtime == "binary" {
		warnings = append(warnings, "manifest.backend.runtime: \"binary\" "+
			"is reserved for a future native side-car runtime; the manifest "+
			"is accepted but installation will fail with "+
			"runtime_not_implemented")
	}
	if err := m.validateStrict(kernelVersion); err != nil {
		return warnings, err
	}
	return warnings, nil
}

// Validate performs a full structural + semantic check of the manifest.
// It is cheap and side-effect free; callers should run it before install.
//
// Validate is the legacy entry-point and intentionally returns only the
// first hard error. Use ValidateAdvisory to surface non-fatal warnings
// alongside the error (deprecated fields, reserved runtimes, etc.).
func (m *Manifest) Validate(kernelVersion string) error {
	if m == nil {
		return fmt.Errorf("manifest: nil")
	}
	return m.validateStrict(kernelVersion)
}

// validateStrict is the structural / semantic check shared by Validate and
// ValidateAdvisory. Splitting it out keeps the advisory warnings from
// affecting either call's error contract.
func (m *Manifest) validateStrict(kernelVersion string) error {
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
	// Index every model's declared columns by model KEY so the compute-engine
	// cross-field checks (rollup target on the parent, rollup from on the
	// child, formula identifiers on the model) can resolve columns across
	// models. Mirrors the v3 validator so a manifest fails identically on both
	// the v3 and the legacy/install surfaces ("dual validation gotcha").
	colsByModel := make(map[string]map[string]struct{}, len(m.ModelDefinitions))
	for _, md := range m.ModelDefinitions {
		set := make(map[string]struct{}, len(md.Columns))
		for _, c := range md.Columns {
			set[c.Name] = struct{}{}
		}
		colsByModel[md.ModelKey] = set
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
		if err := validateSeed(md.Seed, md.Columns); err != nil {
			return fmt.Errorf("manifest.model_definitions[%d].%w", i, err)
		}
		if err := validateComputeFormulas(md.Formulas, colsByModel[md.ModelKey]); err != nil {
			return fmt.Errorf("manifest.model_definitions[%d].%w", i, err)
		}
		if err := validateComputeRollups(md.Relations, colsByModel[md.ModelKey], colsByModel); err != nil {
			return fmt.Errorf("manifest.model_definitions[%d].%w", i, err)
		}
		if err := validateStageMachine(md); err != nil {
			return fmt.Errorf("manifest.model_definitions[%d].%w", i, err)
		}
		if err := validateConstraints(md, colsByModel[md.ModelKey]); err != nil {
			return fmt.Errorf("manifest.model_definitions[%d].%w", i, err)
		}
		if err := validateSequences(md); err != nil {
			return fmt.Errorf("manifest.model_definitions[%d].%w", i, err)
		}
	}
	if err := m.validatePipelineRuntime(); err != nil {
		return err
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
	if err := m.validateActionFields(); err != nil {
		return err
	}
	if err := m.validateActionIdempotency(); err != nil {
		return err
	}
	if err := m.validateLifecycleHooks(); err != nil {
		return err
	}
	if m.Frontend != nil {
		switch m.Frontend.Format {
		case "federation", "script", "":
			// ok
		default:
			return fmt.Errorf("manifest.frontend.format: unknown %q", m.Frontend.Format)
		}
		if m.Frontend.Layout != "" {
			if _, ok := validFrontendLayouts[m.Frontend.Layout]; !ok {
				return fmt.Errorf("manifest.frontend.layout: unknown %q (want shell|immersive)", m.Frontend.Layout)
			}
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
// ModelExtension) and enforces ActionTrigger.validate against the exports
// declared by Backend.Exports. The Backend exports set is hoisted once so the
// per-trigger checks stay O(triggers) instead of O(triggers * exports). When
// no Backend.Exports list is declared (v3 manifests, whose wasm handlers are
// the export surface) the membership cross-check is skipped — symbol shape is
// still validated. Manifests without any Trigger field set are a no-op so the
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

// validateActionIdempotency enforces that any ActionDef declaring an
// idempotency block names a non-empty key field. This mirrors the lenient
// v3.Validate check so a manifest that passes one plane passes the other (the
// "double validation" contract). Actions without idempotency are a no-op.
func (m *Manifest) validateActionIdempotency() error {
	check := func(where string, def *ActionDef) error {
		if def.Idempotency != nil && strings.TrimSpace(def.Idempotency.KeyField) == "" {
			return fmt.Errorf("%s.idempotency requires a non-empty keyField", where)
		}
		return nil
	}
	for model, defs := range m.Actions {
		for i := range defs {
			if err := check(fmt.Sprintf("manifest.actions[%q][%d]", model, i), &defs[i]); err != nil {
				return err
			}
		}
	}
	for i, ext := range m.Extensions {
		for j := range ext.Actions {
			if err := check(fmt.Sprintf("manifest.extensions[%d].actions[%d]", i, j), &ext.Actions[j]); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateActionFields walks every action FieldDef the manifest carries (the
// Actions map keyed by model and the Actions slice on each ModelExtension,
// recursing into item_fields line-item groups) and enforces the FORMAT of any
// options_source key with the SAME alphabet as the v3 schema pattern
// (^[a-z][a-z0-9_]*$). This mirrors validateColumnExtensions for columns so a
// manifest that passes v3.Validate never fails the strict legacy validation —
// the "double validation" planes stay in agreement. Fields without an
// options_source are a no-op, so legacy action forms keep validating.
func (m *Manifest) validateActionFields() error {
	for model, defs := range m.Actions {
		for i := range defs {
			if err := validateFieldOptionsSource(defs[i].Fields); err != nil {
				return fmt.Errorf("manifest.actions[%q][%d].%w", model, i, err)
			}
		}
	}
	for i, ext := range m.Extensions {
		for j := range ext.Actions {
			if err := validateFieldOptionsSource(ext.Actions[j].Fields); err != nil {
				return fmt.Errorf("manifest.extensions[%d].actions[%d].%w", i, j, err)
			}
		}
	}
	return nil
}

// validateFieldOptionsSource enforces the options_source key format on a list
// of action fields, recursing into each field's item_fields (line-items).
func validateFieldOptionsSource(fields []FieldDef) error {
	for i := range fields {
		f := fields[i]
		if f.OptionsSource != "" && !optionsSourceRe.MatchString(f.OptionsSource) {
			return fmt.Errorf("fields[%d].options_source %q must match %s", i, f.OptionsSource, optionsSourceRe)
		}
		if err := validateOptionWhen(f.DependsOn, f.Options); err != nil {
			return fmt.Errorf("fields[%d].%w", i, err)
		}
		if len(f.ItemFields) > 0 {
			if err := validateFieldOptionsSource(f.ItemFields); err != nil {
				return fmt.Errorf("fields[%d].%w", i, err)
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
		// Cross-check against Backend.Exports only when an export list was
		// actually declared — the same lenient rule validateLifecycleHooks
		// already applies. A v3 manifest carries no separate backend block:
		// the wasm handler functions ARE the export surface, so an empty set
		// means "nothing authoritative to cross-check" rather than "the
		// backend exports nothing". Legacy manifests that ship an explicit
		// Backend.Exports keep strict typo enforcement.
		if len(exports) > 0 {
			if _, ok := exports[t.Export]; !ok {
				return fmt.Errorf("trigger.export: %q not declared in backend.exports", t.Export)
			}
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
	// OptionsSource is an OPEN enum (providers are host-registered, so the
	// kernel cannot whitelist them); only the key FORMAT is enforced, with the
	// same alphabet as the v3 schema pattern (^[a-z][a-z0-9_]*$) so a manifest
	// that passes v3.Validate never fails here — the "double validation"
	// planes stay in agreement.
	if col.OptionsSource != "" && !optionsSourceRe.MatchString(col.OptionsSource) {
		return fmt.Errorf("options_source %q must match %s", col.OptionsSource, optionsSourceRe)
	}
	if col.Validation != nil {
		if err := col.Validation.validate(); err != nil {
			return fmt.Errorf("validation: %w", err)
		}
	}
	if err := validateOptionWhen(col.DependsOn, col.Options); err != nil {
		return err
	}
	return nil
}

// validateOptionWhen enforces the static-option cascade guard contract on the
// strict plane (mirrors manifest/v3.validateOptionWhen): an option's `when`
// must resolve a governing sibling field (its own `field` or the container's
// `depends_on`) and scope the value with a non-empty `in` or `not_in`. Options
// without a `when` block are unaffected (retro-compatible).
func validateOptionWhen(containerDependsOn string, opts []Option) error {
	for i := range opts {
		w := opts[i].When
		if w == nil {
			continue
		}
		if w.Field == "" && containerDependsOn == "" {
			return fmt.Errorf("options[%d].when requires `field`, or the container's `depends_on`, to name the governing sibling field", i)
		}
		if len(w.In) == 0 && len(w.NotIn) == 0 {
			return fmt.Errorf("options[%d].when requires a non-empty `in` or `not_in`", i)
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
		if !modelKeyRe.MatchString(r.Through) {
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
		case "belongs_to":
			if r.Pivot != "" {
				return fmt.Errorf("relations[%d]: pivot %q not allowed for belongs_to", i, r.Pivot)
			}
		case "many_to_many":
			if !pivotRe.MatchString(r.Pivot) {
				return fmt.Errorf("relations[%d]: many_to_many requires a valid pivot, got %q", i, r.Pivot)
			}
		}
	}
	return nil
}

// validateSeed checks a model's seed block: the natural key must be non-empty
// and name a declared column on the model, there must be at least one row, and
// every row must be a non-empty object. Mirrors the lenient v3 validator so a
// manifest with seeds passes both the v3 and the legacy/install paths. A nil
// seed (the common case) is accepted unconditionally.
func validateSeed(seed *SeedDef, cols []ColumnDef) error {
	if seed == nil {
		return nil
	}
	if seed.Key == "" {
		return fmt.Errorf("seed.key required")
	}
	known := false
	for _, c := range cols {
		if c.Name == seed.Key {
			known = true
			break
		}
	}
	if !known {
		return fmt.Errorf("seed.key %q is not a declared column on the model", seed.Key)
	}
	if len(seed.Rows) == 0 {
		return fmt.Errorf("seed.rows required")
	}
	for i, row := range seed.Rows {
		if len(row) == 0 {
			return fmt.Errorf("seed.rows[%d]: empty object", i)
		}
	}
	return nil
}

// validFns is the rollup aggregate-function allowlist (mirrors v3.validFns and
// the schema enum). count ignores from/expr.
var validFns = map[string]struct{}{
	"sum": {}, "count": {}, "avg": {}, "min": {}, "max": {},
}

// validHookPrefixes mirrors v3.validHookPrefixes: the TransitionHook.Do dispatch
// targets the dynamic engine knows how to route.
var validHookPrefixes = map[string]struct{}{
	"wasm": {}, "webhook": {}, "compiled": {},
}

// validateStageMachine is the legacy/install-surface twin of v3.validateStageMachine:
// stage_field must name a declared column, stage keys must be unique, transitions
// must reference declared stages, and each on_transition hook must match declared
// stages (or "*") with a wasm:/webhook:/compiled: `do`. An empty stage machine
// (no StageField/Stages/Transitions/OnTransition) is accepted unconditionally so
// flat models pass unchanged. Mirrors the v3 validator so a manifest fails
// identically on both the v3 and the legacy/install surfaces ("dual validation").
func validateStageMachine(md ModelDefinition) error {
	if len(md.Stages) == 0 && md.StageField == "" && len(md.Transitions) == 0 && len(md.OnTransition) == 0 {
		return nil
	}
	if md.StageField == "" {
		return fmt.Errorf("stage machine declared without stage_field")
	}
	known := false
	for _, c := range md.Columns {
		if c.Name == md.StageField {
			known = true
			break
		}
	}
	if !known {
		return fmt.Errorf("stage_field %q is not a declared column on the model", md.StageField)
	}
	if len(md.Stages) == 0 {
		return fmt.Errorf("stage_field %q declared but no stages", md.StageField)
	}
	stageKeys := make(map[string]struct{}, len(md.Stages))
	for i, st := range md.Stages {
		if st.Key == "" {
			return fmt.Errorf("stages[%d].key required", i)
		}
		if _, dup := stageKeys[st.Key]; dup {
			return fmt.Errorf("stages[%d].key %q duplicated", i, st.Key)
		}
		stageKeys[st.Key] = struct{}{}
	}
	for i, t := range md.Transitions {
		if _, ok := stageKeys[t.From]; !ok {
			return fmt.Errorf("transitions[%d].from %q is not a declared stage key", i, t.From)
		}
		if _, ok := stageKeys[t.To]; !ok {
			return fmt.Errorf("transitions[%d].to %q is not a declared stage key", i, t.To)
		}
	}
	colNames := make(map[string]struct{}, len(md.Columns))
	for _, c := range md.Columns {
		colNames[c.Name] = struct{}{}
	}
	for i, h := range md.OnTransition {
		if h.From != "*" && h.From != "" {
			if _, ok := stageKeys[h.From]; !ok {
				return fmt.Errorf("onTransition[%d].from %q is not a declared stage key (or \"*\")", i, h.From)
			}
		}
		if h.To != "*" {
			if _, ok := stageKeys[h.To]; !ok {
				return fmt.Errorf("onTransition[%d].to %q is not a declared stage key (or \"*\")", i, h.To)
			}
		}
		if h.Do == "" && len(h.Set) == 0 {
			return fmt.Errorf("onTransition[%d] must declare `set`, `do`, or both", i)
		}
		for col := range h.Set {
			if _, ok := colNames[col]; !ok {
				return fmt.Errorf("onTransition[%d].set key %q is not a declared column on the model", i, col)
			}
		}
		if h.Do != "" {
			prefix, _, found := strings.Cut(h.Do, ":")
			if !found {
				return fmt.Errorf("onTransition[%d].do %q must be wasm:<export> | webhook:<key> | compiled:<fn>", i, h.Do)
			}
			if _, ok := validHookPrefixes[prefix]; !ok {
				return fmt.Errorf("onTransition[%d].do %q has an unknown prefix (want wasm|webhook|compiled)", i, prefix)
			}
		}
	}
	return nil
}

// validateDoRef is the legacy/install twin of v3.validateDoRef: a Schedule /
// InboundWebhook `do` must carry a known wasm:/webhook:/compiled: prefix.
func validateDoRef(do string) error {
	prefix, _, found := strings.Cut(do, ":")
	if !found {
		return fmt.Errorf("%q must be wasm:<export> | webhook:<key> | compiled:<fn>", do)
	}
	if _, ok := validHookPrefixes[prefix]; !ok {
		return fmt.Errorf("%q has an unknown prefix (want wasm|webhook|compiled)", do)
	}
	return nil
}

// validatePipelineRuntime is the legacy/install-surface twin of
// v3.validatePipelineRuntime: connector keys unique; a schedule's `every` parses
// as a positive Go duration and its `do` carries a known prefix; a webhook's
// `do` carries a known prefix and, when it declares a `verify`, its `secret_ref`
// resolves to a declared connector credential. Empty blocks pass unchanged so
// addons without runtime primitives are unaffected. Mirrors the v3 validator so
// a manifest fails identically on both surfaces ("dual validation").
func (m *Manifest) validatePipelineRuntime() error {
	if len(m.Connectors) == 0 && len(m.Schedules) == 0 && len(m.Webhooks) == 0 {
		return nil
	}
	connectorCreds := make(map[string]map[string]struct{}, len(m.Connectors))
	seenConn := make(map[string]struct{}, len(m.Connectors))
	for ci, c := range m.Connectors {
		if c.Key == "" {
			return fmt.Errorf("connectors[%d].key required", ci)
		}
		if _, dup := seenConn[c.Key]; dup {
			return fmt.Errorf("connectors[%d].key %q duplicated", ci, c.Key)
		}
		seenConn[c.Key] = struct{}{}
		creds := make(map[string]struct{}, len(c.Credentials))
		for _, cr := range c.Credentials {
			creds[cr.Key] = struct{}{}
		}
		connectorCreds[c.Key] = creds
	}
	seenSched := make(map[string]struct{}, len(m.Schedules))
	for si, s := range m.Schedules {
		if s.Key == "" {
			return fmt.Errorf("schedules[%d].key required", si)
		}
		if _, dup := seenSched[s.Key]; dup {
			return fmt.Errorf("schedules[%d].key %q duplicated", si, s.Key)
		}
		seenSched[s.Key] = struct{}{}
		if d, err := time.ParseDuration(s.Every); err != nil || d <= 0 {
			return fmt.Errorf("schedules[%d].every %q is not a positive Go duration", si, s.Every)
		}
		if err := validateDoRef(s.Do); err != nil {
			return fmt.Errorf("schedules[%d].do %w", si, err)
		}
	}
	seenHook := make(map[string]struct{}, len(m.Webhooks))
	seenPath := make(map[string]struct{}, len(m.Webhooks))
	for wi, w := range m.Webhooks {
		if w.Key == "" {
			return fmt.Errorf("webhooks[%d].key required", wi)
		}
		if _, dup := seenHook[w.Key]; dup {
			return fmt.Errorf("webhooks[%d].key %q duplicated", wi, w.Key)
		}
		seenHook[w.Key] = struct{}{}
		if w.Path == "" {
			return fmt.Errorf("webhooks[%d].path required", wi)
		}
		if _, dup := seenPath[w.Path]; dup {
			return fmt.Errorf("webhooks[%d].path %q duplicated", wi, w.Path)
		}
		seenPath[w.Path] = struct{}{}
		if err := validateDoRef(w.Do); err != nil {
			return fmt.Errorf("webhooks[%d].do %w", wi, err)
		}
		if w.Verify != "" {
			if w.SecretRef == "" {
				return fmt.Errorf("webhooks[%d] declares verify=%q but no secret_ref", wi, w.Verify)
			}
			conn, cred, ok := strings.Cut(w.SecretRef, ".")
			if !ok {
				return fmt.Errorf("webhooks[%d].secret_ref %q must be \"<connector>.<credential>\"", wi, w.SecretRef)
			}
			creds, ok := connectorCreds[conn]
			if !ok {
				return fmt.Errorf("webhooks[%d].secret_ref %q references undeclared connector %q", wi, w.SecretRef, conn)
			}
			if _, ok := creds[cred]; !ok {
				return fmt.Errorf("webhooks[%d].secret_ref %q references undeclared credential %q on connector %q", wi, w.SecretRef, cred, conn)
			}
		}
	}
	return nil
}

// validateComputeFormulas mirrors the v3 Tier-2 check on the legacy/install
// surface: every formula's target + expr identifiers must be columns on the
// owning model, and expr must pass the strict arithmetic allowlist. A nil/empty
// slice is accepted (the common case). ownCols is the owning model's column set.
func validateComputeFormulas(formulas []Formula, ownCols map[string]struct{}) error {
	for i, f := range formulas {
		if f.Target == "" {
			return fmt.Errorf("formulas[%d]: target required", i)
		}
		if _, ok := ownCols[f.Target]; !ok {
			return fmt.Errorf("formulas[%d]: target %q is not a declared column on the model", i, f.Target)
		}
		if strings.TrimSpace(f.Expr) == "" {
			return fmt.Errorf("formulas[%d]: expr required", i)
		}
		if err := computeexpr.Validate(f.Expr, ownCols); err != nil {
			return fmt.Errorf("formulas[%d]: expr %q: %w", i, f.Expr, err)
		}
	}
	return nil
}

// constraintComparisonOps mirrors dynamic.constraintOps + the v3 validator so
// all three planes agree on the guard grammar (longest-match first).
var constraintComparisonOps = []string{">=", "<=", "!=", "==", ">", "<", "="}

// seqPlaceholderRe mirrors dynamic.seqPlaceholderRe + the v3 validator so all
// three planes agree on the folio placeholder grammar.
var seqPlaceholderRe = regexp.MustCompile(`\{seq(?::0(\d+))?\}`)

// validateSequences mirrors the v3 folio check on the legacy/install surface:
// unique keys, scope enum, a format with exactly one {seq}/{seq:0N} placeholder,
// and every Column.Sequence binding referencing a declared key.
func validateSequences(md ModelDefinition) error {
	keys := make(map[string]struct{}, len(md.Sequences))
	for i, sq := range md.Sequences {
		where := fmt.Sprintf("sequences[%d]", i)
		if strings.TrimSpace(sq.Key) == "" {
			return fmt.Errorf("%s: key required", where)
		}
		if _, dup := keys[sq.Key]; dup {
			return fmt.Errorf("%s: key %q duplicated on the model", where, sq.Key)
		}
		keys[sq.Key] = struct{}{}
		if sq.Scope != "" && sq.Scope != "org" && sq.Scope != "branch" {
			return fmt.Errorf(`%s: scope %q is not one of ""|"org"|"branch"`, where, sq.Scope)
		}
		if strings.TrimSpace(sq.Format) == "" {
			return fmt.Errorf("%s: format required", where)
		}
		if n := len(seqPlaceholderRe.FindAllString(sq.Format, -1)); n != 1 {
			return fmt.Errorf("%s: format %q must contain exactly one {seq}/{seq:0N} placeholder (found %d)", where, sq.Format, n)
		}
	}
	for j, col := range md.Columns {
		if col.Sequence == "" {
			continue
		}
		if _, ok := keys[col.Sequence]; !ok {
			return fmt.Errorf("columns[%d]: sequence %q is not a declared sequence key on the model", j, col.Sequence)
		}
	}
	return nil
}

// validateConstraints mirrors the v3 guard check on the legacy/install surface:
// the model's Locking must be ""|"row", and every column Constraint must carry a
// non-empty error_key and an expr of the shape `<arith> <op> <arith>` whose both
// sides pass the strict arithmetic allowlist against the model's columns.
func validateConstraints(md ModelDefinition, ownCols map[string]struct{}) error {
	if md.Locking != "" && md.Locking != "row" {
		return fmt.Errorf(`locking %q is not one of ""|"row"`, md.Locking)
	}
	for j, col := range md.Columns {
		for k, con := range col.Constraints {
			where := fmt.Sprintf("columns[%d].constraints[%d]", j, k)
			if strings.TrimSpace(con.ErrorKey) == "" {
				return fmt.Errorf("%s: error_key required", where)
			}
			if strings.TrimSpace(con.Expr) == "" {
				return fmt.Errorf("%s: expr required", where)
			}
			if err := validateConstraintExprStrict(con.Expr, ownCols); err != nil {
				return fmt.Errorf("%s: expr %q: %w", where, con.Expr, err)
			}
		}
	}
	return nil
}

// validateConstraintExprStrict checks a guard predicate splits into two
// arithmetic operands around a single comparison operator, each passing the
// allowlist. Mirrors v3.validateConstraintExpr.
func validateConstraintExprStrict(expr string, cols map[string]struct{}) error {
	op, idx := "", -1
	for i := 0; i < len(expr) && op == ""; i++ {
		c := expr[i]
		if c != '>' && c != '<' && c != '=' && c != '!' {
			continue
		}
		for _, o := range constraintComparisonOps {
			if strings.HasPrefix(expr[i:], o) {
				op, idx = o, i
				break
			}
		}
	}
	if op == "" {
		return fmt.Errorf("must contain a comparison operator (>= <= > < == !=)")
	}
	lhs := strings.TrimSpace(expr[:idx])
	rhs := strings.TrimSpace(expr[idx+len(op):])
	if lhs == "" || rhs == "" {
		return fmt.Errorf("comparison is missing an operand")
	}
	if err := computeexpr.Validate(lhs, cols); err != nil {
		return fmt.Errorf("left side %q: %w", lhs, err)
	}
	if err := computeexpr.Validate(rhs, cols); err != nil {
		return fmt.Errorf("right side %q: %w", rhs, err)
	}
	return nil
}

// validateComputeRollups mirrors the v3 Tier-1 check on the legacy/install
// surface: for each relation's rollups, target must be a column on the PARENT
// (ownCols), from (if present) must be a column on the CHILD (relation.Through,
// resolved via colsByModel), fn must be in the enum, exactly one of from/expr
// (count may omit both), and expr must pass the strict arithmetic allowlist
// against the child's columns. Nil/empty rollups are accepted.
func validateComputeRollups(rels []RelationDef, ownCols map[string]struct{}, colsByModel map[string]map[string]struct{}) error {
	for ri, rel := range rels {
		if len(rel.Rollups) == 0 {
			continue
		}
		childCols := colsByModel[rel.Through]
		for ki, r := range rel.Rollups {
			where := fmt.Sprintf("relations[%d].rollups[%d]", ri, ki)
			if r.Target == "" {
				return fmt.Errorf("%s: target required", where)
			}
			if _, ok := ownCols[r.Target]; !ok {
				return fmt.Errorf("%s: target %q is not a declared column on the parent model", where, r.Target)
			}
			fn := strings.ToLower(strings.TrimSpace(r.Fn))
			if fn == "" {
				fn = "sum"
			}
			if _, ok := validFns[fn]; !ok {
				return fmt.Errorf("%s: fn %q is not one of sum|count|avg|min|max", where, r.Fn)
			}
			hasFrom := strings.TrimSpace(r.From) != ""
			hasExpr := strings.TrimSpace(r.Expr) != ""
			if hasFrom && hasExpr {
				return fmt.Errorf("%s: declares both from and expr (use exactly one)", where)
			}
			if fn != "count" && !hasFrom && !hasExpr {
				return fmt.Errorf("%s: fn=%s requires either from or expr", where, fn)
			}
			if hasFrom {
				if !computeexpr.IdentRe.MatchString(r.From) {
					return fmt.Errorf("%s: from %q is not a valid column identifier", where, r.From)
				}
				if _, ok := childCols[r.From]; !ok {
					return fmt.Errorf("%s: from %q is not a declared column on the child model", where, r.From)
				}
			}
			if hasExpr {
				if err := computeexpr.Validate(r.Expr, childCols); err != nil {
					return fmt.Errorf("%s: expr %q: %w", where, r.Expr, err)
				}
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
	if v.Custom != "" && !customValidatorRe.MatchString(v.Custom) && !orgRefRe.MatchString(v.Custom) {
		return fmt.Errorf("custom %q must be a dotted identifier (e.g. \"email.basic\") or an org reference (e.g. \"$org.tax_id_validator\")", v.Custom)
	}
	return nil
}

// validateLifecycleHooks walks `m.LifecycleHooks` and enforces the closed
// event/target sets plus the per-shape contract (wasm exports listed,
// webhook URL present, async forbidden on before-events). A nil map is a
// no-op so manifests that don't declare hooks validate exactly as before.
func (m *Manifest) validateLifecycleHooks() error {
	if len(m.LifecycleHooks) == 0 {
		return nil
	}
	exports := m.backendExportSet()
	for event, defs := range m.LifecycleHooks {
		if _, ok := validLifecycleHookEvents[event]; !ok {
			return fmt.Errorf("manifest.lifecycle_hooks[%q]: unknown event (want install|uninstall|enable|disable|upgrade|before_create|after_create|before_update|after_update|before_delete|after_delete)", event)
		}
		_, isBefore := lifecycleBeforeEvents[event]
		for i, h := range defs {
			if h.Event != "" && h.Event != event {
				// Event is keyed by the map already; the optional field
				// must match when set so the manifest stays self-consistent.
				return fmt.Errorf("manifest.lifecycle_hooks[%q][%d]: event %q does not match map key", event, i, h.Event)
			}
			if _, ok := validLifecycleHookTargets[h.Target.Type]; !ok {
				return fmt.Errorf("manifest.lifecycle_hooks[%q][%d].target.type: unknown %q (want wasm|webhook|prompt)", event, i, h.Target.Type)
			}
			switch h.Target.Type {
			case "wasm":
				if strings.TrimSpace(h.Target.Function) == "" {
					return fmt.Errorf("manifest.lifecycle_hooks[%q][%d].target.function: required when type=wasm", event, i)
				}
				if !triggerExportRe.MatchString(h.Target.Function) {
					return fmt.Errorf("manifest.lifecycle_hooks[%q][%d].target.function: invalid symbol %q", event, i, h.Target.Function)
				}
				// Cross-check against Backend.Exports so a typo is caught
				// at install time rather than at first dispatch.
				if len(exports) > 0 {
					if _, ok := exports[h.Target.Function]; !ok {
						return fmt.Errorf("manifest.lifecycle_hooks[%q][%d].target.function: %q not declared in backend.exports", event, i, h.Target.Function)
					}
				}
			case "webhook":
				if strings.TrimSpace(h.Target.URL) == "" {
					return fmt.Errorf("manifest.lifecycle_hooks[%q][%d].target.url: required when type=webhook", event, i)
				}
			case "prompt":
				if strings.TrimSpace(h.Target.Prompt) == "" {
					return fmt.Errorf("manifest.lifecycle_hooks[%q][%d].target.prompt: required when type=prompt", event, i)
				}
			}
			if h.Async && isBefore {
				// Before-hooks veto the operation on error; async would
				// silently drop the veto. Forbid the combination at
				// authoring time rather than at dispatch.
				return fmt.Errorf("manifest.lifecycle_hooks[%q][%d].async: not allowed on before/lifecycle events (the runner must block on the result to honour a veto)", event, i)
			}
		}
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
