package inbound

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// RFC 0003 §2.4: a contract may declare a short chain of actions instead of a
// single Route. The list of actions is CLOSED (adding one is a kernel change
// with tests); the chain itself is data. Templates are validated when the
// contract is registered, never when an event arrives.

// Closed list of actions.
const (
	ActionRecordCreate = "record.create"
	ActionRecordUpdate = "record.update"
	ActionRecordUpsert = "record.upsert"
	ActionEventEmit    = "event.emit"
	ActionNotify       = "notify"
	ActionCommandRun   = "command.run"
	// ActionAutomationEnqueue hands an event to the automation trigger
	// (Scheduler.Ingest) through AutomationIngester.
	ActionAutomationEnqueue = "automation.enqueue"
)

// On-error policies.
const (
	OnErrorStop     = "stop"
	OnErrorContinue = "continue"
)

const maxActions = 10

// Action is one step of a contract chain. Which fields apply depends on Do;
// a field that does not belong to the action is a registration error.
type Action struct {
	Do string `json:"do"`
	// When gates the action; a false condition records the step as skipped.
	When *Condition `json:"when,omitempty"`
	// OnError overrides the contract's on_error for this step (stop|continue).
	OnError string `json:"on_error,omitempty"`

	// record.create|update|upsert
	Model    string            `json:"model,omitempty"`
	Match    []string          `json:"match,omitempty"`    // keys of Mapping
	Mapping  map[string]string `json:"mapping,omitempty"`  // target field -> template
	Defaults map[string]any    `json:"defaults,omitempty"` // constants
	Scope    *ScopeMapping     `json:"scope,omitempty"`    // payload paths (not templates)

	// event.emit | automation.enqueue (event name)
	Name string `json:"name,omitempty"`
	// Payload is the body of event.emit / the params of notify; string leaves
	// may hold templates.
	Payload map[string]any `json:"payload,omitempty"`

	// notify
	Template string `json:"template,omitempty"`

	// command.run (Op is passed to the command; default create)
	Command string `json:"command,omitempty"`
	Op      Op     `json:"op,omitempty"`
}

// Condition is a declarative guard: exactly one of Eq or Exists.
//
//	{"path":"result.created","eq":true}
//	{"path":"payload.email","exists":true}
type Condition struct {
	Path   string          `json:"path"`
	Eq     json.RawMessage `json:"eq,omitempty"`
	Exists *bool           `json:"exists,omitempty"`
}

// Step statuses recorded in the receipt.
const (
	StepDone    = "done"
	StepSkipped = "skipped"
	StepError   = "error"  // failed but the chain continued (on_error: continue)
	StepFailed  = "failed" // failed and stopped the chain; re-run on retry
)

// ActionStep is the per-action progress kept in the receipt. It carries ids
// and codes only, never payload values.
type ActionStep struct {
	Index     int    `json:"index"`
	Do        string `json:"do"`
	Status    string `json:"status"`
	RecordID  string `json:"record_id,omitempty"`
	Created   bool   `json:"created,omitempty"`
	ErrorCode string `json:"error_code,omitempty"`
}

// ------------------------------------------------------------ side ports ---

// EmitRequest is what event.emit hands to the host's emitter. Origin must be
// propagated with the event (e.g. as a header or field of the delivery) so a
// subscription can exclude it (see OriginExcluded). IdempotencyKey is stable
// per receipt+action: an emitter that can dedupe makes a crash between the
// effect and the progress write harmless.
type EmitRequest struct {
	Org            uuid.UUID
	Name           string
	Payload        map[string]any
	Origin         string
	IdempotencyKey string
}

// EventEmitter is the minimal port to the outbound side. The host adapts its
// event bus / webhooks to it; this package does not import them.
type EventEmitter interface {
	Emit(ctx context.Context, req EmitRequest) error
}

// NotifyRequest is what notify hands to the host's notifier.
type NotifyRequest struct {
	Org            uuid.UUID
	Template       string
	Params         map[string]any
	Origin         string
	IdempotencyKey string
}

// Notifier is the minimal port to notifications.
type Notifier interface {
	Notify(ctx context.Context, req NotifyRequest) error
}

