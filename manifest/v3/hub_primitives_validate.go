package v3

// Contract validation for the hub-primitives RFC set (docs/rfcs/2026-10-22-hub-primitivos):
//
//	RFC-01  webhook action dispatch fields on Handler (url_ref, url_setting, path,
//	        method, signing, send_record, timeout_ms, retries, on_success)
//	RFC-02  handler.type set_fields + Handler.set, Action.confirm_typed, Action.tone
//	RFC-04  Model.ingest and webhooks[].do "upsert:<Model>"
//	RFC-06  rbac_permission_resolves (tolerated rule)
//	RFC-08  WidgetQuery.range last_hour|last_24_hours, "$now-<dur>" in where,
//	        DashboardWidget.link
//
// This file is CONTRACT only: no runtime. The checks live here (not inline in
// validate.go) so the dual-validation twin in manifest/validate.go can reuse
// the exported helpers (ParseSetValue, ManagedColumn, ...).

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ---- tolerated rules (see compat.go) ----

const (
	// RuleWebhookActionSigning: a contributions.actions[] webhook handler must
	// declare an https destination (url / url_ref / url_setting) AND `signing`.
	// Before RFC-01 a webhook action was an unsigned POST that OPS never even
	// dispatched, so no published manifest depends on it; the rule still ships
	// as a warning for one minor. Promote to a hard error one minor after
	// v0.230.0.
	RuleWebhookActionSigning = "webhook_action_signing"

	// RuleRBACPermissionResolves: a key in rbac.roles[].permissions prefixed with
	// this addon's own key (<addon>.<resource>.<verb>) must be declared in
	// rbac.permissions or resolve to one of the addon's models (verb in
	// read|write|create|update|delete|index|show|export|import or an action key).
	// A key that resolves to nothing grants nothing and nobody notices (fails
	// closed, silently), so staff end up using admin for everything. Keys with
	// another addon's prefix cannot be judged from one manifest and are skipped.
	// Promote to a hard error one minor after v0.230.0.
	RuleRBACPermissionResolves = "rbac_permission_resolves"
)

// ---- helpers shared with the legacy validator (manifest/validate.go) ----

var setIdentRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// SetValueKind classifies a Handler.set value.
type SetValueKind string

// Kinds of SetValue.
const (
	SetLiteral SetValueKind = "literal" // scalar literal (string, number, bool)
	SetNull    SetValueKind = "null"
	SetNow     SetValueKind = "now"      // $now
	SetActorID SetValueKind = "actor_id" // $actor.id
	SetPayload SetValueKind = "payload"  // $payload.<field>
	SetRecord  SetValueKind = "record"   // $record.<column>
)

// SetValue is a parsed Handler.set value.
type SetValue struct {
	Kind SetValueKind
	// Name is the payload field or record column for SetPayload / SetRecord.
	Name string
	// Literal is the scalar for SetLiteral (a "$$x" string is unescaped to "$x").
	Literal any
}

// ParseSetValue parses one value of Handler.set / on_success.set: a scalar
// literal, null or a token ($now, $actor.id, $payload.<field>, $record.<col>).
// A string starting with "$$" is the literal "$..." (escape). Any other "$..."
// string, and any object/array, is an error.
func ParseSetValue(v any) (SetValue, error) {
	switch x := v.(type) {
	case nil:
		return SetValue{Kind: SetNull}, nil
	case bool, float64, float32, int, int32, int64, json.Number:
		return SetValue{Kind: SetLiteral, Literal: x}, nil
	case string:
		if !strings.HasPrefix(x, "$") {
			return SetValue{Kind: SetLiteral, Literal: x}, nil
		}
		if strings.HasPrefix(x, "$$") {
			return SetValue{Kind: SetLiteral, Literal: x[1:]}, nil
		}
		switch x {
		case "$now":
			return SetValue{Kind: SetNow}, nil
		case "$actor.id":
			return SetValue{Kind: SetActorID}, nil
		}
		if name, ok := strings.CutPrefix(x, "$payload."); ok && setIdentRe.MatchString(name) {
			return SetValue{Kind: SetPayload, Name: name}, nil
		}
		if name, ok := strings.CutPrefix(x, "$record."); ok && setIdentRe.MatchString(name) {
			return SetValue{Kind: SetRecord, Name: name}, nil
		}
		return SetValue{}, fmt.Errorf("unknown token %q (want $now, $actor.id, $payload.<field>, $record.<column>; write a literal starting with $ as $$...)", x)
	default:
		return SetValue{}, fmt.Errorf("must be a scalar, null or a token, got %T", v)
	}
}

// ManagedColumn reports whether a column is owned by the kernel and can never
// be written by set_fields / ingest: id, organization_id and the audit columns
// (created_*, updated_*, deleted_*).
func ManagedColumn(name string) bool {
	switch name {
	case "id", "organization_id":
		return true
	}
	return strings.HasPrefix(name, "created_") || strings.HasPrefix(name, "updated_") || strings.HasPrefix(name, "deleted_")
}

