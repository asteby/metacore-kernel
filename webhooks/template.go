package webhooks

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Event es la ocurrencia que alimenta una suscripcion declarativa. Es la
// UNICA vista que ven el filtro y la transformacion: rutas `event.*` (campos
// fijos del sobre) y `payload.*` (el cuerpo del evento). No se ejecuta codigo.
type Event struct {
	// ID identifica la ocurrencia; es la base de la llave de idempotencia por
	// omision y de la deduplicacion cuando el bus republica (at-least-once).
	// Vacio = el servicio genera uno aleatorio (sin deduplicacion).
	ID            string
	Name          string
	OrgID         uuid.UUID
	OccurredAt    time.Time
	AggregateType string
	AggregateID   string
	Sequence      int64
	Source        string
	BranchID      string
	CorrelationID string
	ActorID       string
	Payload       map[string]any
}

// EventFields es la lista cerrada de campos `event.*` disponibles.
var EventFields = []string{
	"id", "name", "occurred_at", "org_id", "aggregate_type", "aggregate_id",
	"sequence", "source", "branch_id", "correlation_id", "actor_id",
}

func isEventField(f string) bool {
	for _, x := range EventFields {
		if x == f {
			return true
		}
	}
	return false
}

// view arma la raiz sobre la que se resuelven las rutas. El payload debe estar
// normalizado (JSON: map/[]any/float64/string/bool/nil).
func (e *Event) view() map[string]any {
	occ := ""
	if !e.OccurredAt.IsZero() {
		occ = e.OccurredAt.UTC().Format(time.RFC3339)
	}
	org := ""
	if e.OrgID != uuid.Nil {
		org = e.OrgID.String()
	}
	ev := map[string]any{
		"id": e.ID, "name": e.Name, "occurred_at": occ, "org_id": org,
		"aggregate_type": e.AggregateType, "aggregate_id": e.AggregateID,
		"source": e.Source, "branch_id": e.BranchID,
		"correlation_id": e.CorrelationID, "actor_id": e.ActorID,
	}
	if e.Sequence != 0 {
		ev["sequence"] = float64(e.Sequence)
	} else {
		ev["sequence"] = nil
	}
	p := e.Payload
	if p == nil {
		p = map[string]any{}
	}
	return map[string]any{"event": ev, "payload": p}
}

