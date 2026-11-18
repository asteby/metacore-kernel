package inbound

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Op is the operation a contract performs against its target.
type Op string

const (
	OpCreate Op = "create"
	OpUpdate Op = "update"
	OpUpsert Op = "upsert"
	// OpDelete only exists inside a chain (record.delete); a route or a
	// command.run cannot declare it.
	OpDelete Op = "delete"
)

// Route says where a validated event lands. Exactly one of Model or Command.
type Route struct {
	// Model is the dynamic model key (modelbase registry name) the event
	// creates/updates rows of.
	Model string `json:"model,omitempty"`
	// Command is the name of a command already registered in the host's
	// CommandRegistry. The contract only routes to it; it never carries code.
	Command string `json:"command,omitempty"`
	// Op is create | update | upsert. Commands receive it but decide for
	// themselves.
	Op Op `json:"op"`
	// Mapping is target field -> source path in the payload ("a.b", "items.0.sku").
	Mapping map[string]string `json:"mapping"`
	// Defaults are constant target values (never read from the payload).
	Defaults map[string]any `json:"defaults,omitempty"`
	// Match lists the target fields (keys of Mapping) that identify an
	// existing row for update/upsert (e.g. ["sku","warehouse"]).
	Match []string `json:"match,omitempty"`
	// Scope maps the optional branch/entity of the event to a payload path.
	Scope *ScopeMapping `json:"scope,omitempty"`
}

// ScopeMapping extracts the branch/entity scope from the payload so the
// executor can enforce them (RFC 0001).
type ScopeMapping struct {
	Branch string `json:"branch,omitempty"`
	Entity string `json:"entity,omitempty"`
}

// LogPolicy controls what the receipt log keeps about the event.
type LogPolicy struct {
	// KeyInClear stores the event key as-is in the receipt. Default false:
	// only its hash is kept, because keys often embed personal data (email,
	// phone). Enable it only when the template yields opaque ids.
	KeyInClear bool `json:"key_in_clear,omitempty"`
}

// Contract is the declarative description of one inbound event type.
type Contract struct {
	Key     string `json:"key"`
	Version int    `json:"version"`
	// JSONSchema validates the payload (draft 2020-12). Remote $ref is denied.
	JSONSchema json.RawMessage `json:"json_schema"`
	Route      Route           `json:"route"`
	// Idempotency is a template over the payload: "{event_id}",
	// "{sku}:{updated_at}", "{header:X-Event-Id}". The result is the event key.
	Idempotency string `json:"idempotency"`
	// Auth is the name of the Verifier profile (hmac_canonical_v1, bearer, ...).
	Auth string    `json:"auth"`
	Log  LogPolicy `json:"log,omitempty"`
	// Actions is the RFC 0003 §2.4 chain; when set, Route must be empty.
	Actions []Action `json:"actions,omitempty"`
	// OnError is the default policy of the chain: stop (default) | continue.
	OnError string `json:"on_error,omitempty"`
	// IgnoreOrigins lists origin marks (exact or "prefix*") whose events are
	// ignored with 200 "ignored"; the contract own origin is always ignored.
	IgnoreOrigins []string `json:"ignore_origins,omitempty"`
}