var (
	webhookMethods  = map[string]struct{}{"POST": {}, "PUT": {}, "PATCH": {}, "DELETE": {}}
	signingSchemes  = map[string]struct{}{"metacore-hmac-v1": {}, "instance-ed25519-v1": {}}
	connectorRefRe  = regexp.MustCompile(`^[a-z][a-z0-9_]*\.[a-z][a-z0-9_]*$`)
	pathTemplateRe  = regexp.MustCompile(`\{[^{}]*\}`)
	nowTokenRe      = regexp.MustCompile(`^\$now(?:-(\d+)([mhd]))?$`)
	linkURLForbidRe = regexp.MustCompile(`[\x00-\x1f\\]`)
)

const (
	maxNowDays      = 400
	maxLinkURLChars = 512
)

// handlerWebhookFields lists the webhook-only dispatch fields set on h.
func handlerWebhookFields(h Handler) []string {
	var f []string
	if h.URLRef != "" {
		f = append(f, "url_ref")
	}
	if h.URLSetting != "" {
		f = append(f, "url_setting")
	}
	if h.Path != "" {
		f = append(f, "path")
	}
	if h.Method != "" {
		f = append(f, "method")
	}
	if h.Signing != nil {
		f = append(f, "signing")
	}
	if len(h.SendRecord) > 0 {
		f = append(f, "send_record")
	}
	if h.TimeoutMS != 0 {
		f = append(f, "timeout_ms")
	}
	if h.Retries != nil {
		f = append(f, "retries")
	}
	if h.OnSuccess != nil {
		f = append(f, "on_success")
	}
	return f
}

// actionOnlyHandlerFields lists every field only valid on an action handler
// (webhook dispatch fields + set).
func actionOnlyHandlerFields(h Handler) []string {
	f := handlerWebhookFields(h)
	if h.Set != nil {
		f = append(f, "set")
	}
	return f
}

// ---- actions ----

type modelIndex struct {
	byKey map[string]Model
	cols  map[string]map[string]Column
}

func newModelIndex(m *Manifest) modelIndex {
	ix := modelIndex{byKey: map[string]Model{}, cols: map[string]map[string]Column{}}
	for _, mod := range m.Models {
		ix.byKey[mod.Key] = mod
		cs := make(map[string]Column, len(mod.Columns))
		for _, c := range mod.Columns {
			cs[c.Name] = c
		}
		ix.cols[mod.Key] = cs
	}
	return ix
}

// settable returns the reason a column of mod cannot be written by set_fields /
// ingest ("" when it can).
func settable(mod Model, c Column) string {
	switch {
	case ManagedColumn(c.Name):
		return "is managed by the kernel (id, organization_id, created_*, updated_*, deleted_*)"
	case c.Generated != "":
		return "is a generated column"
	case c.Sequence != "":
		return "is filled from a sequence"
	}
	for _, sn := range mod.Snapshots {
		for _, target := range sn.Fields {
			if target == c.Name {
				return "is filled by a snapshot"
			}
		}
	}
	return ""
}

// validateSetMap checks a set / on_success.set map against the target model.
// payload is the set of action field keys ($payload.<field> must be one).
func validateSetMap(where string, set map[string]any, mod Model, cols map[string]Column, payload map[string]struct{}, rowContext bool) []string {
	var errs []string
	if len(set) == 0 {
		return []string{where + " needs at least one column"}
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		c, ok := cols[k]
		if !ok {
			errs = append(errs, fmt.Sprintf("%s: %q is not a column of model %q", where, k, mod.Key))
		} else if why := settable(mod, c); why != "" {
			errs = append(errs, fmt.Sprintf("%s: column %q %s", where, k, why))
		}
		sv, err := ParseSetValue(set[k])
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s.%s: %v", where, k, err))
			continue
		}
		switch sv.Kind {
		case SetPayload:
			if _, ok := payload[sv.Name]; !ok {
				errs = append(errs, fmt.Sprintf("%s.%s: $payload.%s is not a field declared in the action's fields/steps", where, k, sv.Name))
			}
		case SetRecord:
			if _, ok := cols[sv.Name]; !ok {
				errs = append(errs, fmt.Sprintf("%s.%s: $record.%s is not a column of model %q", where, k, sv.Name, mod.Key))
			}
			if !rowContext {
				errs = append(errs, fmt.Sprintf("%s.%s: $record.* needs a row action", where, k))
			}
		}
	}
	return errs
}

func actionPayloadFields(a Action) map[string]struct{} {
	out := map[string]struct{}{}
	for _, f := range a.Fields {
		out[f.Key] = struct{}{}
	}
	for _, st := range a.Steps {
		for _, f := range st.Fields {
			out[f.Key] = struct{}{}
		}
	}
	return out
}

