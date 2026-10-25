package capability

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// legacySelect is a verbatim copy of Select as it was before Priority/Status
// (X1.5). The equivalence test below pins the new resolver to it: with no
// priority and no status anywhere the result, including ambiguous_provider on
// a tie, must be identical.
func legacySelect(c Contract, providers []Provider, m Market) ([]Provider, error) {
	sorted := append([]Provider(nil), providers...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Addon < sorted[j].Addon })

	var selected, eligible []Provider
	for _, p := range sorted {
		if p.SelectedFor(m.Code) {
			selected = append(selected, p)
		}
		if p.Serves(m) {
			eligible = append(eligible, p)
		}
	}
	fail := func(err error, ps []Provider) error {
		e := &SelectionError{Capability: c.Key, Market: m.Code, err: err}
		for _, p := range ps {
			e.Candidates = append(e.Candidates, p.Addon)
		}
		return e
	}
	if c.Fanout() {
		if len(selected) > 0 {
			return selected, nil
		}
		if len(eligible) == 0 {
			return nil, fail(ErrNoProvider, sorted)
		}
		return eligible, nil
	}
	switch len(selected) {
	case 1:
		return selected, nil
	case 0:
	default:
		return nil, fail(ErrAmbiguousProvider, selected)
	}
	var defaults []Provider
	for _, p := range eligible {
		if p.Default {
			defaults = append(defaults, p)
		}
	}
	switch {
	case len(defaults) == 1:
		return defaults, nil
	case len(defaults) > 1:
		return nil, fail(ErrAmbiguousProvider, defaults)
	case len(eligible) == 1:
		return eligible, nil
	case len(eligible) == 0:
		return nil, fail(ErrNoProvider, sorted)
	}
	return nil, fail(ErrAmbiguousProvider, eligible)
}

// Exhaustive: up to 3 providers, each with every combination of
// {countries none|MX|US} x default x selected-for-mx, over markets "", mx, us,
// against a One and a fan-out contract.
func TestSelectEquivalentToLegacyWithoutPriorityOrStatus(t *testing.T) {
	one := mustContract(t, CommercePaymentsSession)
	fan := mustContract(t, CommerceShippingRates)
	if one.Fanout() || !fan.Fanout() {
		t.Fatal("test contracts have the wrong cardinality")
	}
	countries := [][]string{nil, {"MX"}, {"US"}}
	var variants []Provider
	for _, cs := range countries {
		for _, def := range []bool{false, true} {
			for _, sel := range []bool{false, true} {
				p := Provider{Countries: cs, Default: def}
				if sel {
					p.Markets = []string{"mx"}
				}
				variants = append(variants, p)
			}
		}
	}
	markets := []Market{{}, {Code: "mx", Countries: []string{"MX"}}, {Code: "us", Countries: []string{"US"}}, {Code: "mx"}}
	n := 0
	var walk func(depth int, cur []Provider)
	walk = func(depth int, cur []Provider) {
		check := func() {
			for _, c := range []Contract{one, fan} {
				for _, m := range markets {
					in := append([]Provider(nil), cur...)
					want, wantErr := legacySelect(c, in, m)
					got, gotErr := Select(c, in, m)
					n++
					if !reflect.DeepEqual(addons(want), addons(got)) {
						t.Fatalf("%s market %+v providers %+v: got %v want %v", c.Key, m, cur, addons(got), addons(want))
					}
					if (wantErr == nil) != (gotErr == nil) {
						t.Fatalf("%s market %+v providers %+v: err got %v want %v", c.Key, m, cur, gotErr, wantErr)
					}
					if wantErr != nil {
						var we, ge *SelectionError
						if !errors.As(wantErr, &we) || !errors.As(gotErr, &ge) || !errors.Is(gotErr, we.err) ||
							!reflect.DeepEqual(we.Candidates, ge.Candidates) || we.Error() != ge.Error() {
							t.Fatalf("%s market %+v providers %+v: err got %v want %v", c.Key, m, cur, gotErr, wantErr)
						}
					}
				}
			}
		}
		check()
		if depth == 3 {
			return
		}
		for _, v := range variants {
			p := v
			p.Addon = fmt.Sprintf("a%d", depth)
			walk(depth+1, append(append([]Provider(nil), cur...), p))
		}
	}
	walk(0, nil)
	if n < 10000 {
		t.Fatalf("only %d vectors", n)
	}
}

