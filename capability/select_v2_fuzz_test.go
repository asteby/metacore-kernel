package capability

import (
	"errors"
	"math/rand"
	"reflect"
	"testing"
)

// Random vectors beyond the exhaustive table: duplicate and unsorted addon
// keys, multi-market selections, ready/unhealthy/empty status and non-positive
// priorities must all resolve exactly like the legacy Select.
func TestSelectRandomEquivalentToLegacy(t *testing.T) {
	one := mustContract(t, CommercePaymentsSession)
	fan := mustContract(t, CommerceShippingRates)
	rng := rand.New(rand.NewSource(1))
	names := []string{"b", "a", "c", "a", "d"}
	cs := [][]string{nil, {"MX"}, {"US"}, {"MX", "US"}, {}}
	mk := [][]string{nil, {"mx"}, {"us"}, {"mx", "us"}}
	sts := []ProviderStatus{"", StatusReady, StatusUnhealthy}
	markets := []Market{{}, {Code: "mx", Countries: []string{"MX"}}, {Code: "us", Countries: []string{"US"}}, {Code: "mx"}, {Countries: []string{"MX"}}}
	for it := 0; it < 20000; it++ {
		n := rng.Intn(6)
		var ps []Provider
		for i := 0; i < n; i++ {
			ps = append(ps, Provider{Addon: names[rng.Intn(len(names))], Countries: cs[rng.Intn(len(cs))], Markets: mk[rng.Intn(len(mk))], Default: rng.Intn(2) == 0, Status: sts[rng.Intn(3)], Priority: -rng.Intn(2)})
		}
		for _, c := range []Contract{one, fan} {
			for _, m := range markets {
				w, we := legacySelect(c, ps, m)
				g, ge := Select(c, ps, m)
				if !reflect.DeepEqual(w, g) || (we == nil) != (ge == nil) || (we != nil && we.Error() != ge.Error()) {
					t.Fatalf("diff %+v %+v: %v/%v vs %v/%v", ps, m, w, we, g, ge)
				}
				if we != nil {
					var a, b *SelectionError
					errors.As(we, &a)
					errors.As(ge, &b)
					if !reflect.DeepEqual(a.Candidates, b.Candidates) || !errors.Is(ge, a.err) {
						t.Fatalf("cand diff")
					}
				}
			}
		}
	}
}

// Invariants with Priority/Status: chosen is always usable, comes from the
// deciding tier only, and an explicit market selection never falls to defaults.
func TestSelectV2NeverLeavesLegacyCandidates(t *testing.T) {
	one := mustContract(t, CommercePaymentsSession)
	rng := rand.New(rand.NewSource(2))
	sts := []ProviderStatus{"", StatusReady, StatusUnhealthy, StatusNotConfigured}
	m := Market{Code: "mx", Countries: []string{"MX"}}
	for it := 0; it < 20000; it++ {
		n := rng.Intn(5)
		var ps []Provider
		for i := 0; i < n; i++ {
			p := Provider{Addon: string(rune('a' + i)), Countries: [][]string{nil, {"MX"}, {"US"}}[rng.Intn(3)], Default: rng.Intn(2) == 0, Status: sts[rng.Intn(4)], Priority: rng.Intn(3)}
			if rng.Intn(3) == 0 {
				p.Markets = []string{"mx"}
			}
			ps = append(ps, p)
		}
		g, err := Select(one, ps, m)
		anySel := false
		for _, p := range ps {
			if p.SelectedFor("mx") {
				anySel = true
			}
		}
		if err != nil {
			continue
		}
		if len(g) != 1 || !g[0].Usable() {
			t.Fatalf("bad %v", g)
		}
		if anySel && !g[0].SelectedFor("mx") {
			t.Fatalf("explicit selection bypassed: %+v -> %v", ps, g)
		}
		// legacy-with-only-usable-in-tier: if legacy was ambiguous, the winner
		// must be the unique best usable one of the legacy candidates.
		_, le := legacySelect(one, ps, m)
		var lse *SelectionError
		if errors.As(le, &lse) && errors.Is(le, ErrAmbiguousProvider) {
			ok := false
			for _, c := range lse.Candidates {
				if c == g[0].Addon {
					ok = true
				}
			}
			if !ok {
				t.Fatalf("winner outside legacy tie: %+v -> %v (%v)", ps, g, lse.Candidates)
			}
		} else if le == nil {
			lg, _ := legacySelect(one, ps, m)
			if lg[0].Addon != g[0].Addon {
				t.Fatalf("legacy resolved %s, new %s: %+v", lg[0].Addon, g[0].Addon, ps)
			}
		}
	}
}