// AutomationEvent mirrors automation.Event without importing that package.
// ID is stable per receipt+action, so the scheduler's idempotency per
// (automation, event) makes a repeated enqueue harmless.
type AutomationEvent struct {
	ID      string
	Org     uuid.UUID
	Name    string
	Payload map[string]any
	Origin  string
}

// AutomationIngester is satisfied by a thin host adapter over
// automation.Scheduler.Ingest (its run count is ignored here).
type AutomationIngester interface {
	IngestEvent(ctx context.Context, ev AutomationEvent) error
}

// ---------------------------------------------------------------- origin ---

// OriginPrefix prefixes the origin mark of everything an inbound chain
// causes: "inbound:<contract>".
const OriginPrefix = "inbound:"

// OriginHeader is the HTTP header an outbound delivery uses to carry the
// origin mark; when it comes back to an inbound endpoint the loop is cut.
const OriginHeader = "X-Metacore-Origin"

type originKey struct{}

// WithOrigin stores the origin mark in ctx so adapters (emitter, bus) can
// read it with OriginFrom.
func WithOrigin(ctx context.Context, origin string) context.Context {
	return context.WithValue(ctx, originKey{}, origin)
}

// OriginFrom returns the origin mark in ctx, or "".
func OriginFrom(ctx context.Context) string {
	s, _ := ctx.Value(originKey{}).(string)
	return s
}

// OriginExcluded reports whether origin matches one of the exclusions. An
// entry is exact, or a prefix when it ends in "*" ("inbound:*"). Outbound
// subscriptions use it to skip events caused by an inbound chain.
func OriginExcluded(origin string, excluded []string) bool {
	if origin == "" {
		return false
	}
	for _, e := range excluded {
		if p, ok := strings.CutSuffix(e, "*"); ok {
			if strings.HasPrefix(origin, p) {
				return true
			}
		} else if e == origin {
			return true
		}
	}
	return false
}

// --------------------------------------------------------------- compile ---

type cAction struct {
	Action
	mapping map[string]node
	payload map[string]node
	when    *cCond
	onErr   string
}

type cCond struct {
	ns, path string
	eq       any
	hasEq    bool
	exists   bool
	wantsEx  bool
}

// validator checks template references against what exists at that point of
// the chain.
type validator struct {
	schema    any // decoded contract JSON Schema
	hasResult bool
}

var (
	tplRe   = regexp.MustCompile(`\{\{(.*?)\}\}`)
	eventOK = map[string]bool{"key": true, "contract": true, "version": true, "org": true}
	resOK   = map[string]bool{"id": true, "created": true}
)

// compileActions validates the chain of a contract.
func compileActions(c Contract, schema json.RawMessage) ([]*cAction, error) {
	if len(c.Actions) > maxActions {
		return nil, invalidf("%s: at most %d actions", c.Key, maxActions)
	}
	if c.OnError != "" && c.OnError != OnErrorStop && c.OnError != OnErrorContinue {
		return nil, invalidf("%s: on_error must be stop|continue", c.Key)
	}
	var sch any
	if err := json.Unmarshal(schema, &sch); err != nil {
		return nil, invalidf("%s: json_schema: %v", c.Key, err)
	}
	out := make([]*cAction, 0, len(c.Actions))
	for i, a := range c.Actions {
		v := &validator{schema: sch, hasResult: i > 0}
		ca, err := compileAction(a, v)
		if err != nil {
			return nil, invalidf("%s: actions[%d] (%s): %v", c.Key, i, a.Do, err)
		}
		ca.onErr = c.OnError
		if a.OnError != "" {
			if a.OnError != OnErrorStop && a.OnError != OnErrorContinue {
				return nil, invalidf("%s: actions[%d]: on_error must be stop|continue", c.Key, i)
			}
			ca.onErr = a.OnError
		}
		if ca.onErr == "" {
			ca.onErr = OnErrorStop
		}
		out = append(out, ca)
	}
	return out, nil
}

