package security

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/google/uuid"
)

// fixture: an org with the storefront, customers, a payment provider and an
// unrelated "evil" addon. Declared event:emit per addon mirrors the manifests.
func emitFixture() EmitFactsSource {
	installed := []string{"storefront_cart", "storefront_api", "customers", "stripe", "products", "evil", "inventory", "skydropx", "emissary", "cash_recycler", "pos"}
	provides := map[string][]string{
		"stripe":          {"commerce.payments.session", "commerce.payments.confirm", "commerce.payments.status"},
		"storefront_cart": nil,
		"skydropx":        {"commerce.shipping.rates", "commerce.shipping.label", "commerce.shipping.track"},
		"emissary":        {"commerce.fulfillment.create", "commerce.fulfillment.cancel"},
		// A terminal-only provider (cash recycler) is a payments provider.
		"cash_recycler": {"commerce.payments.terminal.create", "commerce.payments.status"},
		"pos":           nil,
	}
	declared := map[string][]string{
		"storefront_cart": {"storefront_cart.*"},
		"storefront_api":  {"customers.external_return_requested"},
		"stripe":          {"stripe.*", "commerce.payments.confirmed"},
		"products":        {"commerce.payments.confirmed"},
		"evil":            {"*"},
		"inventory":       {"inventory.*"},
		"skydropx":        {"shipping.tracking_updated", "commerce.shipping.tracking_updated"},
		"emissary":        {"emissary.*", "commerce.fulfillment.updated", "commerce.shipping.tracking_updated"},
		"cash_recycler":   {"cash_recycler.*", "commerce.payments.confirmed"},
		"pos":             {"pos.*", "commerce.payments.confirmed"},
	}
	grants := []EmitterGrant{{Owner: "customers", Event: "customers.external_return_requested", Emitters: []string{"storefront_api", "storefront_customers"}}}
	return EmitFactsFunc(func(_ context.Context, _ uuid.UUID, emitter string) (EmitFacts, error) {
		var caps []manifest.Capability
		for _, t := range declared[emitter] {
			caps = append(caps, manifest.Capability{Kind: "event:emit", Target: t})
		}
		return EmitFacts{Installed: installed, Provides: provides[emitter], Grants: grants, Caps: Compile(emitter, caps)}, nil
	})
}

func newTestPolicy(src EmitFactsSource, reserved, declared EmitMode) (*EmitPolicy, *bytes.Buffer, *[]EmitViolation) {
	var buf bytes.Buffer
	var seen []EmitViolation
	p := &EmitPolicy{Source: src, Logger: log.New(&buf, "", 0)}
	p.SetMode(EmitGroupReserved, reserved)
	p.SetMode(EmitGroupDeclared, declared)
	p.OnViolation = func(_ context.Context, v EmitViolation) { seen = append(seen, v) }
	return p, &buf, &seen
}

func TestEmitPolicyTable(t *testing.T) {
	org := uuid.New()
	cases := []struct {
		name, emitter, event string
		rule                 EmitRule // "" = no violation
	}{
		{"own namespace", "storefront_cart", "storefront_cart.checkout_paid", ""},
		{"forged checkout_paid", "evil", "storefront_cart.checkout_paid", EmitRuleForeign},
		{"granted emitter", "storefront_api", "customers.external_return_requested", ""},
		{"ungranted emitter", "evil", "customers.external_return_requested", EmitRuleForeign},
		{"provider emits payment contract", "stripe", "commerce.payments.confirmed", ""},
		{"non-provider emits payment contract", "products", "commerce.payments.confirmed", EmitRuleContract},
		{"terminal provider emits payment contract", "cash_recycler", "commerce.payments.confirmed", ""},
		// A sales channel records payments, it does not confirm them: even
		// declaring the event, the POS cannot publish a confirmation.
		{"sales channel emits payment contract", "pos", "commerce.payments.confirmed", EmitRuleContract},
		{"kernel namespace", "evil", "kernel.dispatch.delivery_dead", EmitRuleKernel},
		{"bare kernel", "evil", "kernel", EmitRuleKernel},
		{"undeclared own event", "inventory", "stock.in_transit", EmitRuleDeclared},
		{"shared namespace declared", "evil", "shipping.updated", ""},
		{"carrier emits tracking contract", "skydropx", "commerce.shipping.tracking_updated", ""},
		{"forged tracking contract", "evil", "commerce.shipping.tracking_updated", EmitRuleContract},
		{"3PL emits fulfillment contract", "emissary", "commerce.fulfillment.updated", ""},
		{"forged fulfillment contract", "evil", "commerce.fulfillment.updated", EmitRuleContract},
		{"3PL without a shipping capability emits tracking contract", "emissary", "commerce.shipping.tracking_updated", EmitRuleContract},
		{"carrier emits fulfillment contract", "skydropx", "commerce.fulfillment.updated", EmitRuleContract},
		// The legacy name is not reserved: why subscribers move to the contract.
		{"legacy tracking name is forgeable", "evil", "shipping.tracking_updated", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, _, _ := newTestPolicy(emitFixture(), EmitModeEnforce, EmitModeEnforce)
			vs, err := p.Check(context.Background(), org, tc.emitter, tc.event)
			if tc.rule == "" {
				if err != nil || len(vs) != 0 {
					t.Fatalf("want allowed, got %v %+v", err, vs)
				}
				return
			}
			if len(vs) == 0 || vs[0].Rule != tc.rule {
				t.Fatalf("want rule %s, got %+v", tc.rule, vs)
			}
			var de *EmitDeniedError
			if !errors.As(err, &de) || !errors.Is(err, ErrEmitDenied) || de.Violation.Rule != tc.rule {
				t.Fatalf("want EmitDeniedError(%s), got %v", tc.rule, err)
			}
		})
	}
}

