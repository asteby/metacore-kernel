package v3

import (
	"strings"
	"testing"
)

func authorityManifest(sa map[string]interface{}, extra map[string]interface{}) map[string]interface{} {
	m := withStageAction(nil)
	act := m["contributions"].(map[string]interface{})["actions"].([]interface{})[0].(map[string]interface{})
	delete(act, "requires_state")
	act["stage_authority"] = sa
	for k, v := range extra {
		act[k] = v
	}
	return m
}

// K3b: stage_authority names the exact transitions an action drives with the
// authority of its own permission.
func TestStageAuthority_Validation(t *testing.T) {
	ok := map[string]interface{}{"authority": "action_permission", "transitions": []interface{}{"draft->posted"}}
	if err := Validate(mustJSON(t, authorityManifest(ok, nil))); err != nil {
		t.Fatalf("valid stage_authority: %v", err)
	}
	cases := map[string]struct {
		sa    map[string]interface{}
		extra map[string]interface{}
		want  string
	}{
		"unknown authority":     {map[string]interface{}{"authority": "anything", "transitions": []interface{}{"draft->posted"}}, nil, "value must be 'action_permission'"},
		"no transitions":        {map[string]interface{}{"authority": "action_permission", "transitions": []interface{}{}}, nil, "minItems"},
		"undeclared move":       {map[string]interface{}{"authority": "action_permission", "transitions": []interface{}{"posted->draft"}}, nil, `"posted->draft" is not a transition of model "Order"`},
		"malformed pair":        {map[string]interface{}{"authority": "action_permission", "transitions": []interface{}{"draft"}}, nil, "pattern"},
		"ingest not combinable": {ok, map[string]interface{}{"ingest": true}, "cannot be combined with ingest"},
	}
	for name, c := range cases {
		err := Validate(mustJSON(t, authorityManifest(c.sa, c.extra)))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want %q, got %v", name, c.want, err)
		}
	}
}