func compileAction(a Action, v *validator) (*cAction, error) {
	ca := &cAction{Action: a}
	isRecord := false
	switch a.Do {
	case ActionRecordCreate, ActionRecordUpdate, ActionRecordUpsert:
		isRecord = true
	case ActionEventEmit, ActionNotify, ActionCommandRun, ActionAutomationEnqueue:
	default:
		return nil, fmt.Errorf("unknown action %q", a.Do)
	}
	// Fields foreign to the action are rejected, not ignored.
	foreign := func(name string, set bool, ok bool) error {
		if set && !ok {
			return fmt.Errorf("field %q does not apply to %s", name, a.Do)
		}
		return nil
	}
	isEmit, isNotify, isCmd := a.Do == ActionEventEmit || a.Do == ActionAutomationEnqueue, a.Do == ActionNotify, a.Do == ActionCommandRun
	for _, f := range []struct {
		n       string
		set, ok bool
	}{
		{"model", a.Model != "", isRecord}, {"match", len(a.Match) > 0, isRecord},
		{"defaults", len(a.Defaults) > 0, isRecord}, {"scope", a.Scope != nil, isRecord},
		{"mapping", len(a.Mapping) > 0, isRecord || isCmd},
		{"name", a.Name != "", isEmit}, {"payload", len(a.Payload) > 0, isEmit || isNotify},
		{"template", a.Template != "", isNotify}, {"command", a.Command != "", isCmd}, {"op", a.Op != "", isCmd},
	} {
		if err := foreign(f.n, f.set, f.ok); err != nil {
			return nil, err
		}
	}
	switch {
	case isRecord:
		if a.Model == "" {
			return nil, fmt.Errorf("model required")
		}
		if len(a.Mapping) == 0 {
			return nil, fmt.Errorf("mapping is empty")
		}
		if a.Do != ActionRecordCreate && len(a.Match) == 0 {
			return nil, fmt.Errorf("%s needs match fields", a.Do)
		}
		for _, m := range a.Match {
			if _, ok := a.Mapping[m]; !ok {
				return nil, fmt.Errorf("match field %q is not in mapping", m)
			}
		}
		for to := range a.Defaults {
			if !fieldRe.MatchString(to) || forbidden[to] {
				return nil, fmt.Errorf("default field %q not allowed", to)
			}
		}
		if a.Scope != nil {
			for _, p := range []string{a.Scope.Branch, a.Scope.Entity} {
				if p == "" {
					continue
				}
				if _, err := parsePath(p); err != nil {
					return nil, fmt.Errorf("scope path: %v", err)
				}
				if !v.payloadHas(p) {
					return nil, fmt.Errorf("scope path %q does not exist in the schema", p)
				}
			}
		}
	case isEmit:
		if !keyRe.MatchString(a.Name) {
			return nil, fmt.Errorf("name %q must match %s", a.Name, keyRe)
		}
	case isNotify:
		if !keyRe.MatchString(a.Template) {
			return nil, fmt.Errorf("template %q must match %s", a.Template, keyRe)
		}
	case isCmd:
		if a.Command == "" {
			return nil, fmt.Errorf("command required")
		}
		switch a.Op {
		case "", OpCreate, OpUpdate, OpUpsert:
		default:
			return nil, fmt.Errorf("op must be create|update|upsert")
		}
	}
	if len(a.Mapping) > 0 {
		ca.mapping = make(map[string]node, len(a.Mapping))
		for to, t := range a.Mapping {
			if !fieldRe.MatchString(to) || forbidden[to] {
				return nil, fmt.Errorf("target field %q not allowed", to)
			}
			n, err := compileTemplate(t, v)
			if err != nil {
				return nil, fmt.Errorf("mapping %q: %v", to, err)
			}
			ca.mapping[to] = n
		}
	}
	if len(a.Payload) > 0 {
		ca.payload = make(map[string]node, len(a.Payload))
		for k, val := range a.Payload {
			n, err := compileValue(val, v)
			if err != nil {
				return nil, fmt.Errorf("payload %q: %v", k, err)
			}
			ca.payload[k] = n
		}
	}
	if a.When != nil {
		cc, err := compileCond(*a.When, v)
		if err != nil {
			return nil, fmt.Errorf("when: %v", err)
		}
		ca.when = cc
	}
	return ca, nil
}

