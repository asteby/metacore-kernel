package v3

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Guided setup and health (contributions.setup, contributions.health).
//
// A setup flow is the declarative, resumable "finish configuring this addon"
// page every addon with external moving parts needs (credentials, DNS, a
// webhook registered on the other side, a pairing). The addon declares WHAT the
// steps are and which handler verifies each one; the host persists progress per
// organization (or per instance) and the SDK renders the same page for every
// addon. The runtime contract of the handlers (status, message_key, fix ...)
// lives in package setupflow.
//
// Everything here is optional: an addon without setup/health behaves as it
// always did.

// Setup trigger / scope / step-kind vocabulary.
const (
	// SetupTriggerOnInstall opens the flow automatically when the addon is
	// installed (and shows a "continue configuration" banner until complete).
	SetupTriggerOnInstall = "on_install"
	// SetupTriggerManual makes the flow available but never auto-opened.
	SetupTriggerManual = "manual"

	// SetupScopeOrg keeps one progress record per organization (default).
	SetupScopeOrg = "org"
	// SetupScopeInstance keeps one progress record per host instance, for
	// flows that configure something machine-wide (a native sidecar's host
	// settings). Only instance administrators may run it.
	SetupScopeInstance = "instance"

	SetupKindCheck   = "check"
	SetupKindForm    = "form"
	SetupKindAction  = "action"
	SetupKindRecords = "records"
	SetupKindCustom  = "custom"
)

// SetupLimits bound a flow so a manifest cannot build an unrenderable wizard.
const (
	MaxSetupFlows         = 8
	MaxSetupSteps         = 40
	MaxSetupDependsOn     = 16
	MaxSetupFormSettings  = 64
	MinSetupPollEvery     = time.Second
	MaxSetupPollEvery     = 24 * time.Hour
	MaxSetupTimeout       = 5 * time.Minute
	MaxSetupGiveUpAfter   = 30 * 24 * time.Hour
	MinHealthEvery        = 10 * time.Second
	MaxHealthEvery        = 24 * time.Hour
	MaxSetupPollFactor    = 10.0
	DefaultSetupPollFact  = 2.0
	DefaultSetupTimeout   = 30 * time.Second
	DefaultHealthEvery    = 5 * time.Minute
	DefaultSetupGiveUpAft = 24 * time.Hour
)

// SetupFlow is one guided configuration flow of an addon.
type SetupFlow struct {
	// Key identifies the flow within the addon ("main"); the host serves it
	// at /setup/<addon>[/<key>].
	Key string `json:"key"`
	// Title is an i18n key (or literal) shown as the page heading.
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	// Trigger is "on_install" or "manual" (default). At most one flow of an
	// addon may be on_install: that is the page opened right after install.
	Trigger string `json:"trigger,omitempty"`
	// Permission is the RBAC permission required to open/run the flow. A key
	// under this addon's prefix must be declared in rbac.permissions.
	Permission string `json:"permission,omitempty"`
	// Scope is "org" (default) or "instance".
	Scope string      `json:"scope,omitempty"`
	Steps []SetupStep `json:"steps"`
}