func isRowPlacement(p string) bool { return p == "" || p == "row" }

// validateHubActions enforces RFC-01/02 on contributions.actions[] and rejects
// the action-only handler fields on every other handler site.
func validateHubActions(m *Manifest, tolerate func(rule, msg string)) []string {
	if m.Contributions == nil {
		return nil
	}
	var errs []string
	ix := newModelIndex(m)

	// Other handler sites: webhook dispatch / set are action-only in this version.
	reject := func(where string, h Handler) {
		if f := actionOnlyHandlerFields(h); len(f) > 0 {
			errs = append(errs, fmt.Sprintf("%s.handler: %s only allowed on contributions.actions[] handlers", where, strings.Join(f, ", ")))
		}
		if h.Type == "set_fields" {
			errs = append(errs, fmt.Sprintf("%s.handler.type \"set_fields\" is only allowed on contributions.actions[]", where))
		}
	}
	for i, t := range m.Contributions.Tools {
		reject(fmt.Sprintf("contributions.tools[%d]", i), t.Handler)
	}
	for i, s := range m.Contributions.Subscriptions {
		reject(fmt.Sprintf("contributions.subscriptions[%d]", i), s.Handler)
	}
	for i, pc := range m.ProvidesCapabilities {
		reject(fmt.Sprintf("provides_capabilities[%d]", i), pc.Handler)
	}

	connCred := map[string]Setting{}
	for _, c := range m.Connectors {
		for _, cr := range c.Credentials {
			connCred[c.Key+"."+cr.Key] = cr
		}
	}
	settings := map[string]Setting{}
	for _, s := range m.Settings {
		settings[s.Key] = s
	}
	hasFetchRef := func(ref string) bool {
		for _, c := range m.Capabilities {
			if c.Kind == "http:fetch" && c.Target == "connector:"+ref {
				return true
			}
		}
		return false
	}
	hasFixedHostFetch := func() bool {
		for _, c := range m.Capabilities {
			if c.Kind == "http:fetch" && c.Target != "" && !strings.HasPrefix(c.Target, "connector:") {
				return true
			}
		}
		return false
	}

	for ai, a := range m.Contributions.Actions {
		where := fmt.Sprintf("contributions.actions[%d]", ai)
		h := a.Handler
		mod, ownModel := ix.byKey[a.TargetModel]
		cols := ix.cols[a.TargetModel]
		payload := actionPayloadFields(a)

		if a.ConfirmTyped != nil {
			switch {
			case strings.TrimSpace(a.ConfirmTyped.Field) == "":
				errs = append(errs, where+".confirm_typed.field is required")
			case a.TargetModel == "" || !isRowPlacement(a.Placement):
				errs = append(errs, where+".confirm_typed needs a row action with a target_model (there is no record to name otherwise)")
			case ownModel:
				if _, ok := cols[a.ConfirmTyped.Field]; !ok {
					errs = append(errs, fmt.Sprintf("%s.confirm_typed.field %q is not a column of model %q", where, a.ConfirmTyped.Field, a.TargetModel))
				}
			}
		}

		if h.Type != "webhook" {
			if f := handlerWebhookFields(h); len(f) > 0 {
				errs = append(errs, fmt.Sprintf("%s.handler: %s only allowed when type=webhook", where, strings.Join(f, ", ")))
			}
		}
		if h.Type != "set_fields" && h.Set != nil {
			errs = append(errs, where+".handler.set is only allowed when type=set_fields")
		}

		switch h.Type {
		case "set_fields":
			sw := where + ".handler"
			if h.Function != "" || h.URL != "" || h.Operation != "" || h.Connector != "" || h.Export != "" || h.Capability != "" || len(h.Input) > 0 {
				errs = append(errs, sw+": type=set_fields cannot declare function, url, operation, connector, export, capability or input")
			}
			switch {
			case a.TargetModel == "":
				errs = append(errs, sw+": type=set_fields needs the action's target_model")
			case !ownModel:
				errs = append(errs, fmt.Sprintf("%s: type=set_fields target_model %q is not a model of this addon", sw, a.TargetModel))
			case !isRowPlacement(a.Placement):
				errs = append(errs, fmt.Sprintf("%s: type=set_fields only works as a row action (placement %q)", sw, a.Placement))
			case mod.AppendOnly:
				errs = append(errs, fmt.Sprintf("%s: type=set_fields cannot target append_only model %q", sw, a.TargetModel))
			default:
				errs = append(errs, validateSetMap(sw+".set", h.Set, mod, cols, payload, true)...)
			}

		case "webhook":
			errs = append(errs, validateWebhookHandler(where, a, ix, connCred, settings, hasFetchRef, hasFixedHostFetch, tolerate)...)
		}
	}

	// Tone is a closed enum in the schema; nothing else to check.
	return errs
}