func compileCond(c Condition, v *validator) (*cCond, error) {
	ns, path, err := v.ref(strings.TrimSpace(c.Path))
	if err != nil {
		return nil, err
	}
	cc := &cCond{ns: ns, path: path}
	if (len(c.Eq) > 0) == (c.Exists != nil) {
		return nil, fmt.Errorf("exactly one of eq|exists")
	}
	if c.Exists != nil {
		cc.wantsEx, cc.exists = true, *c.Exists
		return cc, nil
	}
	dec := json.NewDecoder(bytes.NewReader(c.Eq))
	dec.UseNumber()
	var val any
	if err := dec.Decode(&val); err != nil {
		return nil, fmt.Errorf("eq: %v", err)
	}
	switch val.(type) {
	case map[string]any, []any:
		return nil, fmt.Errorf("eq must be a scalar")
	}
	cc.hasEq, cc.eq = true, normalize(val)
	return cc, nil
}

// ref validates "payload.a.b" | "event.key" | "result.id" and splits it.
func (v *validator) ref(expr string) (ns, path string, err error) {
	ns, path, ok := strings.Cut(expr, ".")
	if !ok || path == "" {
		return "", "", fmt.Errorf("reference %q must be payload.<path>, event.<key> or result.<id|created>", expr)
	}
	if _, perr := parsePath(path); perr != nil {
		return "", "", perr
	}
	switch ns {
	case "payload":
		if !v.payloadHas(path) {
			return "", "", fmt.Errorf("payload.%s does not exist in the contract schema", path)
		}
	case "event":
		if !eventOK[path] {
			return "", "", fmt.Errorf("event.%s unknown (key|contract|version|org)", path)
		}
	case "result":
		if !v.hasResult {
			return "", "", fmt.Errorf("result.%s: the first action has no previous result", path)
		}
		if !resOK[path] {
			return "", "", fmt.Errorf("result.%s unknown (id|created)", path)
		}
	default:
		return "", "", fmt.Errorf("reference %q: namespace must be payload|event|result", expr)
	}
	return ns, path, nil
}

// payloadHas walks the JSON Schema statically. A path must be declared in
// `properties` (or `items`/`prefixItems` for indexes). When the schema is
// opaque at some node (no properties and open additionalProperties, or
// $ref/allOf/anyOf/oneOf/if) the rest of the path is accepted: it cannot be
// decided without running the event.
func (v *validator) payloadHas(path string) bool {
	segs, err := parsePath(path)
	if err != nil {
		return false
	}
	node := v.schema
	for _, s := range segs {
		m, ok := node.(map[string]any)
		if !ok {
			return true
		}
		if t, ok := m["type"].(string); ok && t != "object" && t != "array" {
			return false // a scalar has no children
		}
		for _, k := range []string{"$ref", "$dynamicRef", "allOf", "anyOf", "oneOf", "if", "then", "else"} {
			if _, has := m[k]; has {
				return true
			}
		}
		if idx, err := strconv.Atoi(s); err == nil && idx >= 0 {
			if pi, ok := m["prefixItems"].([]any); ok && idx < len(pi) {
				node = pi[idx]
			} else if it, ok := m["items"]; ok {
				node = it
			} else {
				return true
			}
			continue
		}
		if props, ok := m["properties"].(map[string]any); ok {
			child, ok := props[s]
			if !ok {
				return false
			}
			node = child
			continue
		}
		if ap, ok := m["additionalProperties"].(bool); ok && !ap {
			return false
		}
		return true
	}
	return true
}

// ------------------------------------------------------------- templates ---

// evalEnv is what a template sees.
type evalEnv struct {
	payload any
	event   map[string]any
	result  map[string]any // nil before any action produced one
}

func (e *evalEnv) resolve(ns, path string) (any, bool) {
	switch ns {
	case "payload":
		return lookup(e.payload, path)
	case "event":
		v, ok := e.event[path]
		return v, ok && v != nil
	case "result":
		v, ok := e.result[path]
		return v, ok && v != nil
	}
	return nil, false
}

type node interface {
	// eval returns the value; ok=false means "absent" (omitted from maps).
	eval(e *evalEnv) (v any, ok bool, err error)
}

type litNode struct{ v any }

func (n litNode) eval(*evalEnv) (any, bool, error) { return n.v, true, nil }

// refNode is a whole-value template ("{{payload.id}}"): keeps the JSON type.
type refNode struct{ ns, path string }