func TestEmitPolicyWarnModeReportsButAllows(t *testing.T) {
	p, buf, seen := newTestPolicy(emitFixture(), EmitModeWarn, EmitModeWarn)
	vs, err := p.Check(context.Background(), uuid.New(), "evil", "storefront_cart.checkout_paid")
	if err != nil {
		t.Fatalf("warn must not deny: %v", err)
	}
	if len(vs) != 1 || vs[0].Denied || vs[0].Mode != "warn" || vs[0].Owner != "storefront_cart" {
		t.Fatalf("violations = %+v", vs)
	}
	if len(*seen) != 1 {
		t.Fatalf("OnViolation calls = %d", len(*seen))
	}
	line := buf.String()
	for _, want := range []string{"metacore.event_emit.violation", "addon=evil", "event=storefront_cart.checkout_paid", "rule=foreign_namespace", "action=warned"} {
		if !strings.Contains(line, want) {
			t.Fatalf("log %q lacks %q", line, want)
		}
	}
}

func TestEmitPolicyGroupsAreIndependent(t *testing.T) {
	// Reserved enforced, declared warned: an undeclared own event passes with
	// a warning, a forged foreign event is denied.
	p, _, _ := newTestPolicy(emitFixture(), EmitModeEnforce, EmitModeWarn)
	if vs, err := p.Check(context.Background(), uuid.New(), "inventory", "stock.in_transit"); err != nil || len(vs) != 1 || vs[0].Rule != EmitRuleDeclared {
		t.Fatalf("undeclared in warn: %v %+v", err, vs)
	}
	if _, err := p.Check(context.Background(), uuid.New(), "evil", "storefront_cart.checkout_paid"); !errors.Is(err, ErrEmitDenied) {
		t.Fatalf("foreign in enforce: %v", err)
	}
	// Declared enforced, reserved off: only rule 4 counts.
	p.SetMode(EmitGroupReserved, EmitModeOff)
	p.SetMode(EmitGroupDeclared, EmitModeEnforce)
	if vs, err := p.Check(context.Background(), uuid.New(), "evil", "kernel.x"); err != nil || len(vs) != 0 {
		t.Fatalf("reserved off must skip rule 1 (evil declares *): %v %+v", err, vs)
	}
	if _, err := p.Check(context.Background(), uuid.New(), "inventory", "stock.in_transit"); !errors.Is(err, ErrEmitDenied) {
		t.Fatalf("declared enforce: %v", err)
	}
	// Both off: nothing evaluated.
	p.SetMode(EmitGroupDeclared, EmitModeOff)
	if vs, err := p.Check(context.Background(), uuid.New(), "evil", "kernel.x"); err != nil || vs != nil {
		t.Fatalf("all off: %v %+v", err, vs)
	}
}