func validateWebhookHandler(
	where string, a Action, ix modelIndex,
	connCred map[string]Setting, settings map[string]Setting,
	hasFetchRef func(string) bool, hasFixedHostFetch func() bool,
	tolerate func(rule, msg string),
) []string {
	var errs []string
	h := a.Handler
	hw := where + ".handler"
	mod, ownModel := ix.byKey[a.TargetModel]
	cols := ix.cols[a.TargetModel]
	row := a.TargetModel != "" && isRowPlacement(a.Placement)

	// Destination: at most one source (a hard error: before this RFC there was
	// only `url`); none or a non-https literal is the tolerated rule.
	sources := 0
	for _, s := range []string{h.URL, h.URLRef, h.URLSetting} {
		if s != "" {
			sources++
		}
	}
	if sources > 1 {
		errs = append(errs, hw+": url, url_ref and url_setting are mutually exclusive")
	}
	if sources == 0 {
		tolerate(RuleWebhookActionSigning, hw+": a webhook action needs a destination: url (https), url_ref (\"<connector>.<credential>\") or url_setting (a settings[] key)")
	}
	if h.URL != "" && !strings.HasPrefix(strings.ToLower(h.URL), "https://") {
		tolerate(RuleWebhookActionSigning, fmt.Sprintf("%s.url %q must be an absolute https:// URL", hw, h.URL))
	}
	if h.URLRef != "" {
		cred, ok := connCred[h.URLRef]
		switch {
		case !connectorRefRe.MatchString(h.URLRef):
			errs = append(errs, fmt.Sprintf("%s.url_ref %q must be \"<connector>.<credential>\"", hw, h.URLRef))
		case !ok:
			errs = append(errs, fmt.Sprintf("%s.url_ref %q references an undeclared connector credential", hw, h.URLRef))
		case cred.Type != "string":
			errs = append(errs, fmt.Sprintf("%s.url_ref %q must reference a credential of type \"string\" (the base URL), not %q", hw, h.URLRef, cred.Type))
		}
		if !hasFetchRef(h.URLRef) {
			errs = append(errs, fmt.Sprintf("%s.url_ref %q requires a capability {kind: \"http:fetch\", target: \"connector:%s\"}", hw, h.URLRef, h.URLRef))
		}
	}
	if h.URLSetting != "" {
		st, ok := settings[h.URLSetting]
		switch {
		case !ok:
			errs = append(errs, fmt.Sprintf("%s.url_setting %q is not a key of settings[]", hw, h.URLSetting))
		case st.Type != "string":
			errs = append(errs, fmt.Sprintf("%s.url_setting %q must be a setting of type \"string\", not %q", hw, h.URLSetting, st.Type))
		}
		if !hasFixedHostFetch() {
			errs = append(errs, hw+".url_setting requires at least one http:fetch capability with a fixed host (the resolved host must be in that allowlist)")
		}
	}

	// Path template.
	if h.Path != "" {
		switch {
		case h.URL != "":
			errs = append(errs, hw+".path is appended to url_ref/url_setting; a literal url is already complete")
		case h.URLRef == "" && h.URLSetting == "":
			errs = append(errs, hw+".path needs url_ref or url_setting as its base")
		}
		if !strings.HasPrefix(h.Path, "/") || strings.ContainsAny(h.Path, "?# \t\r\n\\") {
			errs = append(errs, fmt.Sprintf("%s.path %q must start with \"/\" and contain no query, fragment, spaces or backslashes", hw, h.Path))
		}
		stripped := pathTemplateRe.ReplaceAllString(h.Path, "")
		if strings.ContainsAny(stripped, "{}") {
			errs = append(errs, fmt.Sprintf("%s.path %q has an unbalanced {placeholder}", hw, h.Path))
		}
		payload := actionPayloadFields(a)
		for _, ph := range pathTemplateRe.FindAllString(h.Path, -1) {
			inner := ph[1 : len(ph)-1]
			switch {
			case strings.HasPrefix(inner, "record."):
				col := strings.TrimPrefix(inner, "record.")
				if !row {
					errs = append(errs, fmt.Sprintf("%s.path placeholder %s needs a row action with a target_model", hw, ph))
				} else if ownModel {
					if _, ok := cols[col]; !ok {
						errs = append(errs, fmt.Sprintf("%s.path placeholder %s: %q is not a column of model %q", hw, ph, col, a.TargetModel))
					}
				}
			case strings.HasPrefix(inner, "payload."):
				f := strings.TrimPrefix(inner, "payload.")
				if _, ok := payload[f]; !ok {
					errs = append(errs, fmt.Sprintf("%s.path placeholder %s: %q is not a field declared in the action's fields/steps", hw, ph, f))
				}
			default:
				errs = append(errs, fmt.Sprintf("%s.path placeholder %s must be {record.<column>} or {payload.<field>}", hw, ph))
			}
		}
	}

	if h.Method != "" {
		if _, ok := webhookMethods[h.Method]; !ok {
			errs = append(errs, fmt.Sprintf("%s.method %q is not one of POST|PUT|PATCH|DELETE", hw, h.Method))
		}
	}

	// Signing.
	if h.Signing == nil {
		tolerate(RuleWebhookActionSigning, hw+": a webhook action must declare signing (scheme metacore-hmac-v1 with a secret_ref, or instance-ed25519-v1)")
	} else {
		sg := h.Signing
		if _, ok := signingSchemes[sg.Scheme]; !ok {
			errs = append(errs, fmt.Sprintf("%s.signing.scheme %q is not one of metacore-hmac-v1|instance-ed25519-v1", hw, sg.Scheme))
		}
		switch sg.Scheme {
		case "metacore-hmac-v1":
			cred, ok := connCred[sg.SecretRef]
			switch {
			case sg.SecretRef == "":
				errs = append(errs, hw+".signing.secret_ref is required with metacore-hmac-v1 (\"<connector>.<credential>\")")
			case !connectorRefRe.MatchString(sg.SecretRef):
				errs = append(errs, fmt.Sprintf("%s.signing.secret_ref %q must be \"<connector>.<credential>\"", hw, sg.SecretRef))
			case !ok:
				errs = append(errs, fmt.Sprintf("%s.signing.secret_ref %q references an undeclared connector credential", hw, sg.SecretRef))
			case cred.Type != "secret":
				errs = append(errs, fmt.Sprintf("%s.signing.secret_ref %q must reference a credential of type \"secret\", not %q", hw, sg.SecretRef, cred.Type))
			}
		case "instance-ed25519-v1":
			if sg.SecretRef != "" {
				errs = append(errs, hw+".signing.secret_ref is not allowed with instance-ed25519-v1 (the instance key signs; there is no shared secret)")
			}
		}
	}

	// send_record.
	seen := map[string]struct{}{}
	for _, c := range h.SendRecord {
		if _, dup := seen[c]; dup {
			errs = append(errs, fmt.Sprintf("%s.send_record %q is duplicated", hw, c))
		}
		seen[c] = struct{}{}
		if !row {
			errs = append(errs, hw+".send_record needs a row action with a target_model")
			break
		}
		if ownModel {
			if _, ok := cols[c]; !ok {
				errs = append(errs, fmt.Sprintf("%s.send_record %q is not a column of model %q", hw, c, a.TargetModel))
			}
		}
	}

	if h.TimeoutMS != 0 && (h.TimeoutMS < 1000 || h.TimeoutMS > 30000) {
		errs = append(errs, fmt.Sprintf("%s.timeout_ms %d must be between 1000 and 30000", hw, h.TimeoutMS))
	}
	if h.Retries != nil && (*h.Retries < 0 || *h.Retries > 3) {
		errs = append(errs, fmt.Sprintf("%s.retries %d must be between 0 and 3", hw, *h.Retries))
	}

	if h.OnSuccess != nil {
		ow := hw + ".on_success"
		switch {
		case !row:
			errs = append(errs, ow+" needs a row action with a target_model")
		case !ownModel:
			errs = append(errs, fmt.Sprintf("%s: target_model %q is not a model of this addon", ow, a.TargetModel))
		case mod.AppendOnly:
			errs = append(errs, fmt.Sprintf("%s: cannot write append_only model %q", ow, a.TargetModel))
		default:
			errs = append(errs, validateSetMap(ow+".set", h.OnSuccess.Set, mod, cols, actionPayloadFields(a), true)...)
		}
	}
	return errs
}

