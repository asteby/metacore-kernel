package webhooks

import (
	"encoding/json"
	"fmt"
	"reflect"
)

// Filter es el filtro declarativo de una suscripcion (RFC 0003 §2.1). Sin
// codigo: solo condiciones sobre `event.*` / `payload.*`.
//
//	{"all": [ {"path": "payload.branch_id", "in": ["a", "b"]},
//	          {"path": "payload.email", "exists": true} ],
//	 "any": [ {"path": "payload.kind", "eq": "retail"},
//	          {"path": "payload.kind", "eq": "wholesale"} ]}
//
// all: todas deben cumplirse; any: al menos una. Si hay ambas, se exigen las
// dos. Un filtro vacio deja pasar todo.
type Filter struct {
	All []Condition `json:"all,omitempty"`
	Any []Condition `json:"any,omitempty"`
}

// Condition: exactamente UN operador entre eq, ne, in, exists.
type Condition struct {
	Path   string          `json:"path"`
	Eq     json.RawMessage `json:"eq,omitempty"`
	Ne     json.RawMessage `json:"ne,omitempty"`
	In     []any           `json:"in,omitempty"`
	Exists *bool           `json:"exists,omitempty"`
}

const maxConditions = 32

type compiledCond struct {
	path   pathRef
	op     string
	val    any
	set    []any
	exists bool
}

type compiledFilter struct {
	all, any []compiledCond
}

func compileFilter(f *Filter) (*compiledFilter, error) {
	if f == nil {
		return nil, nil
	}
	if len(f.All)+len(f.Any) > maxConditions {
		return nil, fmt.Errorf("%w: too many filter conditions (max %d)", ErrInvalidDeclarative, maxConditions)
	}
	cf := &compiledFilter{}
	var err error
	if cf.all, err = compileConds(f.All); err != nil {
		return nil, err
	}
	if cf.any, err = compileConds(f.Any); err != nil {
		return nil, err
	}
	return cf, nil
}

func compileConds(cs []Condition) ([]compiledCond, error) {
	var out []compiledCond
	for i, c := range cs {
		p, err := parsePath(c.Path)
		if err != nil {
			return nil, err
		}
		n := 0
		cc := compiledCond{path: p}
		if len(c.Eq) > 0 {
			n++
			cc.op = "eq"
			if err := json.Unmarshal(c.Eq, &cc.val); err != nil {
				return nil, fmt.Errorf("%w: filter[%d].eq: %v", ErrInvalidDeclarative, i, err)
			}
		}
		if len(c.Ne) > 0 {
			n++
			cc.op = "ne"
			if err := json.Unmarshal(c.Ne, &cc.val); err != nil {
				return nil, fmt.Errorf("%w: filter[%d].ne: %v", ErrInvalidDeclarative, i, err)
			}
		}
		if c.In != nil {
			n++
			cc.op = "in"
			if len(c.In) == 0 || len(c.In) > 200 {
				return nil, fmt.Errorf("%w: filter[%d].in needs 1..200 values", ErrInvalidDeclarative, i)
			}
			norm, err := normalizeJSON(c.In)
			if err != nil {
				return nil, fmt.Errorf("%w: filter[%d].in: %v", ErrInvalidDeclarative, i, err)
			}
			cc.set = norm.([]any)
		}
		if c.Exists != nil {
			n++
			cc.op = "exists"
			cc.exists = *c.Exists
		}
		if n != 1 {
			return nil, fmt.Errorf("%w: filter condition on %q needs exactly one of eq, ne, in, exists", ErrInvalidDeclarative, c.Path)
		}
		out = append(out, cc)
	}
	return out, nil
}

func (c compiledCond) match(root map[string]any) bool {
	v, present := c.path.lookup(root)
	switch c.op {
	case "exists":
		return (present && v != nil) == c.exists
	case "eq":
		return present && reflect.DeepEqual(v, c.val)
	case "ne":
		return !present || !reflect.DeepEqual(v, c.val)
	case "in":
		if !present {
			return false
		}
		for _, x := range c.set {
			if reflect.DeepEqual(v, x) {
				return true
			}
		}
	}
	return false
}

func (cf *compiledFilter) match(root map[string]any) bool {
	if cf == nil {
		return true
	}
	for _, c := range cf.all {
		if !c.match(root) {
			return false
		}
	}
	if len(cf.any) > 0 {
		for _, c := range cf.any {
			if c.match(root) {
				return true
			}
		}
		return false
	}
	return true
}

func (cf *compiledFilter) refs() []pathRef {
	if cf == nil {
		return nil
	}
	var r []pathRef
	for _, c := range cf.all {
		r = append(r, c.path)
	}
	for _, c := range cf.any {
		r = append(r, c.path)
	}
	return r
}
