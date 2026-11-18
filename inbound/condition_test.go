package inbound

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const condSchema = `{"type":"object","required":["id","action"],
 "properties":{"id":{"type":"string"},"action":{"type":"string"},"count":{"type":"integer"},"note":{"type":"string"}}}`

func condValidator(t *testing.T) *validator {
	t.Helper()
	var schema any
	if err := json.Unmarshal([]byte(condSchema), &schema); err != nil {
		t.Fatal(err)
	}
	return &validator{schema: schema}
}

func condHolds(t *testing.T, c Condition, body string) bool {
	t.Helper()
	cc, err := compileCond(c, condValidator(t))
	if err != nil {
		t.Fatalf("compileCond: %v", err)
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(body)))
	dec.UseNumber()
	var payload any
	if err := dec.Decode(&payload); err != nil {
		t.Fatal(err)
	}
	return cc.holds(&evalEnv{payload: payload})
}

func raw(s string) json.RawMessage { return json.RawMessage(s) }

func TestConditionInNeSemantics(t *testing.T) {
	in := func(vs ...any) Condition { return Condition{Path: "payload.action", In: vs} }
	cases := []struct {
		name string
		c    Condition
		body string
		want bool
	}{
		{"in match", in("created", "updated"), `{"action":"updated"}`, true},
		{"in no match", in("created", "updated"), `{"action":"deleted"}`, false},
		{"in absent is false", in("created"), `{"id":"1"}`, false},
		{"ne differs", Condition{Path: "payload.action", Ne: raw(`"deleted"`)}, `{"action":"created"}`, true},
		{"ne equal", Condition{Path: "payload.action", Ne: raw(`"deleted"`)}, `{"action":"deleted"}`, false},
		// documented: absent -> ne is TRUE (same as webhooks.Condition)
		{"ne absent is true", Condition{Path: "payload.note", Ne: raw(`"x"`)}, `{"id":"1"}`, true},
		{"eq absent is false", Condition{Path: "payload.note", Eq: raw(`"x"`)}, `{"id":"1"}`, false},
		// number vs string never equal; numbers compare by value
		{"eq number vs string", Condition{Path: "payload.count", Eq: raw(`"1"`)}, `{"count":1}`, false},
		{"ne number vs string", Condition{Path: "payload.count", Ne: raw(`"1"`)}, `{"count":1}`, true},
		{"in mixed types number", Condition{Path: "payload.count", In: []any{"1", float64(2), float64(3)}}, `{"count":2}`, true},
		{"in mixed types string does not match number", Condition{Path: "payload.count", In: []any{"1", "2"}}, `{"count":2}`, false},
		{"in 1.0 matches 1", Condition{Path: "payload.count", In: []any{1.0}}, `{"count":1}`, true},
		{"ne number equal", Condition{Path: "payload.count", Ne: raw(`1`)}, `{"count":1}`, false},
		// regression: eq / exists unchanged
		{"eq match", Condition{Path: "payload.action", Eq: raw(`"x"`)}, `{"action":"x"}`, true},
		{"eq no match", Condition{Path: "payload.action", Eq: raw(`"x"`)}, `{"action":"y"}`, false},
		{"exists true", Condition{Path: "payload.note", Exists: ptr(true)}, `{"note":"a"}`, true},
		{"exists true absent", Condition{Path: "payload.note", Exists: ptr(true)}, `{}`, false},
		{"exists false absent", Condition{Path: "payload.note", Exists: ptr(false)}, `{}`, true},
	}
	for _, tc := range cases {
		if got := condHolds(t, tc.c, tc.body); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestConditionCompileErrors(t *testing.T) {
	big := make([]any, MaxConditionIn+1)
	for i := range big {
		big[i] = "v"
	}
	ok50 := big[:MaxConditionIn]
	bad := map[string]Condition{
		"eq+ne":        {Path: "payload.action", Eq: raw(`"a"`), Ne: raw(`"b"`)},
		"eq+in":        {Path: "payload.action", Eq: raw(`"a"`), In: []any{"a"}},
		"in+exists":    {Path: "payload.action", In: []any{"a"}, Exists: ptr(true)},
		"ne+exists":    {Path: "payload.action", Ne: raw(`"a"`), Exists: ptr(true)},
		"none":         {Path: "payload.action"},
		"empty in":     {Path: "payload.action", In: []any{}},
		"too many in":  {Path: "payload.action", In: big},
		"in object":    {Path: "payload.action", In: []any{map[string]any{"a": 1}}},
		"in array":     {Path: "payload.action", In: []any{[]any{"a"}}},
		"ne object":    {Path: "payload.action", Ne: raw(`{"a":1}`)},
		"ne bad json":  {Path: "payload.action", Ne: raw(`{`)},
		"path missing": {Path: "payload.nope", In: []any{"a"}},
	}
	for name, c := range bad {
		if _, err := compileCond(c, condValidator(t)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	if _, err := compileCond(Condition{Path: "payload.action", In: ok50}, condValidator(t)); err != nil {
		t.Errorf("50 values must be accepted: %v", err)
	}
}

// Registration reports invalid_condition with the action index.
func TestConditionRegistrationError(t *testing.T) {
	for name, w := range map[string]*Condition{
		"double operator": {Path: "payload.id", Ne: raw(`"a"`), In: []any{"b"}},
		"empty in":        {Path: "payload.id", In: []any{}},
		"unknown path":    {Path: "payload.zzz", In: []any{"a"}},
	} {
		c := crmContract()
		c.Actions[2].When = w
		err := NewRegistry().Register(c)
		if !errors.Is(err, ErrInvalidContract) {
			t.Fatalf("%s: want ErrInvalidContract, got %v", name, err)
		}
		if msg := err.Error(); !strings.Contains(msg, "actions[2]") || !strings.Contains(msg, CodeInvalidCondition) {
			t.Errorf("%s: message lacks index or code: %v", name, err)
		}
	}
	c := crmContract()
	c.Actions[2].When = &Condition{Path: "payload.id", Ne: raw(`"a"`)}
	if err := NewRegistry().Register(c); err != nil {
		t.Fatalf("valid ne rejected: %v", err)
	}
	// JSON round trip of a contract-style `when`
	var w Condition
	if err := json.Unmarshal([]byte(`{"path":"payload.action","in":["created","updated"]}`), &w); err != nil {
		t.Fatal(err)
	}
	if len(w.In) != 2 || w.In[0] != "created" {
		t.Fatalf("%+v", w)
	}
}

// Two complementary actions: `in` and `eq` pick exactly one branch per event.
func TestConditionComplementaryChain(t *testing.T) {
	c := crmContract()
	c.Key, c.Idempotency = "cond-chain", "{id}:{action}"
	c.JSONSchema = json.RawMessage(condSchema)
	c.Actions = []Action{
		{Do: ActionRecordUpsert, Model: "customers", Match: []string{"external_ref"},
			Mapping: map[string]string{"external_ref": "{{payload.id}}"}},
		{Do: ActionNotify, Template: "upsert",
			When: &Condition{Path: "payload.action", In: []any{"created", "updated"}}},
		{Do: ActionEventEmit, Name: "customers.removed",
			When:    &Condition{Path: "payload.action", Eq: raw(`"deleted"`)},
			Payload: map[string]any{"id": "{{result.id}}"}},
		{Do: ActionAutomationEnqueue, Name: "customers.other",
			When: &Condition{Path: "payload.action", Ne: raw(`"deleted"`)}},
	}
	ce := newChainEnv(t, c)
	want := map[string]string{
		"created": "done,done,skipped,done",
		"updated": "done,done,skipped,done",
		"deleted": "done,skipped,done,skipped",
		"other":   "done,skipped,skipped,done",
	}
	for i, act := range []string{"created", "updated", "deleted", "other"} {
		_, out := ce.send(t, ce.orgA, c.Key, `{"id":"k`+string(rune('a'+i))+`","action":"`+act+`"}`, nil)
		if got := strings.Join(steps(t, out), ","); got != want[act] {
			t.Errorf("%s: got %s want %s (%v)", act, got, want[act], out)
		}
	}
	if len(ce.nt.reqs) != 2 || len(ce.em.reqs) != 1 || len(ce.auto.evs) != 3 {
		t.Fatalf("notify %d emit %d auto %d", len(ce.nt.reqs), len(ce.em.reqs), len(ce.auto.evs))
	}
}