var (
	keyRe     = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
	tmplRe    = regexp.MustCompile(`\{([^{}]+)\}`)
	fieldRe   = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,63}$`)
	forbidden = map[string]bool{
		"id": true, "organization_id": true, "tenant_id": true, "created_by_id": true,
		"created_at": true, "updated_at": true, "deleted_at": true,
	}
)

// compiled is a validated contract plus its compiled schema.
type compiled struct {
	Contract
	schema *jsonschema.Schema
	tmpl   []tmplPart
	chain  []*cAction
}

type tmplPart struct {
	lit    string
	path   string
	header string
}

// ErrInvalidContract wraps every registration failure.
var ErrInvalidContract = errors.New("inbound: invalid contract")

func invalidf(f string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidContract, fmt.Sprintf(f, a...))
}

// noLoader denies every external reference: a contract schema is data and
// must never make the server fetch a URL or read a file.
type noLoader struct{}

func (noLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("remote or file $ref not allowed (%q)", url)
}

func compile(c Contract) (*compiled, error) {
	if !keyRe.MatchString(c.Key) {
		return nil, invalidf("key %q must match %s", c.Key, keyRe)
	}
	if c.Version < 1 {
		return nil, invalidf("%s: version must be >= 1", c.Key)
	}
	if strings.TrimSpace(c.Auth) == "" {
		return nil, invalidf("%s: auth profile required", c.Key)
	}
	if strings.TrimSpace(c.Idempotency) == "" {
		return nil, invalidf("%s: idempotency template required", c.Key)
	}
	r := c.Route
	hasChain := len(c.Actions) > 0
	if hasChain {
		if r.Model != "" || r.Command != "" || r.Op != "" || len(r.Mapping) > 0 || len(r.Defaults) > 0 || len(r.Match) > 0 || r.Scope != nil {
			return nil, invalidf("%s: route and actions are mutually exclusive", c.Key)
		}
	} else if c.OnError != "" {
		return nil, invalidf("%s: on_error only applies to actions", c.Key)
	}
	if !hasChain {
		if err := compileRoute(c.Key, r); err != nil {
			return nil, err
		}
	}
	tmpl, err := parseTemplate(c.Idempotency)
	if err != nil {
		return nil, invalidf("%s: idempotency: %v", c.Key, err)
	}
	if len(c.JSONSchema) == 0 {
		return nil, invalidf("%s: json_schema required", c.Key)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(c.JSONSchema))
	if err != nil {
		return nil, invalidf("%s: json_schema: %v", c.Key, err)
	}
	comp := jsonschema.NewCompiler()
	comp.UseLoader(noLoader{})
	if err := comp.AddResource("contract.json", doc); err != nil {
		return nil, invalidf("%s: json_schema: %v", c.Key, err)
	}
	sch, err := comp.Compile("contract.json")
	if err != nil {
		return nil, invalidf("%s: json_schema: %v", c.Key, err)
	}
	var chain []*cAction
	if hasChain {
		if chain, err = compileActions(c, c.JSONSchema); err != nil {
			return nil, err
		}
	}
	return &compiled{Contract: c, schema: sch, tmpl: tmpl, chain: chain}, nil
}

func parseTemplate(t string) ([]tmplPart, error) {
	var parts []tmplPart
	last := 0
	for _, m := range tmplRe.FindAllStringSubmatchIndex(t, -1) {
		if m[0] > last {
			parts = append(parts, tmplPart{lit: t[last:m[0]]})
		}
		expr := t[m[2]:m[3]]
		if h, ok := strings.CutPrefix(expr, "header:"); ok {
			if h == "" {
				return nil, errors.New("empty header name")
			}
			parts = append(parts, tmplPart{header: h})
		} else {
			if _, err := parsePath(expr); err != nil {
				return nil, err
			}
			parts = append(parts, tmplPart{path: expr})
		}
		last = m[1]
	}
	if last < len(t) {
		rest := t[last:]
		if strings.ContainsAny(rest, "{}") {
			return nil, errors.New("unbalanced braces")
		}
		parts = append(parts, tmplPart{lit: rest})
	}
	hasVar := false
	for _, p := range parts {
		if p.path != "" || p.header != "" {
			hasVar = true
		}
	}
	if !hasVar {
		return nil, errors.New("template must reference at least one field")
	}
	return parts, nil
}

// Registry holds contracts by key and version. Safe for concurrent use.
// Adding a contract is pure data; no route is created per type.
type Registry struct {
	mu sync.RWMutex
	m  map[string]map[int]*compiled
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{m: map[string]map[int]*compiled{}} }

// Register validates, compiles and stores a contract. Re-registering the same
// key+version replaces it (hot reload of data).
func (r *Registry) Register(c Contract) error {
	cc, err := compile(c)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m[c.Key] == nil {
		r.m[c.Key] = map[int]*compiled{}
	}
	r.m[c.Key][c.Version] = cc
	return nil
}

// LoadJSON registers contracts from a JSON object or array (preset data).
func (r *Registry) LoadJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	var list []Contract
	if len(data) > 0 && data[0] == '[' {
		if err := json.Unmarshal(data, &list); err != nil {
			return invalidf("json: %v", err)
		}
	} else {
		var c Contract
		if err := json.Unmarshal(data, &c); err != nil {
			return invalidf("json: %v", err)
		}
		list = []Contract{c}
	}
	for _, c := range list {
		if err := r.Register(c); err != nil {
			return err
		}
	}
	return nil
}

// get returns the requested version, or the highest when version == 0.
func (r *Registry) get(key string, version int) (*compiled, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	vs := r.m[key]
	if len(vs) == 0 {
		return nil, false
	}
	if version > 0 {
		c, ok := vs[version]
		return c, ok
	}
	best := 0
	for v := range vs {
		if v > best {
			best = v
		}
	}
	return vs[best], true
}

// Keys lists registered contract keys (sorted).
func (r *Registry) Keys() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.m))
	for k := range r.m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (r *Registry) all() []*compiled {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []*compiled
	for _, vs := range r.m {
		for _, c := range vs {
			out = append(out, c)
		}
	}
	return out
}

func (r *Registry) put(c *compiled) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m[c.Key] == nil {
		r.m[c.Key] = map[int]*compiled{}
	}
	r.m[c.Key][c.Version] = c
}

func compileRoute(key string, r Route) error {
	if (r.Model == "") == (r.Command == "") {
		return invalidf("%s: route needs exactly one of model|command", key)
	}
	switch r.Op {
	case OpCreate, OpUpdate, OpUpsert:
	default:
		return invalidf("%s: op must be create|update|upsert", key)
	}
	if len(r.Mapping) == 0 {
		return invalidf("%s: mapping is empty", key)
	}
	for to, from := range r.Mapping {
		if !fieldRe.MatchString(to) || forbidden[to] {
			return invalidf("%s: target field %q not allowed", key, to)
		}
		if _, err := parsePath(from); err != nil {
			return invalidf("%s: mapping %q: %v", key, to, err)
		}
	}
	for to := range r.Defaults {
		if !fieldRe.MatchString(to) || forbidden[to] {
			return invalidf("%s: default field %q not allowed", key, to)
		}
	}
	if r.Model != "" && (r.Op == OpUpdate || r.Op == OpUpsert) && len(r.Match) == 0 {
		return invalidf("%s: %s needs match fields", key, r.Op)
	}
	for _, m := range r.Match {
		if _, ok := r.Mapping[m]; !ok {
			return invalidf("%s: match field %q is not in mapping", key, m)
		}
	}
	if r.Scope != nil {
		for _, p := range []string{r.Scope.Branch, r.Scope.Entity} {
			if p == "" {
				continue
			}
			if _, err := parsePath(p); err != nil {
				return invalidf("%s: scope path: %v", key, err)
			}
		}
	}
	return nil
}