func (n refNode) eval(e *evalEnv) (any, bool, error) {
	v, ok := e.resolve(n.ns, n.path)
	if !ok {
		return nil, false, nil
	}
	return normalize(v), true, nil
}

// strNode mixes literals and references into a string.
type strNode struct{ parts []strPart }
type strPart struct {
	lit    string
	ns, pa string
}

func (n strNode) eval(e *evalEnv) (any, bool, error) {
	var b strings.Builder
	for _, p := range n.parts {
		if p.ns == "" {
			b.WriteString(p.lit)
			continue
		}
		v, ok := e.resolve(p.ns, p.pa)
		if !ok {
			return nil, false, &ExecError{Kind: KindInvalid, Code: "template_value_missing"}
		}
		s, ok := scalarOf(v)
		if !ok {
			return nil, false, &ExecError{Kind: KindInvalid, Code: "template_value_invalid"}
		}
		b.WriteString(s)
	}
	return b.String(), true, nil
}

func scalarOf(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case bool:
		return strconv.FormatBool(t), true
	case json.Number:
		return t.String(), true
	case int:
		return strconv.Itoa(t), true
	case int64:
		return strconv.FormatInt(t, 10), true
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), true
	}
	return "", false
}

type mapNode map[string]node

func (n mapNode) eval(e *evalEnv) (any, bool, error) {
	out := make(map[string]any, len(n))
	for k, c := range n {
		v, ok, err := c.eval(e)
		if err != nil {
			return nil, false, err
		}
		if ok {
			out[k] = v
		}
	}
	return out, true, nil
}

type listNode []node

func (n listNode) eval(e *evalEnv) (any, bool, error) {
	out := make([]any, len(n))
	for i, c := range n {
		v, _, err := c.eval(e)
		if err != nil {
			return nil, false, err
		}
		out[i] = v
	}
	return out, true, nil
}

func compileTemplate(s string, v *validator) (node, error) {
	ms := tplRe.FindAllStringSubmatchIndex(s, -1)
	rest := tplRe.ReplaceAllString(s, "")
	if strings.Contains(rest, "{{") || strings.Contains(rest, "}}") {
		return nil, fmt.Errorf("unbalanced {{ }} in %q", s)
	}
	if len(ms) == 0 {
		return litNode{s}, nil
	}
	var parts []strPart
	last := 0
	for _, m := range ms {
		if m[0] > last {
			parts = append(parts, strPart{lit: s[last:m[0]]})
		}
		ns, path, err := v.ref(strings.TrimSpace(s[m[2]:m[3]]))
		if err != nil {
			return nil, err
		}
		parts = append(parts, strPart{ns: ns, pa: path})
		last = m[1]
	}
	if last < len(s) {
		parts = append(parts, strPart{lit: s[last:]})
	}
	if len(parts) == 1 {
		return refNode{parts[0].ns, parts[0].pa}, nil
	}
	return strNode{parts}, nil
}

func compileValue(val any, v *validator) (node, error) {
	switch t := val.(type) {
	case string:
		return compileTemplate(t, v)
	case map[string]any:
		m := make(mapNode, len(t))
		for k, c := range t {
			n, err := compileValue(c, v)
			if err != nil {
				return nil, err
			}
			m[k] = n
		}
		return m, nil
	case []any:
		l := make(listNode, len(t))
		for i, c := range t {
			n, err := compileValue(c, v)
			if err != nil {
				return nil, err
			}
			l[i] = n
		}
		return l, nil
	}
	return litNode{val}, nil
}

func evalMap(m map[string]node, e *evalEnv) (map[string]any, error) {
	out := make(map[string]any, len(m))
	for k, n := range m {
		v, ok, err := n.eval(e)
		if err != nil {
			return nil, err
		}
		if ok {
			out[k] = v
		}
	}
	return out, nil
}

func (c *cCond) holds(e *evalEnv) bool {
	v, ok := e.resolve(c.ns, c.path)
	if c.wantsEx {
		return ok == c.exists
	}
	return ok && equalScalar(normalize(v), c.eq)
}

func equalScalar(a, b any) bool {
	an, aok := toFloat(a)
	bn, bok := toFloat(b)
	if aok || bok {
		return aok && bok && an == bn
	}
	return a == b
}

func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case float64:
		return t, true
	}
	return 0, false
}