// SetupStep is one step of a SetupFlow. Kind selects exactly one body:
//
//	check    run a handler and show its status      -> Check
//	form     collect settings or connector creds    -> Form
//	action   a button bound to contributions.actions -> Action
//	records  a table of rows a handler returns       -> Records
//	custom   an addon-provided federated component   -> Custom
type SetupStep struct {
	Key         string `json:"key"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	// Optional steps never block the flow's completion nor the steps that
	// depend on them.
	Optional bool `json:"optional,omitempty"`
	// DependsOn lists step keys of the same flow that must be done (or
	// skipped, or optional-and-unfinished) before this step is offered.
	DependsOn []string `json:"depends_on,omitempty"`
	Kind      string   `json:"kind"`

	Check   *SetupCheck   `json:"check,omitempty"`
	Form    *SetupForm    `json:"form,omitempty"`
	Action  *SetupAction  `json:"action,omitempty"`
	Records *SetupRecords `json:"records,omitempty"`
	Custom  *SetupCustom  `json:"custom,omitempty"`

	// SkipWhen marks the step skipped (and satisfied) while a setting of the
	// addon has a given value, e.g. "no Cloudflare step when dns_mode=manual".
	SkipWhen *SetupSkipWhen `json:"skip_when,omitempty"`
}

// SetupHandler references a handler of this addon: a wasm export or a native
// operation. It is the same dispatch the addon's actions use.
type SetupHandler struct {
	// Type is "wasm" or "native".
	Type string `json:"type"`
	// Function is the wasm export (type=wasm). It joins the addon's declared
	// export surface, so the hub scanner verifies the module exports it.
	Function string `json:"function,omitempty"`
	// Operation is the metacore.native/v1 operation (type=native).
	Operation string `json:"operation,omitempty"`
}

// SetupCheck runs Handler and renders its CheckResult. Poll, when set, makes
// the host re-run it with backoff while the result is pending/running.
type SetupCheck struct {
	Handler SetupHandler `json:"handler"`
	Poll    *SetupPoll   `json:"poll,omitempty"`
	// Timeout is the Go duration one execution may take (default 30s, max 5m).
	Timeout string `json:"timeout,omitempty"`
}

// SetupPoll is the re-run schedule of a check: first re-run after Every, each
// next delay multiplied by Factor up to MaxEvery (30s -> 1m -> 2m ... 1h), until
// the result settles or GiveUpAfter elapses.
type SetupPoll struct {
	Every       string  `json:"every"`
	MaxEvery    string  `json:"max_every,omitempty"`
	Factor      float64 `json:"factor,omitempty"`
	GiveUpAfter string  `json:"give_up_after,omitempty"`
}

// SetupForm collects values. Exactly one of Settings (keys of this addon's
// settings[]) or Connector (key of one of its connectors[]; its credentials
// are the fields).
type SetupForm struct {
	Settings  []string `json:"settings,omitempty"`
	Connector string   `json:"connector,omitempty"`
}

// SetupAction binds the step to one of the addon's contributions.actions[].
type SetupAction struct {
	Action string `json:"action"`
}

// SetupRecords renders a table whose rows come from Handler (the handler
// returns the kernel envelope {success,data:{rows:[...]},meta}).
type SetupRecords struct {
	Handler SetupHandler `json:"handler"`
}

// SetupCustom renders a component the addon contributes to Slot
// (contributions.slots[].slot_kind).
type SetupCustom struct {
	Slot string `json:"slot"`
}

// SetupSkipWhen skips a step while settings[Setting] equals Equals or is one
// of In.
type SetupSkipWhen struct {
	Setting string   `json:"setting"`
	Equals  string   `json:"equals,omitempty"`
	In      []string `json:"in,omitempty"`
}

// HealthProbe declares the addon's periodic health handler. The host runs it
// every Every (default 5m) for each installation and shows the result on the
// installed-addons card; the handler returns a setupflow.HealthResult.
type HealthProbe struct {
	Handler SetupHandler `json:"handler"`
	// Every is a Go duration, 10s..24h (default 5m).
	Every string `json:"every,omitempty"`
	// Permission is the RBAC permission required to read the detailed result.
	Permission string `json:"permission,omitempty"`
}

var (
	setupKeyRe    = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)
	setupSymbolRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	operationRe   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,127}$`)
)

// SetupRefs is everything ValidateSetup cross-checks a flow against. Both the
// v3 validator and the legacy manifest.Manifest validator build one, so the
// two planes reject exactly the same manifests ("dual validation").
type SetupRefs struct {
	// AddonKey is the addon's own key (permission prefix check).
	AddonKey string
	// Permissions are the keys declared in rbac.permissions. nil = not
	// checkable by this validator (the legacy projection carries no RBAC), so
	// the own-prefix declaration check is skipped.
	Permissions map[string]struct{}
	// Settings, Connectors, Actions and Slots are the keys the steps may
	// reference (settings[].key, connectors[].key, contributions.actions[].key,
	// contributions.slots[].slot_kind). Slots nil = not checkable (the legacy
	// projection carries no slots), so custom.slot existence is skipped.
	Settings, Connectors, Actions, Slots map[string]struct{}
	// HasNativeService is true when runtime.native_service is declared.
	HasNativeService bool
	// Exports is the declared wasm export surface. When non-empty every wasm
	// handler function must be in it (legacy manifests with an explicit
	// backend.exports); empty means the handlers ARE the surface.
	Exports map[string]struct{}
}

func setOf(keys []string) map[string]struct{} {
	out := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		out[k] = struct{}{}
	}
	return out
}

func parseDur(field, v string, min, max time.Duration) (time.Duration, string) {
	d, err := time.ParseDuration(v)
	if err != nil || d < min || d > max {
		return 0, fmt.Sprintf("%s %q must be a Go duration between %s and %s", field, v, min, max)
	}
	return d, ""
}