func addons(ps []Provider) []string {
	out := []string{}
	for _, p := range ps {
		out = append(out, p.Addon)
	}
	return out
}

func TestSelectTieStaysAmbiguousAndFailsClosed(t *testing.T) {
	one := mustContract(t, CommercePaymentsSession)
	ps := []Provider{{Addon: "mercadopago"}, {Addon: "stripe"}}
	_, err := Select(one, ps, Market{Code: "mx", Countries: []string{"MX"}})
	if !errors.Is(err, ErrAmbiguousProvider) {
		t.Fatalf("tie must be ambiguous: %v", err)
	}
	// Equal priorities are still a tie.
	ps[0].Priority, ps[1].Priority = 1, 1
	_, err = Select(one, ps, Market{})
	var se *SelectionError
	if !errors.Is(err, ErrAmbiguousProvider) || !errors.As(err, &se) || strings.Join(se.Candidates, ",") != "mercadopago,stripe" {
		t.Fatalf("equal priorities must stay ambiguous: %v", err)
	}
}

func TestSelectV2PriorityAndFallback(t *testing.T) {
	one := mustContract(t, CommercePaymentsSession)
	mx := Market{Code: "mx", Countries: []string{"MX"}}
	pick := func(ps []Provider, m Market) (string, error) {
		got, err := Select(one, ps, m)
		return strings.Join(addons(got), ","), err
	}
	t.Run("lower priority wins among eligible", func(t *testing.T) {
		got, err := pick([]Provider{{Addon: "a", Priority: 2}, {Addon: "b", Priority: 1}}, mx)
		if err != nil || got != "b" {
			t.Fatalf("got %q %v", got, err)
		}
	})
	t.Run("positive priority beats unset", func(t *testing.T) {
		got, err := pick([]Provider{{Addon: "a"}, {Addon: "b", Priority: 9}}, mx)
		if err != nil || got != "b" {
			t.Fatalf("got %q %v", got, err)
		}
	})
	t.Run("priority orders the market selection tier", func(t *testing.T) {
		got, err := pick([]Provider{
			{Addon: "a", Markets: []string{"mx"}, Priority: 2},
			{Addon: "b", Markets: []string{"mx"}, Priority: 1},
			{Addon: "c", Default: true, Priority: 1},
		}, mx)
		if err != nil || got != "b" {
			t.Fatalf("got %q %v", got, err)
		}
	})
	t.Run("not_configured is skipped: backup wins", func(t *testing.T) {
		got, err := pick([]Provider{
			{Addon: "a", Markets: []string{"mx"}, Priority: 1, Status: StatusNotConfigured},
			{Addon: "b", Markets: []string{"mx"}, Priority: 2},
		}, mx)
		if err != nil || got != "b" {
			t.Fatalf("got %q %v", got, err)
		}
	})
	t.Run("not_configured resolves a former tie", func(t *testing.T) {
		got, err := pick([]Provider{{Addon: "a", Status: StatusNotConfigured}, {Addon: "b"}}, mx)
		if err != nil || got != "b" {
			t.Fatalf("got %q %v", got, err)
		}
	})
	t.Run("unhealthy is still routed", func(t *testing.T) {
		got, err := pick([]Provider{{Addon: "a", Status: StatusUnhealthy, Priority: 1}, {Addon: "b", Priority: 2}}, mx)
		if err != nil || got != "a" {
			t.Fatalf("got %q %v", got, err)
		}
	})
	t.Run("all not_configured is ErrNotConfigured, not ErrNoProvider", func(t *testing.T) {
		_, err := pick([]Provider{{Addon: "a", Status: StatusNotConfigured}}, mx)
		var se *SelectionError
		if !errors.Is(err, ErrNotConfigured) || errors.Is(err, ErrNoProvider) || !errors.As(err, &se) || strings.Join(se.Candidates, ",") != "a" {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("a not_configured selection does not leak to another tier", func(t *testing.T) {
		_, err := pick([]Provider{
			{Addon: "a", Markets: []string{"mx"}, Status: StatusNotConfigured},
			{Addon: "b", Default: true},
		}, mx)
		if !errors.Is(err, ErrNotConfigured) {
			t.Fatalf("explicit selection must fail closed: %v", err)
		}
	})
	t.Run("priority does not bypass Serves", func(t *testing.T) {
		got, err := pick([]Provider{{Addon: "a", Countries: []string{"US"}, Priority: 1}, {Addon: "b", Countries: []string{"MX"}, Priority: 5}}, mx)
		if err != nil || got != "b" {
			t.Fatalf("got %q %v", got, err)
		}
	})
	t.Run("nothing installed stays ErrNoProvider", func(t *testing.T) {
		if _, err := pick(nil, mx); !errors.Is(err, ErrNoProvider) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("fan-out drops not_configured, keeps key order", func(t *testing.T) {
		fan := mustContract(t, CommerceShippingRates)
		got, err := Select(fan, []Provider{{Addon: "z"}, {Addon: "b", Status: StatusNotConfigured}, {Addon: "a", Priority: 9}}, mx)
		if err != nil || strings.Join(addons(got), ",") != "a,z" {
			t.Fatalf("got %v %v", addons(got), err)
		}
		_, err = Select(fan, []Provider{{Addon: "b", Status: StatusNotConfigured}}, mx)
		if !errors.Is(err, ErrNotConfigured) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("pinned ignores status and priority", func(t *testing.T) {
		got, err := SelectPinned(mustContract(t, CommercePaymentsConfirm), []Provider{{Addon: "a", Status: StatusNotConfigured}}, mx, "a")
		if err != nil || len(got) != 1 {
			t.Fatalf("a follow-up on an existing record must still reach its provider: %v", err)
		}
	})
}

// Engine.Query, Engine.ListProviders and Eligible share one resolver: what the
// listing marks as reached is exactly what a query invokes.
func TestEligibleSharesTheResolver(t *testing.T) {
	one := mustContract(t, CommercePaymentsSession)
	mx := Market{Code: "mx", Countries: []string{"MX"}}
	ps := []Provider{
		{Addon: "stripe", Countries: []string{"US"}, Priority: 1},
		{Addon: "mp", Countries: []string{"MX"}, Priority: 2, Label: "MP"},
		{Addon: "oxxo", Countries: []string{"MX"}, Priority: 1, Status: StatusNotConfigured},
	}
	views, err := Eligible(one, ps, mx)
	if err != nil {
		t.Fatal(err)
	}
	var reached []string
	for _, v := range views {
		if v.Reached {
			reached = append(reached, v.Addon)
		}
	}
	chosen, _ := Select(one, ps, mx)
	if !reflect.DeepEqual(reached, addons(chosen)) || len(reached) != 1 || reached[0] != "mp" {
		t.Fatalf("reached %v chosen %v", reached, addons(chosen))
	}
	if views[1].Status != StatusNotConfigured || views[2].Serves || views[0].Status != StatusReady {
		t.Fatalf("views %+v", views)
	}

	eng := &Engine{Source: ProviderSourceFunc(func(context.Context, uuid.UUID, string) ([]Provider, error) { return ps, nil })}
	l, err := eng.ListProviders(context.Background(), Query{Org: uuid.New(), Market: mx, Capability: CommercePaymentsSession})
	if err != nil || l.Resolution != "ok" || !reflect.DeepEqual(l.Providers, views) {
		t.Fatalf("listing %+v %v", l, err)
	}
	eng.Source = ProviderSourceFunc(func(context.Context, uuid.UUID, string) ([]Provider, error) {
		return []Provider{{Addon: "a"}, {Addon: "b"}}, nil
	})
	if l, _ = eng.ListProviders(context.Background(), Query{Market: mx, Capability: CommercePaymentsSession}); l.Resolution != "ambiguous_provider" {
		t.Fatalf("resolution %q", l.Resolution)
	}
	eng.Source = ProviderSourceFunc(func(context.Context, uuid.UUID, string) ([]Provider, error) {
		return []Provider{{Addon: "a", Status: StatusNotConfigured}}, nil
	})
	if l, _ = eng.ListProviders(context.Background(), Query{Market: mx, Capability: CommercePaymentsSession}); l.Resolution != "not_configured" {
		t.Fatalf("resolution %q", l.Resolution)
	}
}
