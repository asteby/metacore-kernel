package manifest

import (
	"testing"

	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

func TestProvidesCapabilitiesForN1(t *testing.T) {
	m := Manifest{
		Key: "mercadopago",
		Implements: []v3.Implements{
			{Contract: "payments.charge", Version: "^1.2", Countries: []string{"MX", "AR"}},
			{Contract: "payments.payout", Version: "^1", Operations: []string{"send"}},
		},
		ProvidesCapabilities: []CapabilityProviderDef{{Key: "payments.charge.create", Trigger: &ActionTrigger{Type: "wasm", Export: "charge_create"}}},
	}
	ops := func(c string) ([]string, bool) {
		if c == "payments.charge" {
			return []string{"create", "refund", "status"}, true
		}
		return nil, false
	}
	got := m.ProvidesCapabilitiesForN1(ops)
	want := []string{"payments.charge.create", "payments.charge.refund", "payments.charge.status", "payments.payout.send"}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i, k := range want {
		if got[i].Key != k {
			t.Fatalf("[%d] = %q want %q", i, got[i].Key, k)
		}
	}
	if got[0].Trigger.Export != "charge_create" {
		t.Fatal("the addon's own entry must win")
	}
	if got[1].Trigger.Export != "refund" || got[1].Trigger.Type != "wasm" || len(got[1].Countries) != 2 {
		t.Fatalf("projection = %+v", got[1])
	}
	if len(m.ProvidesCapabilities) != 1 {
		t.Fatal("m was modified")
	}
	if n := len((&Manifest{}).ProvidesCapabilitiesForN1(nil)); n != 0 {
		t.Fatalf("empty manifest projected %d", n)
	}
}

func TestInferImplements(t *testing.T) {
	known := func(c string) bool { return c == "payments.charge" || c == "payments.charge.special" }
	got := InferImplements([]string{"payments.charge.create", "payments.charge.special.x", "other.thing.do"}, nil, known)
	if len(got) != 2 || got[0].Contract != "payments.charge" || got[1].Contract != "payments.charge.special" || got[0].Version != "*" {
		t.Fatalf("got %+v", got)
	}
	if got := InferImplements([]string{"payments.charge.create"}, []v3.Implements{{Contract: "payments.charge"}}, known); got != nil {
		t.Fatalf("already implemented: %+v", got)
	}
}