func validateSetupHandler(where string, h SetupHandler, refs SetupRefs) []string {
	switch h.Type {
	case "wasm":
		var errs []string
		if h.Function == "" {
			return []string{where + ".function is required when type=wasm"}
		}
		if !setupSymbolRe.MatchString(h.Function) {
			errs = append(errs, fmt.Sprintf("%s.function %q is not a valid export symbol", where, h.Function))
		} else if len(refs.Exports) > 0 {
			if _, ok := refs.Exports[h.Function]; !ok {
				errs = append(errs, fmt.Sprintf("%s.function %q is not declared in backend.exports", where, h.Function))
			}
		}
		if h.Operation != "" {
			errs = append(errs, where+".operation is only allowed when type=native")
		}
		return errs
	case "native":
		var errs []string
		if !operationRe.MatchString(h.Operation) {
			errs = append(errs, fmt.Sprintf("%s.operation %q must match %s when type=native", where, h.Operation, operationRe))
		}
		if h.Function != "" {
			errs = append(errs, where+".function is only allowed when type=wasm")
		}
		if !refs.HasNativeService {
			errs = append(errs, where+": type=native requires runtime.native_service")
		}
		return errs
	}
	return []string{fmt.Sprintf("%s.type %q not allowed (want wasm|native)", where, h.Type)}
}

func validateSetupPermission(where, perm string, refs SetupRefs) []string {
	if perm == "" {
		return nil
	}
	if !PermissionKeyRe.MatchString(perm) {
		return []string{fmt.Sprintf("%s.permission %q is not a valid permission key (want e.g. \"<addon>.<resource>.<action>\")", where, perm)}
	}
	if refs.Permissions != nil && refs.AddonKey != "" && strings.HasPrefix(perm, refs.AddonKey+".") {
		if _, ok := refs.Permissions[perm]; !ok {
			return []string{fmt.Sprintf("%s.permission %q is not declared in rbac.permissions", where, perm)}
		}
	}
	return nil
}

// ValidateSetup validates contributions.setup and contributions.health against
// the manifest's other declarations. It returns one message per problem,
// prefixed with the JSON path ("contributions.setup[0].steps[2]...").
func ValidateSetup(flows []SetupFlow, probe *HealthProbe, refs SetupRefs) []string {
	var errs []string
	if len(flows) > MaxSetupFlows {
		errs = append(errs, fmt.Sprintf("contributions.setup allows at most %d flows", MaxSetupFlows))
	}
	seenFlow := map[string]struct{}{}
	onInstall := 0
	for fi, f := range flows {
		w := fmt.Sprintf("contributions.setup[%d]", fi)
		if !setupKeyRe.MatchString(f.Key) {
			errs = append(errs, fmt.Sprintf("%s.key %q must match %s", w, f.Key, setupKeyRe))
		} else if _, dup := seenFlow[f.Key]; dup {
			errs = append(errs, fmt.Sprintf("%s.key %q is duplicated", w, f.Key))
		}
		seenFlow[f.Key] = struct{}{}
		if strings.TrimSpace(f.Title) == "" {
			errs = append(errs, w+".title is required")
		}
		switch f.Trigger {
		case "", SetupTriggerManual:
		case SetupTriggerOnInstall:
			onInstall++
		default:
			errs = append(errs, fmt.Sprintf("%s.trigger %q must be on_install or manual", w, f.Trigger))
		}
		switch f.Scope {
		case "", SetupScopeOrg, SetupScopeInstance:
		default:
			errs = append(errs, fmt.Sprintf("%s.scope %q must be org or instance", w, f.Scope))
		}
		errs = append(errs, validateSetupPermission(w, f.Permission, refs)...)
		errs = append(errs, validateSetupSteps(w, f.Steps, refs)...)
	}
	if onInstall > 1 {
		errs = append(errs, "contributions.setup: at most one flow may have trigger on_install")
	}
	if probe != nil {
		w := "contributions.health"
		errs = append(errs, validateSetupHandler(w+".handler", probe.Handler, refs)...)
		if probe.Every != "" {
			if _, msg := parseDur(w+".every", probe.Every, MinHealthEvery, MaxHealthEvery); msg != "" {
				errs = append(errs, msg)
			}
		}
		errs = append(errs, validateSetupPermission(w, probe.Permission, refs)...)
	}
	return errs
}