// ---- ingest (RFC-04) ----

func isCursorColumnType(t string) bool {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "bigint", "timestamp", "timestamptz":
		return true
	}
	return false
}

func normWhere(w string) string {
	return strings.ToLower(strings.Join(strings.Fields(w), " "))
}

func validateIngest(m *Manifest, rls string) []string {
	var errs []string
	for mi, mod := range m.Models {
		ig := mod.Ingest
		if ig == nil {
			continue
		}
		where := fmt.Sprintf("models[%d] (%s).ingest", mi, mod.Key)
		cols := make(map[string]Column, len(mod.Columns))
		for _, c := range mod.Columns {
			cols[c.Name] = c
		}
		if mod.AppendOnly {
			errs = append(errs, where+": an append_only model cannot be an upsert sink (updates are refused)")
		}
		if mod.Extends != "" {
			errs = append(errs, where+": an extension table (extends) cannot be an upsert sink")
		}

		// Natural key.
		if n := len(ig.Key); n < 1 || n > 4 {
			errs = append(errs, fmt.Sprintf("%s.key needs 1 to 4 columns, got %d", where, n))
		}
		keySet := map[string]struct{}{}
		for _, k := range ig.Key {
			if _, dup := keySet[k]; dup {
				errs = append(errs, fmt.Sprintf("%s.key %q is duplicated", where, k))
			}
			keySet[k] = struct{}{}
			if c, ok := cols[k]; !ok {
				errs = append(errs, fmt.Sprintf("%s.key %q is not a column of the model", where, k))
			} else if why := settable(mod, c); why != "" {
				errs = append(errs, fmt.Sprintf("%s.key %q %s", where, k, why))
			}
		}
		if len(ig.Key) > 0 && len(keySet) == len(ig.Key) {
			want := map[string]struct{}{rls: {}}
			for k := range keySet {
				want[k] = struct{}{}
			}
			covered := false
			for _, ix := range mod.Indices {
				if !ix.Unique || len(ix.Columns) != len(want) {
					continue
				}
				w := normWhere(ix.Where)
				if w != "" && w != "deleted_at is null" {
					continue
				}
				all := true
				for _, c := range ix.Columns {
					if _, ok := want[c]; !ok {
						all = false
					}
				}
				if all {
					covered = true
					break
				}
			}
			if !covered {
				errs = append(errs, fmt.Sprintf("%s.key needs a UNIQUE index over exactly (%s, %s) (a partial index WHERE deleted_at IS NULL is fine): without it concurrent batches can duplicate rows",
					where, rls, strings.Join(ig.Key, ", ")))
			}
		}

		// Cursor.
		if ig.Cursor != "" {
			c, ok := cols[ig.Cursor]
			switch {
			case !ok:
				errs = append(errs, fmt.Sprintf("%s.cursor %q is not a column of the model", where, ig.Cursor))
			case !isCursorColumnType(c.Type):
				errs = append(errs, fmt.Sprintf("%s.cursor %q must be a bigint or timestamp/timestamptz column, not %q", where, ig.Cursor, c.Type))
			default:
				if _, isKey := keySet[ig.Cursor]; isKey {
					errs = append(errs, fmt.Sprintf("%s.cursor %q cannot also be part of the key", where, ig.Cursor))
				}
				if why := settable(mod, c); why != "" {
					errs = append(errs, fmt.Sprintf("%s.cursor %q %s", where, ig.Cursor, why))
				}
			}
		}

		// Writable fields.
		seenF := map[string]struct{}{}
		for _, f := range ig.Fields {
			if _, dup := seenF[f]; dup {
				errs = append(errs, fmt.Sprintf("%s.fields %q is duplicated", where, f))
			}
			seenF[f] = struct{}{}
			c, ok := cols[f]
			if !ok {
				errs = append(errs, fmt.Sprintf("%s.fields %q is not a column of the model", where, f))
				continue
			}
			if why := settable(mod, c); why != "" {
				errs = append(errs, fmt.Sprintf("%s.fields %q %s", where, f, why))
			}
		}

		switch ig.OnDelete {
		case "", "soft", "ignore":
		default:
			errs = append(errs, fmt.Sprintf("%s.on_delete %q is not one of soft|ignore", where, ig.OnDelete))
		}
		if ig.MaxBatch != 0 && (ig.MaxBatch < 1 || ig.MaxBatch > 1000) {
			errs = append(errs, fmt.Sprintf("%s.max_batch %d must be between 1 and 1000", where, ig.MaxBatch))
		}
	}
	return errs
}

