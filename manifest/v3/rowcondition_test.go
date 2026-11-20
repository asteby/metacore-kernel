package v3

import (
	"encoding/json"
	"strings"
	"testing"
)

func rc(t *testing.T, raw string) RowCondition {
	t.Helper()
	var c RowCondition
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRowCondition_Eval(t *testing.T) {
	row := map[string]any{
		"id": "1", "debt": 12.5, "name": "Ana", "active": true, "note": "", "tags": "a",
		"nested": map[string]any{"level": float64(3)},
	}
	for name, tc := range map[string]struct {
		cond string
		want bool
	}{
		"eq string":          {`{"field":"name","operator":"eq","value":"Ana"}`, true},
		"eq default":         {`{"field":"name","value":"Ana"}`, true},
		"eq number as str":   {`{"field":"id","operator":"eq","value":1}`, true},
		"neq id != 1":        {`{"field":"id","operator":"neq","value":1}`, false},
		"neq other":          {`{"field":"id","operator":"neq","value":2}`, true},
		"in":                 {`{"field":"name","operator":"in","value":["Bob","Ana"]}`, true},
		"in miss":            {`{"field":"name","operator":"in","value":["Bob"]}`, false},
		"not_in":             {`{"field":"name","operator":"not_in","value":["Bob"]}`, true},
		"gt debt > 0":        {`{"field":"debt","operator":"gt","value":0}`, true},
		"gt equal":           {`{"field":"debt","operator":"gt","value":12.5}`, false},
		"gte equal":          {`{"field":"debt","operator":"gte","value":12.5}`, true},
		"lt":                 {`{"field":"debt","operator":"lt","value":20}`, true},
		"lte":                {`{"field":"debt","operator":"lte","value":12}`, false},
		"gt non numeric":     {`{"field":"name","operator":"gt","value":0}`, false},
		"gt missing":         {`{"field":"nope","operator":"gt","value":0}`, false},
		"truthy":             {`{"field":"name","operator":"truthy"}`, true},
		"truthy blank":       {`{"field":"note","operator":"truthy"}`, false},
		"falsy blank":        {`{"field":"note","operator":"falsy"}`, true},
		"falsy missing":      {`{"field":"nope","operator":"falsy"}`, true},
		"truthy bool":        {`{"field":"active","operator":"truthy"}`, true},
		"dotted":             {`{"field":"nested.level","operator":"gte","value":3}`, true},
		"alias equals":       {`{"field":"name","operator":"equals","value":"Ana"}`, true},
		"alias notin":        {`{"field":"name","operator":"notin","value":["Ana"]}`, false},
		"alias present":      {`{"field":"name","operator":"present"}`, true},
		"all":                {`{"operator":"all","conditions":[{"field":"debt","operator":"gt","value":0},{"field":"id","operator":"neq","value":1}]}`, false},
		"all ok":             {`{"operator":"all","conditions":[{"field":"debt","operator":"gt","value":0},{"field":"id","operator":"neq","value":2}]}`, true},
		"any":                {`{"operator":"any","conditions":[{"field":"debt","operator":"lt","value":0},{"field":"name","value":"Ana"}]}`, true},
		"any none":           {`{"operator":"any","conditions":[{"field":"debt","operator":"lt","value":0}]}`, false},
		"empty group passes": {`{"operator":"all"}`, true},
		"nested":             {`{"operator":"all","conditions":[{"operator":"any","conditions":[{"field":"id","value":9},{"field":"name","value":"Ana"}]}]}`, true},
		"unknown op fails":   {`{"field":"name","operator":"bogus"}`, false},
		"leaf no field":      {`{"operator":"eq","value":1}`, false},
	} {
		c := rc(t, tc.cond)
		if got := c.Eval(row); got != tc.want {
			t.Fatalf("%s: Eval = %v, want %v", name, got, tc.want)
		}
	}
	var nilC *RowCondition
	if !nilC.Eval(row) || !(&RowCondition{}).Eval(nil) {
		t.Fatal("nil/empty condition must hold")
	}
}

func TestRowCondition_Validate(t *testing.T) {
	known := map[string]struct{}{"debt": {}, "id": {}}
	ok := []string{
		`{"field":"debt","operator":"gt","value":0}`,
		`{"operator":"any","conditions":[{"field":"id","operator":"neq","value":1},{"field":"debt","operator":"falsy"}]}`,
		`{"field":"id","operator":"in","value":[1,2]}`,
	}
	for _, raw := range ok {
		c := rc(t, raw)
		if errs := c.Validate("w", known); len(errs) != 0 {
			t.Fatalf("%s: %v", raw, errs)
		}
	}
	for raw, want := range map[string]string{
		`{"field":"debt","operator":"bogus"}`:                                          "operator",
		`{"operator":"eq","value":1}`:                                                  "field is required",
		`{"field":"nope","operator":"eq","value":1}`:                                   "not a column",
		`{"field":"debt","operator":"gt","value":"x"}`:                                 "number",
		`{"field":"debt","operator":"gt"}`:                                             "number",
		`{"field":"id","operator":"in","value":1}`:                                     "list",
		`{"field":"id","operator":"truthy","value":1}`:                                 "not allowed",
		`{"operator":"all","field":"id","conditions":[]}`:                              "group takes only",
		`{"field":"id","conditions":[{"field":"id"}]}`:                                 "only allowed with operator",
		`{"operator":"any","conditions":[{"field":"nope","operator":"eq","value":1}]}`: "conditions[0]",
	} {
		c := rc(t, raw)
		errs := c.Validate("w", known)
		if len(errs) == 0 || !strings.Contains(strings.Join(errs, "|"), want) {
			t.Fatalf("%s: want %q, got %v", raw, want, errs)
		}
	}
}

func rowCondManifest(modelExtra, actionCond string) []byte {
	return rbacIdxManifest(`"models": [{"key":"Customer","table":"customers"` + modelExtra + `,"columns":[
      {"name":"id","type":"uuid"},{"name":"organization_id","type":"uuid"},{"name":"debt","type":"numeric"}]}],
      "contributions": {"actions": [{"key":"collect","label":"Collect","target_model":"Customer","handler":{"type":"compiled","function":"Collect"}` + actionCond + `}]}`)
}

func TestRowCondition_ManifestAccepted(t *testing.T) {
	m, err := Parse(rowCondManifest(
		`,"row_conditions":{"update":{"field":"id","operator":"neq","value":"00000000-0000-0000-0000-000000000001","error_key":"customers.default_locked"},"delete":{"operator":"all","conditions":[{"field":"debt","operator":"lte","value":0}]}}`,
		`,"condition":{"operator":"all","conditions":[{"field":"debt","operator":"gt","value":0}]}`))
	if err != nil {
		t.Fatal(err)
	}
	rcs := m.Models[0].RowConditions
	if rcs == nil || rcs.Update == nil || rcs.Update.ErrorKey != "customers.default_locked" || rcs.Delete == nil {
		t.Fatalf("row_conditions not decoded: %+v", rcs)
	}
	act := m.Contributions.Actions[0].Condition
	if act == nil || act.RowMatches(map[string]any{"debt": 0.0}) || !act.RowMatches(map[string]any{"debt": 5.0}) {
		t.Fatalf("action group condition not evaluated: %+v", act)
	}
}

func TestRowCondition_ManifestRejected(t *testing.T) {
	for name, tc := range map[string]struct {
		raw  []byte
		want string
	}{
		"unknown column":   {rowCondManifest(`,"row_conditions":{"update":{"field":"nope","operator":"eq","value":1}}`, ``), "not a column"},
		"bad operator":     {rowCondManifest(`,"row_conditions":{"delete":{"field":"debt","operator":"bogus"}}`, ``), "operator"},
		"empty gate":       {rowCondManifest(`,"row_conditions":{"delete":{}}`, ``), "declares no predicate"},
		"with append_only": {rowCondManifest(`,"append_only":true,"row_conditions":{"delete":{"field":"debt","operator":"lte","value":0}}`, ``), "append_only"},
		"action bad shape": {rowCondManifest(``, `,"condition":{"field":"debt","operator":"gt"}`), "number"},
	} {
		_, err := Parse(tc.raw)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: want error containing %q, got %v", name, tc.want, err)
		}
	}
}
