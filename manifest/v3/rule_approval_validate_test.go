package v3

import (
	"strings"
	"testing"
)

func ruleModel(then map[string]interface{}, extra map[string]interface{}) map[string]interface{} {
	m := baseValid()
	m["models"] = []interface{}{
		map[string]interface{}{
			"key": "Sale", "table": "sales",
			"columns": []interface{}{
				map[string]interface{}{"name": "id", "type": "uuid"},
				map[string]interface{}{"name": "organization_id", "type": "uuid", "not_null": true},
				map[string]interface{}{"name": "discount", "type": "numeric"},
			},
		},
	}
	rule := map[string]interface{}{"key": "r1", "model": "Sale", "when": "discount > 10", "then": then}
	for k, v := range extra {
		rule[k] = v
	}
	m["contributions"] = map[string]interface{}{"rules": []interface{}{rule}}
	return m
}

func TestRuleApproval_Validation(t *testing.T) {
	ok := map[string]interface{}{"kind": "request_approval", "message": "m", "approval": map[string]interface{}{"roles": []interface{}{"manager"}}}
	if err := Validate(mustJSON(t, ruleModel(ok, nil))); err != nil {
		t.Fatalf("valid rule: %v", err)
	}
	cases := map[string]struct {
		then  map[string]interface{}
		extra map[string]interface{}
		want  string
	}{
		"approval missing": {map[string]interface{}{"kind": "request_approval", "message": "m"}, nil, "then.approval is required"},
		"approval on flag": {map[string]interface{}{"kind": "flag", "message": "m", "approval": map[string]interface{}{"roles": []interface{}{"x"}}}, nil, "only valid when then.kind is request_approval"},
		"empty roles":      {map[string]interface{}{"kind": "request_approval", "message": "m", "approval": map[string]interface{}{"roles": []interface{}{}}}, nil, "minItems"},
		"event_types":      {ok, map[string]interface{}{"event_types": []interface{}{"created"}}, "event_types is not allowed"},
	}
	for name, c := range cases {
		err := Validate(mustJSON(t, ruleModel(c.then, c.extra)))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want %q, got %v", name, c.want, err)
		}
	}
}