// validateWebhookDo validates a webhooks[].do: the shared wasm|webhook|compiled
// set plus "upsert:<Model>" (RFC-04), which only inbound webhooks may use.
func validateWebhookDo(w InboundWebhook, models map[string]Model) string {
	prefix, rest, found := strings.Cut(w.Do, ":")
	if !found || prefix != "upsert" {
		return validateDoRef(w.Do)
	}
	if rest == "" {
		return fmt.Sprintf("%q must be upsert:<Model>", w.Do)
	}
	mod, ok := models[rest]
	switch {
	case !ok:
		return fmt.Sprintf("%q targets %q, which is not a model of this addon", w.Do, rest)
	case mod.Ingest == nil:
		return fmt.Sprintf("%q targets model %q, which has no ingest block", w.Do, rest)
	case w.Verify == "" || w.Verify == "addon":
		return fmt.Sprintf("%q needs verify (hmac-sha256, bearer or metacore-hmac-v1): the kernel verifies the request, there is no guest to do it", w.Do)
	}
	return ""
}

// ---- dashboard (RFC-08 §3) ----

func validateDashboardHub(m *Manifest) []string {
	if m.Contributions == nil || len(m.Contributions.Dashboard) == 0 {
		return nil
	}
	var errs []string
	ix := newModelIndex(m)
	for i, w := range m.Contributions.Dashboard {
		id := w.Key
		if id == "" {
			id = fmt.Sprintf("#%d", i)
		}
		where := fmt.Sprintf("contributions.dashboard[%s]", id)
		if w.Query != nil {
			errs = append(errs, validateNowTokens(where+".query", w.Query, ix)...)
		}
		if w.Link != nil {
			errs = append(errs, validateWidgetLink(where+".link", w.Link, ix)...)
		}
	}
	return errs
}