func validateSetupSteps(fw string, steps []SetupStep, refs SetupRefs) []string {
	var errs []string
	if len(steps) == 0 {
		return []string{fw + ".steps requires at least one step"}
	}
	if len(steps) > MaxSetupSteps {
		errs = append(errs, fmt.Sprintf("%s.steps allows at most %d steps", fw, MaxSetupSteps))
	}
	keys := make(map[string]struct{}, len(steps))
	for _, s := range steps {
		keys[s.Key] = struct{}{}
	}
	seen := map[string]struct{}{}
	for si, s := range steps {
		w := fmt.Sprintf("%s.steps[%d]", fw, si)
		if !setupKeyRe.MatchString(s.Key) {
			errs = append(errs, fmt.Sprintf("%s.key %q must match %s", w, s.Key, setupKeyRe))
		} else if _, dup := seen[s.Key]; dup {
			errs = append(errs, fmt.Sprintf("%s.key %q is duplicated in the flow", w, s.Key))
		}
		seen[s.Key] = struct{}{}
		if strings.TrimSpace(s.Title) == "" {
			errs = append(errs, w+".title is required")
		}
		if len(s.DependsOn) > MaxSetupDependsOn {
			errs = append(errs, fmt.Sprintf("%s.depends_on allows at most %d entries", w, MaxSetupDependsOn))
		}
		depSeen := map[string]struct{}{}
		for _, d := range s.DependsOn {
			switch _, ok := keys[d]; {
			case d == s.Key:
				errs = append(errs, fmt.Sprintf("%s.depends_on: step %q depends on itself", w, d))
			case !ok:
				errs = append(errs, fmt.Sprintf("%s.depends_on references unknown step %q", w, d))
			}
			if _, dup := depSeen[d]; dup {
				errs = append(errs, fmt.Sprintf("%s.depends_on lists %q twice", w, d))
			}
			depSeen[d] = struct{}{}
		}
		errs = append(errs, validateSetupStepBody(w, s, refs)...)
		if sw := s.SkipWhen; sw != nil {
			if _, ok := refs.Settings[sw.Setting]; !ok {
				errs = append(errs, fmt.Sprintf("%s.skip_when.setting %q is not a setting of this addon", w, sw.Setting))
			}
			if (sw.Equals == "") == (len(sw.In) == 0) {
				errs = append(errs, w+".skip_when requires exactly one of equals or in")
			}
		}
	}
	if cyc := setupDependencyCycle(steps); cyc != nil {
		errs = append(errs, fmt.Sprintf("%s.steps: depends_on cycle %s", fw, strings.Join(cyc, " -> ")))
	}
	return errs
}

func validateSetupStepBody(w string, s SetupStep, refs SetupRefs) []string {
	var errs []string
	bodies := map[string]bool{
		SetupKindCheck: s.Check != nil, SetupKindForm: s.Form != nil, SetupKindAction: s.Action != nil,
		SetupKindRecords: s.Records != nil, SetupKindCustom: s.Custom != nil,
	}
	if _, known := bodies[s.Kind]; !known {
		return []string{fmt.Sprintf("%s.kind %q must be one of check|form|action|records|custom", w, s.Kind)}
	}
	for kind, present := range bodies {
		switch {
		case kind == s.Kind && !present:
			errs = append(errs, fmt.Sprintf("%s.%s is required when kind=%s", w, kind, s.Kind))
		case kind != s.Kind && present:
			errs = append(errs, fmt.Sprintf("%s.%s is not allowed when kind=%s", w, kind, s.Kind))
		}
	}
	sort.Strings(errs)
	switch s.Kind {
	case SetupKindCheck:
		if c := s.Check; c != nil {
			errs = append(errs, validateSetupHandler(w+".check.handler", c.Handler, refs)...)
			if c.Timeout != "" {
				if _, msg := parseDur(w+".check.timeout", c.Timeout, time.Millisecond, MaxSetupTimeout); msg != "" {
					errs = append(errs, msg)
				}
			}
			if c.Poll != nil {
				errs = append(errs, validateSetupPoll(w+".check.poll", *c.Poll)...)
			}
		}
	case SetupKindForm:
		if f := s.Form; f != nil {
			hasSettings, hasConn := len(f.Settings) > 0, f.Connector != ""
			switch {
			case hasSettings == hasConn:
				errs = append(errs, w+".form requires exactly one of settings or connector")
			case hasConn:
				if _, ok := refs.Connectors[f.Connector]; !ok {
					errs = append(errs, fmt.Sprintf("%s.form.connector %q is not a connector of this addon", w, f.Connector))
				}
			default:
				if len(f.Settings) > MaxSetupFormSettings {
					errs = append(errs, fmt.Sprintf("%s.form.settings allows at most %d keys", w, MaxSetupFormSettings))
				}
				dup := map[string]struct{}{}
				for _, k := range f.Settings {
					if _, ok := refs.Settings[k]; !ok {
						errs = append(errs, fmt.Sprintf("%s.form.settings: %q is not a setting of this addon", w, k))
					}
					if _, d := dup[k]; d {
						errs = append(errs, fmt.Sprintf("%s.form.settings lists %q twice", w, k))
					}
					dup[k] = struct{}{}
				}
			}
		}
	case SetupKindAction:
		if a := s.Action; a != nil {
			if _, ok := refs.Actions[a.Action]; !ok {
				errs = append(errs, fmt.Sprintf("%s.action.action %q is not declared in contributions.actions", w, a.Action))
			}
		}
	case SetupKindRecords:
		if r := s.Records; r != nil {
			errs = append(errs, validateSetupHandler(w+".records.handler", r.Handler, refs)...)
		}
	case SetupKindCustom:
		if c := s.Custom; c != nil {
			if _, ok := refs.Slots[c.Slot]; refs.Slots != nil && !ok {
				errs = append(errs, fmt.Sprintf("%s.custom.slot %q is not contributed in contributions.slots", w, c.Slot))
			}
		}
	}
	return errs
}

