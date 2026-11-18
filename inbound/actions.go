package inbound

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	// ActionRecordDelete retires rows selected ONLY by match (never by id).
	ActionRecordDelete = "record.delete"
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

// record.delete modes and limits.
const (
	DeleteSoft = "soft"
	DeleteHard = "hard"
	// maxDeleteRowsCeiling is the highest max_rows a contract may declare;
	// the executor applies its own (host-configurable) cap on top.
	maxDeleteRowsCeiling = 100
)

// Action is one step of a contract chain. Which fields apply depends on Do;
// a field that does not belong to the action is a registration error.
type Action struct {
	Do string `json:"do"`
	// When gates the action; a false condition records the step as skipped.
	When *Condition `json:"when,omitempty"`
	// OnError overrides the contract's on_error for this step (stop|continue).
	OnError string `json:"on_error,omitempty"`

	// record.create|update|upsert|delete
	Model    string            `json:"model,omitempty"`
	Match    []string          `json:"match,omitempty"`    // keys of Mapping
	Mapping  map[string]string `json:"mapping,omitempty"`  // target field -> template
	Defaults map[string]any    `json:"defaults,omitempty"` // constants
	Scope    *ScopeMapping     `json:"scope,omitempty"`    // payload paths (not templates)
	// IdempotencyField (required by record.create) is the model column that
	// stores "<receipt>:<action>"; the create runs as an upsert on it, so a
	// retry after an ambiguous failure finds the row instead of duplicating it.
	IdempotencyField string `json:"idempotency_field,omitempty"`
	// record.delete: Mode is soft (default) | hard. soft needs a model with a
	// deleted_at column; hard is refused unless the host opted the model in.
	// MaxRows caps the rows one event may delete (default 1, at most 100);
	// more matches than that fail the step without deleting anything.
	Mode    string `json:"mode,omitempty"`
	MaxRows int    `json:"max_rows,omitempty"`

	// Guard (record.*) protects against out-of-order or retried events: the
	// action is skipped as stale_event when the event is older than the row.
	Guard *Guard `json:"guard,omitempty"`

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

// Guard ops.
const (
	GuardGTE = "gte"
	GuardGT  = "gt"
)

// CodeStaleEvent is the ErrorCode of a step skipped by its Guard.
const CodeStaleEvent = "stale_event"

// Guard is the declarative order guard of a record action. Path is a
// payload.* reference holding an RFC 3339 instant or an integer (a version);
// the event is applied only when "event Op row[Field]" holds. A missing or
// unparseable payload value applies the event (the guard is opt-in by data).
//
//	{"field":"source_updated_at","path":"payload.updated_at","op":"gte"}
type Guard struct {
	Field string `json:"field"`
	Path  string `json:"path"`
	Op    string `json:"op"`
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
	Index    int    `json:"index"`
	Do       string `json:"do"`
	Status   string `json:"status"`
	RecordID string `json:"record_id,omitempty"`
	Created  bool   `json:"created,omitempty"`
	// Affected / NotFound are set by record.delete: rows retired, or nothing
	// matched (already deleted: a successful no-op).
	Affected  int    `json:"affected,omitempty"`
	NotFound  bool   `json:"not_found,omitempty"`
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
	Hop            int // hop of this event (incoming hop + 1)
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
	Hop            int // hop of this event (incoming hop + 1)
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
	Hop     int
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

// Reserved body keys. event.emit adds them to the outgoing payload, so they
// are signed together with the event; an inbound endpoint reads and removes
// them BEFORE schema validation. They are never read from a header (unsigned).
const (
	OriginField = "_origin" // "inbound:<contract>"
	HopField    = "_hop"    // number of inbound->emit->inbound hops so far
)

// takeOrigin extracts and removes the reserved fields from a decoded body.
func takeOrigin(payload any) (origin string, hop int, err error) {
	m, ok := payload.(map[string]any)
	if !ok {
		return "", 0, nil
	}
	if v, has := m[OriginField]; has {
		s, isStr := v.(string)
		if !isStr || len(s) > 200 {
			return "", 0, errors.New("bad origin")
		}
		origin = s
		delete(m, OriginField)
	}
	if v, has := m[HopField]; has {
		n, isNum := v.(json.Number)
		i, perr := n.Int64()
		if !isNum || perr != nil || i < 0 || i > 1000 {
			return "", 0, errors.New("bad hop")
		}
		hop = int(i)
		delete(m, HopField)
	}
	return origin, hop, nil
}

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
	// guardPath is Guard.Path without the "payload." prefix.
	guardPath string
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
	resOK   = map[string]bool{"id": true, "created": true, "affected": true}
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
	case ActionRecordCreate, ActionRecordUpdate, ActionRecordUpsert, ActionRecordDelete:
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
		{"idempotency_field", a.IdempotencyField != "", a.Do == ActionRecordCreate}, {"model", a.Model != "", isRecord}, {"match", len(a.Match) > 0, isRecord},
		{"defaults", len(a.Defaults) > 0, isRecord && a.Do != ActionRecordDelete},
		{"mode", a.Mode != "", a.Do == ActionRecordDelete}, {"max_rows", a.MaxRows != 0, a.Do == ActionRecordDelete}, {"scope", a.Scope != nil, isRecord},
		{"mapping", len(a.Mapping) > 0, isRecord || isCmd}, {"guard", a.Guard != nil, isRecord},
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
		if a.Do == ActionRecordCreate {
			f := a.IdempotencyField
			if !fieldRe.MatchString(f) || forbidden[f] {
				return nil, fmt.Errorf("record.create needs idempotency_field (a model column that stores the retry key)")
			}
			if _, dup := a.Mapping[f]; dup {
				return nil, fmt.Errorf("idempotency_field %q must not be mapped", f)
			}
			if _, dup := a.Defaults[f]; dup {
				return nil, fmt.Errorf("idempotency_field %q must not be a default", f)
			}
		}
		if a.Do == ActionRecordDelete {
			if err := checkDelete(a); err != nil {
				return nil, err
			}
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
		if a.Guard != nil {
			gp, err := checkGuard(a, v)
			if err != nil {
				return nil, err
			}
			ca.guardPath = gp
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
		if _, has := a.Payload[OriginField]; has {
			return nil, fmt.Errorf("payload key %q is reserved", OriginField)
		}
		if _, has := a.Payload[HopField]; has {
			return nil, fmt.Errorf("payload key %q is reserved", HopField)
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

// checkDelete validates record.delete: rows are selected only by match, so the
// mapping carries exactly the match fields (nothing is written), the mode is
// known and max_rows stays within the ceiling.
func checkDelete(a Action) error {
	if len(a.Match) == 0 {
		return fmt.Errorf("record.delete needs match fields (it never deletes without a filter)")
	}
	in := map[string]bool{}
	for _, m := range a.Match {
		in[m] = true
	}
	for to := range a.Mapping {
		if !in[to] {
			return fmt.Errorf("record.delete mapping may only hold match fields (%q is not one)", to)
		}
	}
	switch a.Mode {
	case "", DeleteSoft, DeleteHard:
	default:
		return fmt.Errorf("mode must be soft|hard")
	}
	if a.MaxRows < 0 || a.MaxRows > maxDeleteRowsCeiling {
		return fmt.Errorf("max_rows must be between 1 and %d", maxDeleteRowsCeiling)
	}
	return nil
}

// checkGuard validates the shape of a guard and returns its payload path
// (without the "payload." prefix). Whether the field is a writable, mapped
// column of the model is checked by the executor (ContractChecker).
func checkGuard(a Action, v *validator) (string, error) {
	g := a.Guard
	if !fieldRe.MatchString(g.Field) || forbidden[g.Field] {
		return "", fmt.Errorf("guard.field %q not allowed", g.Field)
	}
	for _, m := range a.Match {
		if m == g.Field {
			return "", fmt.Errorf("guard.field %q must not be a match field", g.Field)
		}
	}
	if g.Op != GuardGTE && g.Op != GuardGT {
		return "", fmt.Errorf("guard.op must be %s|%s", GuardGTE, GuardGT)
	}
	ns, path, err := v.ref(strings.TrimSpace(g.Path))
	if err != nil {
		return "", fmt.Errorf("guard.path: %v", err)
	}
	if ns != "payload" {
		return "", fmt.Errorf("guard.path must be payload.<path>")
	}
	return path, nil
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
		return "", "", fmt.Errorf("reference %q must be payload.<path>, event.<key> or result.<id|created|affected>", expr)
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
			return "", "", fmt.Errorf("result.%s unknown (id|created|affected)", path)
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