// NowTokenOffset parses "$now" / "$now-<dur>" and returns the offset in
// minutes (0 for "$now"). ok is false when s is not a well-formed token.
func NowTokenOffset(s string) (minutes int, ok bool) {
	g := nowTokenRe.FindStringSubmatch(s)
	if g == nil {
		return 0, false
	}
	if g[1] == "" {
		return 0, true
	}
	n, err := strconv.Atoi(g[1])
	if err != nil || n <= 0 {
		return 0, false
	}
	switch g[2] {
	case "m":
		minutes = n
	case "h":
		minutes = n * 60
	case "d":
		minutes = n * 60 * 24
	}
	if minutes > maxNowDays*24*60 {
		return 0, false
	}
	return minutes, true
}

func validateNowTokens(where string, q *WidgetQuery, ix modelIndex) []string {
	var errs []string
	cols := ix.cols[q.Model]
	keys := make([]string, 0, len(q.Where))
	for k := range q.Where {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, col := range keys {
		raw := q.Where[col]
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			continue
		}
		switch x := v.(type) {
		case string:
			if strings.HasPrefix(x, "$now") {
				errs = append(errs, fmt.Sprintf("%s.where.%s: %q only works with an operator, e.g. {\"gte\": %q}", where, col, x, x))
			}
		case map[string]any:
			ops := make([]string, 0, len(x))
			for op := range x {
				ops = append(ops, op)
			}
			sort.Strings(ops)
			for _, op := range ops {
				s, isStr := x[op].(string)
				if !isStr || !strings.HasPrefix(s, "$now") {
					continue
				}
				ww := fmt.Sprintf("%s.where.%s.%s", where, col, op)
				switch op {
				case "gt", "gte", "lt", "lte":
				default:
					errs = append(errs, fmt.Sprintf("%s: %q is only allowed with gt|gte|lt|lte", ww, s))
					continue
				}
				if _, ok := NowTokenOffset(s); !ok {
					errs = append(errs, fmt.Sprintf("%s: %q must be $now or $now-<n>(m|h|d) with at most %dd", ww, s, maxNowDays))
				}
				c, ok := cols[col]
				switch {
				case !ok:
					errs = append(errs, fmt.Sprintf("%s: %q is not a column of model %q", ww, col, q.Model))
				case !isTemporalColumnType(c.Type):
					errs = append(errs, fmt.Sprintf("%s: %s tokens need a date/timestamp/timestamptz column, %q is %q", ww, "$now", col, c.Type))
				}
			}
		}
	}
	return errs
}

func validateWidgetLink(where string, l *WidgetLink, ix modelIndex) []string {
	var errs []string
	switch {
	case l.Model == "" && l.URL == "":
		return []string{where + " needs model (with an optional filter) or url"}
	case l.Model != "" && l.URL != "":
		return []string{where + ": model and url are mutually exclusive"}
	}
	if l.Model != "" {
		cols, ok := ix.cols[l.Model]
		if !ok {
			return []string{fmt.Sprintf("%s.model %q is not a model of this addon", where, l.Model)}
		}
		keys := make([]string, 0, len(l.Filter))
		for k := range l.Filter {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if _, ok := cols[k]; !ok {
				errs = append(errs, fmt.Sprintf("%s.filter.%s is not a column of model %q", where, k, l.Model))
			}
			switch l.Filter[k].(type) {
			case string, bool, float64:
			default:
				errs = append(errs, fmt.Sprintf("%s.filter.%s must be a string, number or boolean", where, k))
			}
		}
		return errs
	}
	if len(l.Filter) > 0 {
		errs = append(errs, where+".filter needs model (a url already carries its own query)")
	}
	switch {
	case !strings.HasPrefix(l.URL, "/") || strings.HasPrefix(l.URL, "//"):
		errs = append(errs, fmt.Sprintf("%s.url %q must be a relative internal route starting with a single \"/\" (never absolute)", where, l.URL))
	case linkURLForbidRe.MatchString(l.URL):
		errs = append(errs, fmt.Sprintf("%s.url %q contains control characters or backslashes", where, l.URL))
	case len(l.URL) > maxLinkURLChars:
		errs = append(errs, fmt.Sprintf("%s.url is longer than %d characters", where, maxLinkURLChars))
	}
	return errs
}

// ---- RBAC (RFC-06 §A) ----

