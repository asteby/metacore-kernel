package modelbase

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestAccessPolicyRuleFallback(t *testing.T) {
	p := AccessPublicReadStaffWrite("store.admin")
	for _, a := range []AccessAction{AccessList, AccessGet} {
		if r := p.Rule(a); r == nil || !r.Public {
			t.Fatalf("%s must be public, got %+v", a, r)
		}
	}
	for _, a := range []AccessAction{AccessCreate, AccessUpdate, AccessDelete} {
		r := p.Rule(a)
		if r == nil || r.Public || !r.MatchesRoles([]string{"Store.Admin"}) || r.MatchesRoles([]string{"owner"}) {
			t.Fatalf("%s must be staff-only, got %+v", a, r)
		}
	}
	if (AccessPolicy{}).Rule(AccessCreate) != nil || !(AccessPolicy{}).IsZero() {
		t.Fatal("empty policy restricts nothing")
	}
	if AccessNobody().MatchesRoles([]string{"owner", "admin"}) {
		t.Fatal("AccessNobody must match nobody")
	}
	ro := AccessReadOnly()
	if ro.Rule(AccessDelete).MatchesRoles([]string{"owner"}) || !ro.Rule(AccessList).Public {
		t.Fatalf("read-only preset: %+v", ro)
	}
	so := AccessStaffOnly("a", "b")
	if !so.Rule(AccessList).MatchesRoles([]string{"b"}) || so.Rule(AccessList).MatchesRoles([]string{"c"}) {
		t.Fatalf("staff-only preset: %+v", so)
	}
	r := AccessRoles("a").OrRoles("b").OrCapabilities("x.y")
	if !r.MatchesRoles([]string{"b"}) || len(r.Capabilities) != 1 {
		t.Fatalf("combinators: %+v", r)
	}
	raw, _ := json.Marshal(p)
	if !strings.Contains(string(raw), `"list":{"public":true}`) {
		t.Fatalf("wire: %s", raw)
	}
}

type policyModel struct{}

func (policyModel) DefineAccess() AccessPolicy { return AccessStaffOnly("x") }

func TestAccessPolicyForResolution(t *testing.T) {
	if _, ok := AccessPolicyFor("nope_model", struct{}{}); ok {
		t.Fatal("a model without policy has none")
	}
	p, ok := AccessPolicyFor("policy_model", policyModel{})
	if !ok || !p.Rule(AccessCreate).MatchesRoles([]string{"x"}) {
		t.Fatalf("DefineAccess: %+v", p)
	}
	SetAccessPolicy("policy_model_override", AccessReadOnly())
	p, _ = AccessPolicyFor("policy_model_override", policyModel{})
	if !p.Rule(AccessList).Public {
		t.Fatal("SetAccessPolicy must win over DefineAccess")
	}
}

func TestWithRolesIsLazyAndDelegates(t *testing.T) {
	u := &BaseUser{Role: "owner"}
	u.ID = uuid.New()
	calls := 0
	p := WithRoles(u, func() []string { calls++; return []string{"store.admin"} })
	if p.GetID() != u.ID || p.GetRole() != "owner" {
		t.Fatal("must delegate AuthUser")
	}
	if calls != 0 {
		t.Fatal("roles must resolve lazily")
	}
	for i := 0; i < 3; i++ {
		got := PrincipalRoles(p)
		if len(got) != 2 || got[0] != "owner" || got[1] != "store.admin" {
			t.Fatalf("roles: %v", got)
		}
	}
	if calls != 1 {
		t.Fatalf("resolver called %d times, want 1", calls)
	}
	if WithRoles(u, nil) != AuthUser(u) || PrincipalRoles(u)[0] != "owner" || PrincipalRoles(nil) != nil {
		t.Fatal("nil resolver / plain user")
	}
}

type singletonThing struct{ SingletonModel }

func TestSingletonMarker(t *testing.T) {
	if !IsSingletonModel("x", singletonThing{}) || !IsSingletonModel("x", &singletonThing{}) {
		t.Fatal("embedded SingletonModel must mark the model")
	}
	if IsSingletonModel("y", struct{}{}) {
		t.Fatal("plain model is not a singleton")
	}
	MarkSingleton("marked_key")
	if !IsSingletonModel("marked_key", struct{}{}) {
		t.Fatal("MarkSingleton")
	}
	raw, _ := json.Marshal(singletonThing{})
	if string(raw) != "{}" {
		t.Fatalf("marker must not reach JSON: %s", raw)
	}
}

func TestFieldErrorsAndRuleHelpers(t *testing.T) {
	var fe FieldErrors
	if fe.Err() != nil {
		t.Fatal("empty FieldErrors must be a nil error")
	}
	fe = FieldErrors{"b": "y", "a": "x"}
	var target FieldErrors
	if !errors.As(fe.Err(), &target) || fe.Error() != "validation failed: a: x; b: y" {
		t.Fatalf("FieldErrors: %v", fe.Error())
	}
	r := Range(1, 5).WithPattern(`^\d$`).WithCustom("digits")
	if *r.Min != 1 || *r.Max != 5 || r.Regex == "" || r.Custom != "digits" {
		t.Fatalf("rule: %+v", r)
	}
	if MinValue(2).Max != nil || *MaxValue(3).Max != 3 || Pattern("a").Regex != "a" {
		t.Fatal("helpers")
	}
}