func TestEmitPolicyMultipleViolationsAndForeignGrantIgnored(t *testing.T) {
	// products emits in stripe's namespace, undeclared: two violations.
	p, _, seen := newTestPolicy(emitFixture(), EmitModeWarn, EmitModeWarn)
	vs, _ := p.Check(context.Background(), uuid.New(), "products", "stripe.payment_updated")
	if len(vs) != 2 || vs[0].Rule != EmitRuleForeign || vs[1].Rule != EmitRuleDeclared || len(*seen) != 2 {
		t.Fatalf("violations = %+v", vs)
	}
	// A grant written by someone other than the namespace owner is ignored.
	src := EmitFactsFunc(func(context.Context, uuid.UUID, string) (EmitFacts, error) {
		return EmitFacts{Installed: []string{"storefront_cart", "evil"}, Grants: []EmitterGrant{{Owner: "evil", Event: "storefront_cart.*", Emitters: []string{"evil"}}}}, nil
	})
	p2, _, _ := newTestPolicy(src, EmitModeEnforce, EmitModeEnforce)
	if _, err := p2.Check(context.Background(), uuid.New(), "evil", "storefront_cart.checkout_paid"); !errors.Is(err, ErrEmitDenied) {
		t.Fatalf("self-granted emitters must not open a foreign namespace: %v", err)
	}
}

func TestEmitPolicyFactsUnavailable(t *testing.T) {
	src := EmitFactsFunc(func(context.Context, uuid.UUID, string) (EmitFacts, error) {
		return EmitFacts{}, errors.New("db down")
	})
	p, _, _ := newTestPolicy(src, EmitModeEnforce, EmitModeEnforce)
	// Own namespace still works (rule 4 unknown, not evaluated).
	if vs, err := p.Check(context.Background(), uuid.New(), "storefront_cart", "storefront_cart.checkout_paid"); err != nil || len(vs) != 0 {
		t.Fatalf("own namespace with facts down: %v %+v", err, vs)
	}
	for _, ev := range []string{"customers.updated", "commerce.payments.confirmed"} {
		vs, err := p.Check(context.Background(), uuid.New(), "storefront_cart", ev)
		if !errors.Is(err, ErrEmitDenied) || vs[0].Rule != EmitRuleUnverified {
			t.Fatalf("%s with facts down must fail closed in enforce: %v %+v", ev, err, vs)
		}
	}
	// No source at all behaves the same.
	p2, _, _ := newTestPolicy(nil, EmitModeWarn, EmitModeWarn)
	if vs, err := p2.Check(context.Background(), uuid.New(), "a", "b.c"); err != nil || len(vs) != 1 || vs[0].Rule != EmitRuleUnverified {
		t.Fatalf("nil source: %v %+v", err, vs)
	}
}

func TestEmitPolicyUnknownCapsSkipsRule4(t *testing.T) {
	src := EmitFactsFunc(func(context.Context, uuid.UUID, string) (EmitFacts, error) {
		return EmitFacts{Installed: []string{"inventory"}}, nil
	})
	p, _, _ := newTestPolicy(src, EmitModeEnforce, EmitModeEnforce)
	if vs, err := p.Check(context.Background(), uuid.New(), "inventory", "stock.in_transit"); err != nil || len(vs) != 0 {
		t.Fatalf("nil Caps must skip rule 4: %v %+v", err, vs)
	}
}

func TestEmitModeParsing(t *testing.T) {
	for in, want := range map[string]EmitMode{"warn": EmitModeWarn, "ENFORCE": EmitModeEnforce, " off ": EmitModeOff, "shadow": EmitModeWarn, "deny": EmitModeEnforce} {
		if m, ok := ParseEmitMode(in); !ok || m != want {
			t.Fatalf("%q = %v,%v", in, m, ok)
		}
	}
	if _, ok := ParseEmitMode("enforcee"); ok {
		t.Fatal("typo must not parse")
	}
	t.Setenv(EnvEmitReserved, "")
	t.Setenv(EnvEmitDeclared, "enforcee")
	p := NewEmitPolicy(nil)
	if p.Mode(EmitGroupReserved) != EmitModeWarn || p.Mode(EmitGroupDeclared) != EmitModeWarn {
		t.Fatal("unset and typo must default to warn")
	}
	t.Setenv(EnvEmitReserved, "enforce")
	t.Setenv("METACORE_ENFORCE", "shadow")
	if NewEmitPolicy(nil).Mode(EmitGroupReserved) != EmitModeEnforce {
		t.Fatal("reserved mode must not follow METACORE_ENFORCE")
	}
	var nilPolicy *EmitPolicy
	if vs, err := nilPolicy.Check(context.Background(), uuid.New(), "a", "kernel.x"); vs != nil || err != nil {
		t.Fatal("nil policy must allow")
	}
}