var rbacCapabilityVerbs = map[string]struct{}{
	"read": {}, "write": {}, "create": {}, "update": {}, "delete": {},
	"index": {}, "show": {}, "export": {}, "import": {},
}

func rbacSnakeWords(s string) []string {
	var words []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			words = append(words, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	rs := []rune(s)
	isUpper := func(r rune) bool { return r >= 'A' && r <= 'Z' }
	isLetterLower := func(r rune) bool { return r >= 'a' && r <= 'z' }
	for i, r := range rs {
		switch {
		case r == '_' || r == '-' || r == ' ' || r == '.':
			flush()
		case isUpper(r):
			prevLower := i > 0 && (isLetterLower(rs[i-1]) || (rs[i-1] >= '0' && rs[i-1] <= '9'))
			acronymEnd := i > 0 && isUpper(rs[i-1]) && i+1 < len(rs) && isLetterLower(rs[i+1])
			if prevLower || acronymEnd {
				flush()
			}
			cur = append(cur, r)
		default:
			cur = append(cur, r)
		}
	}
	flush()
	return words
}

// modelResourceNames: segments a capability may use for a model — every
// contiguous run of words of its key and table, joined with "_" and "", with
// the last word in singular and plural form (the host accepts the singular;
// the plural is tolerated here so a plausible spelling is not flagged).
func modelResourceNames(mod Model) map[string]struct{} {
	out := map[string]struct{}{}
	for _, src := range []string{mod.Key, mod.Table} {
		words := rbacSnakeWords(src)
		if len(words) == 0 {
			continue
		}
		last := words[len(words)-1]
		forms := []string{last}
		switch {
		case strings.HasSuffix(last, "ies") && len(last) > 3:
			forms = append(forms, last[:len(last)-3]+"y")
		case strings.HasSuffix(last, "s") && !strings.HasSuffix(last, "ss") && len(last) > 2:
			forms = append(forms, last[:len(last)-1])
		}
		if strings.HasSuffix(last, "es") && len(last) > 4 && !strings.HasSuffix(last, "ies") {
			forms = append(forms, last[:len(last)-2])
		}
		for _, form := range forms {
			ws := append([]string(nil), words...)
			ws[len(ws)-1] = form
			for i := range ws {
				for j := i + 1; j <= len(ws); j++ {
					out[strings.Join(ws[i:j], "_")] = struct{}{}
					out[strings.Join(ws[i:j], "")] = struct{}{}
				}
			}
		}
	}
	return out
}

func rbacAddonPrefixMatches(prefix, owner string) bool {
	norm := func(s string) string { return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), "-", "_") }
	p, o := norm(prefix), norm(owner)
	if p == "" || o == "" {
		return false
	}
	if p == o {
		return true
	}
	o = strings.TrimPrefix(o, "link_")
	o = strings.TrimSuffix(o, "_lite")
	return p == o
}

func validateRBACPermissionResolves(m *Manifest, tolerate func(rule, msg string)) {
	if m.RBAC == nil || len(m.RBAC.Roles) == 0 {
		return
	}
	declared := map[string]struct{}{}
	for _, p := range m.RBAC.Permissions {
		declared[p.Key] = struct{}{}
	}
	resources := map[string]struct{}{}
	for _, mod := range m.Models {
		for n := range modelResourceNames(mod) {
			resources[n] = struct{}{}
		}
	}
	actionKeys := map[string]struct{}{}
	if m.Contributions != nil {
		for _, a := range m.Contributions.Actions {
			actionKeys[strings.ToLower(a.Key)] = struct{}{}
		}
	}
	for ri, role := range m.RBAC.Roles {
		for pi, key := range role.Permissions {
			if _, ok := declared[key]; ok {
				continue
			}
			parts := strings.Split(strings.ToLower(strings.TrimSpace(key)), ".")
			if len(parts) != 3 || !rbacAddonPrefixMatches(parts[0], m.Metadata.Key) {
				continue // another addon's key / canonical form: not judgeable here
			}
			_, resOK := resources[parts[1]]
			_, verbOK := rbacCapabilityVerbs[parts[2]]
			if !verbOK {
				_, verbOK = actionKeys[parts[2]]
			}
			if resOK && verbOK {
				continue
			}
			tolerate(RuleRBACPermissionResolves, fmt.Sprintf(
				"rbac.roles[%d].permissions[%d] %q is not declared in rbac.permissions and does not resolve to a model of this addon "+
					"(want %s.<model>.<read|write|create|update|delete|index|show|export|import|<action key>>): the role would grant nothing. "+
					"Fix the typo or declare {\"key\": %q, \"models\": [{\"model\": \"<Model>\", \"actions\": [...]}]} in rbac.permissions",
				ri, pi, key, m.Metadata.Key, key))
		}
	}
}