func validateSetupPoll(w string, p SetupPoll) []string {
	var errs []string
	every, msg := parseDur(w+".every", p.Every, MinSetupPollEvery, MaxSetupPollEvery)
	if msg != "" {
		errs = append(errs, msg)
	}
	if p.MaxEvery != "" {
		mx, m2 := parseDur(w+".max_every", p.MaxEvery, MinSetupPollEvery, MaxSetupPollEvery)
		if m2 != "" {
			errs = append(errs, m2)
		} else if msg == "" && mx < every {
			errs = append(errs, fmt.Sprintf("%s.max_every %s must be >= every %s", w, p.MaxEvery, p.Every))
		}
	}
	if p.Factor != 0 && (p.Factor < 1 || p.Factor > MaxSetupPollFactor) {
		errs = append(errs, fmt.Sprintf("%s.factor must be between 1 and %v", w, MaxSetupPollFactor))
	}
	if p.GiveUpAfter != "" {
		if _, m3 := parseDur(w+".give_up_after", p.GiveUpAfter, time.Second, MaxSetupGiveUpAfter); m3 != "" {
			errs = append(errs, m3)
		}
	}
	return errs
}

// setupDependencyCycle returns the first depends_on cycle ("a -> b -> a") or
// nil. Unknown keys are ignored (reported elsewhere).
func setupDependencyCycle(steps []SetupStep) []string {
	deps := make(map[string][]string, len(steps))
	order := make([]string, 0, len(steps))
	for _, s := range steps {
		if _, dup := deps[s.Key]; dup {
			continue
		}
		deps[s.Key] = s.DependsOn
		order = append(order, s.Key)
	}
	const (
		white, grey, black = 0, 1, 2
	)
	color := map[string]int{}
	var stack []string
	var visit func(string) []string
	visit = func(k string) []string {
		color[k] = grey
		stack = append(stack, k)
		for _, d := range deps[k] {
			if _, ok := deps[d]; !ok || d == k {
				continue // unknown / self refs are reported by the caller
			}
			switch color[d] {
			case grey:
				i := len(stack) - 1
				for stack[i] != d {
					i--
				}
				return append(append([]string(nil), stack[i:]...), d)
			case white:
				if c := visit(d); c != nil {
					return c
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[k] = black
		return nil
	}
	for _, k := range order {
		if color[k] == white {
			if c := visit(k); c != nil {
				return c
			}
		}
	}
	return nil
}

// SetupDependencyCycle exposes the cycle finder to hosts that build flows
// programmatically.
func SetupDependencyCycle(steps []SetupStep) []string { return setupDependencyCycle(steps) }

// SetupWasmExports lists the wasm export names the flows and the probe call, in
// declaration order and deduplicated. Hosts and deriveBackend use it so every
// declared handler joins the export surface the runtime dispatches against.
func SetupWasmExports(flows []SetupFlow, probe *HealthProbe) []string {
	var out []string
	seen := map[string]struct{}{}
	add := func(h SetupHandler) {
		if h.Type != "wasm" || h.Function == "" {
			return
		}
		if _, ok := seen[h.Function]; ok {
			return
		}
		seen[h.Function] = struct{}{}
		out = append(out, h.Function)
	}
	for _, f := range flows {
		for _, s := range f.Steps {
			if s.Check != nil {
				add(s.Check.Handler)
			}
			if s.Records != nil {
				add(s.Records.Handler)
			}
		}
	}
	if probe != nil {
		add(probe.Handler)
	}
	return out
}

// validateSetupSettings checks Setting.visible_when / write_only across a list
// of sibling settings (addon settings[] or one connector's credentials[]).
func validateSettingExtras(where string, list []Setting) []string {
	var errs []string
	keys := make(map[string]struct{}, len(list))
	for _, s := range list {
		keys[s.Key] = struct{}{}
	}
	deps := map[string]string{}
	for i, s := range list {
		w := fmt.Sprintf("%s[%d] (%s)", where, i, s.Key)
		if s.WriteOnly && s.Default != nil {
			errs = append(errs, w+".write_only cannot declare a default (it would be served back)")
		}
		vw := s.VisibleWhen
		if vw == nil {
			continue
		}
		switch {
		case vw.Class != "":
			errs = append(errs, w+".visible_when.class is not supported on settings (use field)")
		case vw.Field == "":
			errs = append(errs, w+".visible_when.field is required")
		case vw.Field == s.Key:
			errs = append(errs, w+".visible_when cannot reference itself")
		default:
			if _, ok := keys[vw.Field]; !ok {
				errs = append(errs, fmt.Sprintf("%s.visible_when.field %q is not a sibling setting", w, vw.Field))
			} else {
				deps[s.Key] = vw.Field
			}
		}
		if vw.Equals == "" && len(vw.In) == 0 && vw.Class == "" {
			errs = append(errs, w+".visible_when requires equals or in")
		}
	}
	// visibility cycles: a -> b -> a would hide both forever.
	for start := range deps {
		cur, hops := start, 0
		for hops <= len(deps) {
			next, ok := deps[cur]
			if !ok {
				break
			}
			if next == start {
				errs = append(errs, fmt.Sprintf("%s: visible_when cycle through %q", where, start))
				break
			}
			cur = next
			hops++
		}
	}
	sort.Strings(errs)
	return errs
}

// ValidateSettingExtras validates visible_when / write_only over a settings
// list; exported for the legacy validator.
func ValidateSettingExtras(where string, list []Setting) []string {
	return validateSettingExtras(where, list)
}

// setupRefsFromManifest builds the cross-reference sets for a v3 manifest.
func setupRefsFromManifest(m *Manifest) SetupRefs {
	refs := SetupRefs{
		AddonKey:    m.Metadata.Key,
		Permissions: map[string]struct{}{},
		Settings:    map[string]struct{}{},
		Connectors:  map[string]struct{}{},
		Actions:     map[string]struct{}{},
		Slots:       map[string]struct{}{},
	}
	if m.RBAC != nil {
		for _, p := range m.RBAC.Permissions {
			refs.Permissions[p.Key] = struct{}{}
		}
	}
	for _, s := range m.Settings {
		refs.Settings[s.Key] = struct{}{}
	}
	for _, c := range m.Connectors {
		refs.Connectors[c.Key] = struct{}{}
	}
	if m.Contributions != nil {
		for _, a := range m.Contributions.Actions {
			refs.Actions[a.Key] = struct{}{}
		}
		for _, sl := range m.Contributions.Slots {
			refs.Slots[sl.SlotKind] = struct{}{}
		}
	}
	refs.HasNativeService = m.Runtime != nil && m.Runtime.NativeService != nil
	return refs
}

func validateSetup(m *Manifest) []string {
	var errs []string
	if m.Contributions != nil && (len(m.Contributions.Setup) > 0 || m.Contributions.Health != nil) {
		errs = append(errs, ValidateSetup(m.Contributions.Setup, m.Contributions.Health, setupRefsFromManifest(m))...)
	}
	errs = append(errs, validateSettingExtras("settings", m.Settings)...)
	for ci, c := range m.Connectors {
		errs = append(errs, validateSettingExtras(fmt.Sprintf("connectors[%d].credentials", ci), c.Credentials)...)
	}
	return errs
}