// normalizeJSON pasa v por JSON para que numeros, UUID, tiempos y structs
// tengan la misma forma que si hubieran viajado por el bus.
func normalizeJSON(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// --- rutas ----------------------------------------------------------------

var segRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// pathRef es una ruta ya analizada.
type pathRef struct {
	raw  string
	root string   // "event" | "payload"
	segs []string // sin la raiz
}

func parsePath(p string) (pathRef, error) {
	p = strings.TrimSpace(p)
	parts := strings.Split(p, ".")
	switch parts[0] {
	case "event":
		if len(parts) != 2 || !isEventField(parts[1]) {
			return pathRef{}, fmt.Errorf("%w: %q (event fields: %s)", ErrUnknownPath, p, strings.Join(EventFields, ", "))
		}
	case "payload":
		if len(parts) > 12 {
			return pathRef{}, fmt.Errorf("%w: %q is too deep", ErrInvalidDeclarative, p)
		}
		for _, s := range parts[1:] {
			if !segRe.MatchString(s) {
				return pathRef{}, fmt.Errorf("%w: bad path %q", ErrInvalidDeclarative, p)
			}
		}
	default:
		return pathRef{}, fmt.Errorf("%w: path %q must start with event. or payload.", ErrUnknownPath, p)
	}
	return pathRef{raw: p, root: parts[0], segs: parts[1:]}, nil
}

func (p pathRef) lookup(root map[string]any) (any, bool) {
	var cur any = root[p.root]
	for _, s := range p.segs {
		switch c := cur.(type) {
		case map[string]any:
			v, ok := c[s]
			if !ok {
				return nil, false
			}
			cur = v
		case []any:
			i, err := strconv.Atoi(s)
			if err != nil || i < 0 || i >= len(c) {
				return nil, false
			}
			cur = c[i]
		default:
			return nil, false
		}
	}
	return cur, true
}

// --- plantillas -----------------------------------------------------------

const (
	maxTemplateLen = 1000
	maxBodyNodes   = 500
	maxBodyDepth   = 8
)

type tmplFn struct{ name, arg string }

type tmplExpr struct {
	path pathRef
	fns  []tmplFn
}

type tmplPart struct {
	lit  string
	expr *tmplExpr
}

// closedFns es la lista cerrada de funciones puras. `map` usa las tablas.
var closedFns = map[string]bool{"lower": true, "upper": true, "trim": true, "default": true, "map": true}

var tableNameRe = regexp.MustCompile(`^[a-z0-9_]{1,40}$`)

func parseTemplate(s string) ([]tmplPart, error) {
	if len(s) > maxTemplateLen {
		return nil, fmt.Errorf("%w: template too long", ErrInvalidDeclarative)
	}
	var parts []tmplPart
	for len(s) > 0 {
		i := strings.Index(s, "{{")
		if i < 0 {
			parts = append(parts, tmplPart{lit: s})
			break
		}
		if i > 0 {
			parts = append(parts, tmplPart{lit: s[:i]})
		}
		rest := s[i+2:]
		j := strings.Index(rest, "}}")
		if j < 0 {
			return nil, fmt.Errorf("%w: unclosed {{ in %q", ErrInvalidDeclarative, s)
		}
		ex, err := parseExpr(rest[:j])
		if err != nil {
			return nil, err
		}
		parts = append(parts, tmplPart{expr: ex})
		s = rest[j+2:]
	}
	return parts, nil
}

func parseExpr(src string) (*tmplExpr, error) {
	segs := strings.Split(src, "|")
	p, err := parsePath(segs[0])
	if err != nil {
		return nil, err
	}
	ex := &tmplExpr{path: p}
	for _, f := range segs[1:] {
		f = strings.TrimSpace(f)
		name, arg, _ := strings.Cut(f, ":")
		name = strings.TrimSpace(name)
		if !closedFns[name] {
			return nil, fmt.Errorf("%w: unknown function %q (allowed: lower, upper, trim, default, map)", ErrInvalidDeclarative, name)
		}
		arg = strings.TrimSpace(arg)
		if len(arg) >= 2 && arg[0] == '"' && arg[len(arg)-1] == '"' {
			arg = arg[1 : len(arg)-1]
		}
		switch name {
		case "map":
			if !tableNameRe.MatchString(arg) {
				return nil, fmt.Errorf("%w: map needs a table name, got %q", ErrInvalidDeclarative, arg)
			}
		case "default":
			// el argumento puede ser vacio: default de cadena vacia
		default:
			if arg != "" {
				return nil, fmt.Errorf("%w: %s takes no argument", ErrInvalidDeclarative, name)
			}
		}
		ex.fns = append(ex.fns, tmplFn{name: name, arg: arg})
	}
	return ex, nil
}

func (ex *tmplExpr) eval(root map[string]any, tables map[string]map[string]string) (any, error) {
	v, ok := ex.path.lookup(root)
	if !ok {
		v = nil
	}
	for _, f := range ex.fns {
		switch f.name {
		case "default":
			if v == nil || v == "" {
				v = f.arg
			}
		case "lower":
			if v != nil {
				v = strings.ToLower(scalarText(v))
			}
		case "upper":
			if v != nil {
				v = strings.ToUpper(scalarText(v))
			}
		case "trim":
			if v != nil {
				v = strings.TrimSpace(scalarText(v))
			}
		case "map":
			tbl := tables[f.arg]
			key := ""
			if v != nil {
				key = scalarText(v)
			}
			if out, ok := tbl[key]; ok {
				v = out
			} else if out, ok := tbl["*"]; ok {
				v = out
			} else {
				return nil, fmt.Errorf("table %q has no entry for %q", f.arg, key)
			}
		}
	}
	return v, nil
}

// scalarText da el texto de un valor JSON (numeros sin ".0" espurio).
func scalarText(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

// --- cuerpo ---------------------------------------------------------------

// Transform es la transformacion declarativa del cuerpo (RFC 0003, D4):
// sustitucion de rutas, constantes y tablas de traduccion. Sin lenguaje.
//
//	{"body": {"external_reference": "{{event.aggregate_id}}",
//	          "name": "{{payload.name}}",
//	          "channel": "{{payload.source | map:channels}}",
//	          "currency": "MXN"},
//	 "tables": {"channels": {"web": "b2c", "*": "other"}}}
type Transform struct {
	Body   map[string]any               `json:"body"`
	Tables map[string]map[string]string `json:"tables,omitempty"`
}

type bodyNode struct {
	constant any // si parts == nil && obj == nil && arr == nil
	parts    []tmplPart
	obj      map[string]*bodyNode
	arr      []*bodyNode
}

type compiledTransform struct {
	root   *bodyNode
	tables map[string]map[string]string
	refs   []pathRef
}

func compileTransform(t *Transform) (*compiledTransform, error) {
	if t == nil {
		return nil, nil
	}
	if len(t.Body) == 0 {
		return nil, fmt.Errorf("%w: transform.body is empty", ErrInvalidDeclarative)
	}
	if len(t.Tables) > 50 {
		return nil, fmt.Errorf("%w: too many tables", ErrInvalidDeclarative)
	}
	for name, tbl := range t.Tables {
		if !tableNameRe.MatchString(name) || len(tbl) > 1000 {
			return nil, fmt.Errorf("%w: bad table %q", ErrInvalidDeclarative, name)
		}
	}
	c := &compiledTransform{tables: t.Tables}
	count := 0
	root, err := c.compileNode(t.Body, 0, &count)
	if err != nil {
		return nil, err
	}
	c.root = root
	return c, nil
}

func (c *compiledTransform) compileNode(v any, depth int, count *int) (*bodyNode, error) {
	*count++
	if *count > maxBodyNodes || depth > maxBodyDepth {
		return nil, fmt.Errorf("%w: transform body too large or too deep", ErrInvalidDeclarative)
	}
	switch x := v.(type) {
	case string:
		parts, err := parseTemplate(x)
		if err != nil {
			return nil, err
		}
		hasExpr := false
		for _, p := range parts {
			if p.expr != nil {
				hasExpr = true
				c.refs = append(c.refs, p.expr.path)
				for _, f := range p.expr.fns {
					if f.name == "map" {
						if _, ok := c.tables[f.arg]; !ok {
							return nil, fmt.Errorf("%w: unknown table %q", ErrInvalidDeclarative, f.arg)
						}
					}
				}
			}
		}
		if !hasExpr {
			return &bodyNode{constant: x}, nil
		}
		return &bodyNode{parts: parts}, nil
	case map[string]any:
		n := &bodyNode{obj: map[string]*bodyNode{}}
		for k, e := range x {
			ch, err := c.compileNode(e, depth+1, count)
			if err != nil {
				return nil, err
			}
			n.obj[k] = ch
		}
		return n, nil
	case []any:
		n := &bodyNode{arr: []*bodyNode{}}
		for _, e := range x {
			ch, err := c.compileNode(e, depth+1, count)
			if err != nil {
				return nil, err
			}
			n.arr = append(n.arr, ch)
		}
		return n, nil
	case nil, bool, float64, json.Number, int, int64:
		return &bodyNode{constant: x}, nil
	default:
		return nil, fmt.Errorf("%w: unsupported constant %T in transform", ErrInvalidDeclarative, v)
	}
}

func (c *compiledTransform) render(root map[string]any) (map[string]any, error) {
	out, err := c.root.render(root, c.tables)
	if err != nil {
		return nil, err
	}
	m, _ := out.(map[string]any)
	return m, nil
}

func (n *bodyNode) render(root map[string]any, tables map[string]map[string]string) (any, error) {
	switch {
	case n.obj != nil:
		m := make(map[string]any, len(n.obj))
		for k, ch := range n.obj {
			v, err := ch.render(root, tables)
			if err != nil {
				return nil, err
			}
			m[k] = v
		}
		return m, nil
	case n.arr != nil:
		a := make([]any, 0, len(n.arr))
		for _, ch := range n.arr {
			v, err := ch.render(root, tables)
			if err != nil {
				return nil, err
			}
			a = append(a, v)
		}
		return a, nil
	case n.parts != nil:
		return renderParts(n.parts, root, tables)
	default:
		return n.constant, nil
	}
}

// renderParts: una expresion unica conserva el tipo (numero, objeto...); con
// texto alrededor, o varias expresiones, el resultado es una cadena.
func renderParts(parts []tmplPart, root map[string]any, tables map[string]map[string]string) (any, error) {
	if len(parts) == 1 && parts[0].expr != nil {
		return parts[0].expr.eval(root, tables)
	}
	var sb strings.Builder
	for _, p := range parts {
		if p.expr == nil {
			sb.WriteString(p.lit)
			continue
		}
		v, err := p.expr.eval(root, tables)
		if err != nil {
			return nil, err
		}
		sb.WriteString(scalarText(v))
	}
	return sb.String(), nil
}

// renderString evalua una plantilla de texto (llave de idempotencia).
func renderString(parts []tmplPart, root map[string]any) (string, error) {
	v, err := renderParts(parts, root, nil)
	if err != nil {
		return "", err
	}
	return scalarText(v), nil
}
