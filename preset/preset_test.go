package preset

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

func loadPitsline(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "pitsline_preset.json"))
	if err != nil {
		t.Fatalf("read pitsline manifest: %v", err)
	}
	return raw
}

// requiredKeys mirrors the non-optional addons declared in the pitsline
// manifest, in declaration order.
var pitslineRequired = []string{
	"inventory", "customers", "billing_sat", "pos", "accounting_lite",
	"purchases", "hr_lite", "vehicles", "tires_inventory", "workshop",
	"alignment", "balancing", "vulcanizado", "waybill_cartaporte",
}

var pitslineOptional = []string{"fleet_customers", "tire_warranty"}

func TestResolve_Pitsline(t *testing.T) {
	r, err := Resolve(loadPitsline(t))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if r.Key != "pitsline" {
		t.Fatalf("preset key = %q, want pitsline", r.Key)
	}
	if r.Version != "0.1.0" {
		t.Fatalf("preset version = %q, want 0.1.0", r.Version)
	}
	if len(r.Addons) != len(pitslineRequired)+len(pitslineOptional) {
		t.Fatalf("addon count = %d, want %d", len(r.Addons), len(pitslineRequired)+len(pitslineOptional))
	}

	gotReq := r.Required()
	if len(gotReq) != len(pitslineRequired) {
		t.Fatalf("required count = %d, want %d", len(gotReq), len(pitslineRequired))
	}
	for i, want := range pitslineRequired {
		if gotReq[i].Key != want {
			t.Errorf("required[%d] = %q, want %q (order must be preserved)", i, gotReq[i].Key, want)
		}
		if gotReq[i].Optional {
			t.Errorf("required[%d] %q marked optional", i, gotReq[i].Key)
		}
	}

	gotOpt := r.Optional()
	if len(gotOpt) != len(pitslineOptional) {
		t.Fatalf("optional count = %d, want %d", len(gotOpt), len(pitslineOptional))
	}
	for i, want := range pitslineOptional {
		if gotOpt[i].Key != want {
			t.Errorf("optional[%d] = %q, want %q", i, gotOpt[i].Key, want)
		}
		if !gotOpt[i].Optional {
			t.Errorf("optional[%d] %q not marked optional", i, gotOpt[i].Key)
		}
	}

	// Required addons must sort before optional in the combined list.
	for i := 0; i < len(pitslineRequired); i++ {
		if r.Addons[i].Optional {
			t.Errorf("Addons[%d] should be required (required-first ordering)", i)
		}
	}

	if r.Defaults["roles_file"] != "./defaults/roles.json" {
		t.Errorf("defaults not preserved: %v", r.Defaults)
	}
}

func TestResolveFromManifest_RejectsNonPreset(t *testing.T) {
	addon := &v3.Manifest{Kind: v3.KindAddon, Metadata: v3.Metadata{Key: "inventory"}}
	if _, err := ResolveFromManifest(addon); !errors.Is(err, ErrNotPreset) {
		t.Fatalf("ResolveFromManifest(addon) err = %v, want ErrNotPreset", err)
	}
	if _, err := ResolveFromManifest(nil); !errors.Is(err, ErrNotPreset) {
		t.Fatalf("ResolveFromManifest(nil) err = %v, want ErrNotPreset", err)
	}
	// kind=Preset but missing preset block.
	noBlock := &v3.Manifest{Kind: v3.KindPreset, Metadata: v3.Metadata{Key: "x"}}
	if _, err := ResolveFromManifest(noBlock); !errors.Is(err, ErrNotPreset) {
		t.Fatalf("ResolveFromManifest(no block) err = %v, want ErrNotPreset", err)
	}
}

func TestResolve_ParseError(t *testing.T) {
	if _, err := Resolve([]byte(`{not json`)); err == nil {
		t.Fatal("Resolve(bad json) want error")
	}
}

// TestInstallPreset_AllRequired stubs the per-addon installer and asserts the
// orchestration installs the full required set, in order, and reports a clean
// summary.
func TestInstallPreset_AllRequired(t *testing.T) {
	r, err := Resolve(loadPitsline(t))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	var installOrder []string
	install := func(a Addon) (bool, error) {
		installOrder = append(installOrder, a.Key)
		return true, nil
	}

	sum, err := InstallPreset(r, install, Options{})
	if err != nil {
		t.Fatalf("InstallPreset: %v", err)
	}

	// With no optional selection, only required addons are touched by the
	// installer; optional addons are skipped without calling install.
	if len(installOrder) != len(pitslineRequired) {
		t.Fatalf("installed %d addons, want %d required", len(installOrder), len(pitslineRequired))
	}
	for i, want := range pitslineRequired {
		if installOrder[i] != want {
			t.Errorf("install order[%d] = %q, want %q", i, installOrder[i], want)
		}
	}
	if got := len(sum.Installed()); got != len(pitslineRequired) {
		t.Errorf("summary installed = %d, want %d", got, len(pitslineRequired))
	}
	// Both optional addons skipped as "not selected".
	if got := len(sum.Skipped()); got != len(pitslineOptional) {
		t.Errorf("summary skipped = %d, want %d", got, len(pitslineOptional))
	}
	if got := len(sum.Failed()); got != 0 {
		t.Errorf("summary failed = %d, want 0", got)
	}
}

// TestInstallPreset_OptionalSelected opts into one optional addon and verifies
// only that one is installed (the other stays skipped).
func TestInstallPreset_OptionalSelected(t *testing.T) {
	r, _ := Resolve(loadPitsline(t))

	installed := map[string]bool{}
	install := func(a Addon) (bool, error) {
		installed[a.Key] = true
		return true, nil
	}

	sum, err := InstallPreset(r, install, Options{
		IncludeOptional: IncludeOptionalSet([]string{"tire_warranty"}),
	})
	if err != nil {
		t.Fatalf("InstallPreset: %v", err)
	}
	if !installed["tire_warranty"] {
		t.Error("opted-in optional tire_warranty was not installed")
	}
	if installed["fleet_customers"] {
		t.Error("non-selected optional fleet_customers was installed")
	}
	// Summary: required + 1 optional installed; 1 optional skipped.
	if got := len(sum.Installed()); got != len(pitslineRequired)+1 {
		t.Errorf("installed = %d, want %d", got, len(pitslineRequired)+1)
	}
	if got := sum.Skipped(); len(got) != 1 || got[0] != "fleet_customers" {
		t.Errorf("skipped = %v, want [fleet_customers]", got)
	}
}

// TestInstallPreset_Idempotent simulates already-installed addons: install
// returns (false, nil). They land in Skipped, not Installed.
func TestInstallPreset_Idempotent(t *testing.T) {
	r, _ := Resolve(loadPitsline(t))

	already := map[string]bool{"inventory": true, "pos": true}
	install := func(a Addon) (bool, error) {
		if already[a.Key] {
			return false, nil // idempotent skip
		}
		return true, nil
	}

	sum, err := InstallPreset(r, install, Options{})
	if err != nil {
		t.Fatalf("InstallPreset: %v", err)
	}
	skipped := sum.Skipped()
	// 2 already-installed + 2 optional-not-selected.
	wantSkipped := 2 + len(pitslineOptional)
	if len(skipped) != wantSkipped {
		t.Errorf("skipped = %v (%d), want %d", skipped, len(skipped), wantSkipped)
	}
	if got := len(sum.Installed()); got != len(pitslineRequired)-2 {
		t.Errorf("installed = %d, want %d", got, len(pitslineRequired)-2)
	}
}

// TestInstallPreset_RequiredFailureAborts verifies a required-addon failure
// aborts the loop by default and returns a partial summary + error.
func TestInstallPreset_RequiredFailureAborts(t *testing.T) {
	r, _ := Resolve(loadPitsline(t))

	notEntitled := errors.New("403 not entitled")
	calls := 0
	install := func(a Addon) (bool, error) {
		calls++
		if a.Key == "billing_sat" { // 3rd required addon
			return false, notEntitled
		}
		return true, nil
	}

	sum, err := InstallPreset(r, install, Options{})
	if err == nil {
		t.Fatal("InstallPreset: want error on required addon failure")
	}
	if !errors.Is(err, notEntitled) {
		t.Errorf("err = %v, want wrap of notEntitled", err)
	}
	// Aborted at the 3rd addon — no further installs attempted.
	if calls != 3 {
		t.Errorf("install called %d times, want 3 (abort on first required failure)", calls)
	}
	if got := sum.Failed(); len(got) != 1 || got[0] != "billing_sat" {
		t.Errorf("failed = %v, want [billing_sat]", got)
	}
}

// TestInstallPreset_RequiredFailureContinue verifies ContinueOnRequiredError
// keeps going and collects all failures.
func TestInstallPreset_RequiredFailureContinue(t *testing.T) {
	r, _ := Resolve(loadPitsline(t))

	install := func(a Addon) (bool, error) {
		if a.Key == "billing_sat" {
			return false, errors.New("boom")
		}
		return true, nil
	}

	sum, err := InstallPreset(r, install, Options{ContinueOnRequiredError: true})
	if err != nil {
		t.Fatalf("InstallPreset with continue: unexpected err %v", err)
	}
	if got := sum.Failed(); len(got) != 1 || got[0] != "billing_sat" {
		t.Errorf("failed = %v, want [billing_sat]", got)
	}
	// All other required addons still installed.
	if got := len(sum.Installed()); got != len(pitslineRequired)-1 {
		t.Errorf("installed = %d, want %d", got, len(pitslineRequired)-1)
	}
}

// TestInstallPreset_OptionalFailureDoesNotAbort: an opted-in optional addon
// that fails (e.g. not entitled) must NOT abort the preset.
func TestInstallPreset_OptionalFailureDoesNotAbort(t *testing.T) {
	r, _ := Resolve(loadPitsline(t))

	install := func(a Addon) (bool, error) {
		if a.Key == "tire_warranty" {
			return false, errors.New("not entitled")
		}
		return true, nil
	}

	sum, err := InstallPreset(r, install, Options{
		IncludeOptional: IncludeOptionalSet([]string{"tire_warranty"}),
	})
	if err != nil {
		t.Fatalf("optional failure must not abort: got err %v", err)
	}
	if got := sum.Failed(); len(got) != 1 || got[0] != "tire_warranty" {
		t.Errorf("failed = %v, want [tire_warranty]", got)
	}
	if got := len(sum.Installed()); got != len(pitslineRequired) {
		t.Errorf("installed = %d, want %d", got, len(pitslineRequired))
	}
}

// TestInstallPreset_RetryRecoversTransientFailure: a required addon that
// fails once (transient hub timeout) but succeeds on retry must land as
// Installed, not Failed.
func TestInstallPreset_RetryRecoversTransientFailure(t *testing.T) {
	r, _ := Resolve(loadPitsline(t))

	attempts := 0
	install := func(a Addon) (bool, error) {
		if a.Key == "tires_inventory" {
			attempts++
			if attempts == 1 {
				return false, errors.New("hub: request timeout")
			}
		}
		return true, nil
	}

	sum, err := InstallPreset(r, install, Options{RetryAttempts: 1})
	if err != nil {
		t.Fatalf("InstallPreset with retry: unexpected err %v", err)
	}
	if got := sum.Failed(); len(got) != 0 {
		t.Errorf("failed = %v, want none (retry should have recovered)", got)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2 (initial + 1 retry)", attempts)
	}
}

// TestInstallPreset_RetryExhaustedStillFails: an addon that keeps failing
// past RetryAttempts is still recorded as Failed, and FailedReasons keeps
// the underlying error message.
func TestInstallPreset_RetryExhaustedStillFails(t *testing.T) {
	r, _ := Resolve(loadPitsline(t))

	install := func(a Addon) (bool, error) {
		if a.Key == "tires_inventory" {
			return false, errors.New("hub: request timeout")
		}
		return true, nil
	}

	sum, err := InstallPreset(r, install, Options{RetryAttempts: 2, ContinueOnRequiredError: true})
	if err != nil {
		t.Fatalf("InstallPreset with continue: unexpected err %v", err)
	}
	if got := sum.Failed(); len(got) != 1 || got[0] != "tires_inventory" {
		t.Errorf("failed = %v, want [tires_inventory]", got)
	}
	if got := sum.FailedReasons()["tires_inventory"]; got != "hub: request timeout" {
		t.Errorf("FailedReasons()[tires_inventory] = %q, want %q", got, "hub: request timeout")
	}
}

func TestInstallPreset_NilFunc(t *testing.T) {
	r, _ := Resolve(loadPitsline(t))
	if _, err := InstallPreset(r, nil, Options{}); err == nil {
		t.Fatal("InstallPreset(nil func) want error")
	}
}

// TestSortByDeps_TopoSortsRequires verifies that addons declaring
// `requires` land after their prereqs even when the manifest lists them
// in the wrong order. Three addons: c requires b, b requires a. Input
// order intentionally reverses the dep direction.
func TestSortByDeps_TopoSortsRequires(t *testing.T) {
	in := []Addon{
		{Key: "c", Requires: []string{"b"}},
		{Key: "b", Requires: []string{"a"}},
		{Key: "a"},
	}
	out, err := SortByDeps(in)
	if err != nil {
		t.Fatalf("SortByDeps: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("len=%d want 3", len(out))
	}
	if out[0].Key != "a" || out[1].Key != "b" || out[2].Key != "c" {
		t.Fatalf("order = [%s %s %s], want [a b c]", out[0].Key, out[1].Key, out[2].Key)
	}
}

// TestSortByDeps_PreservesOrderWithoutRequires confirms that without
// `requires` the input order is preserved exactly — no spurious reshuffle
// for legacy presets.
func TestSortByDeps_PreservesOrderWithoutRequires(t *testing.T) {
	in := []Addon{{Key: "z"}, {Key: "a"}, {Key: "m"}}
	out, err := SortByDeps(in)
	if err != nil {
		t.Fatalf("SortByDeps: %v", err)
	}
	for i := range in {
		if out[i].Key != in[i].Key {
			t.Fatalf("index %d: got %s want %s — order must be preserved", i, out[i].Key, in[i].Key)
		}
	}
}

// TestSortByDeps_RejectsCycles_TwoNode covers a→b→a cycle.
func TestSortByDeps_RejectsCycles_TwoNode(t *testing.T) {
	in := []Addon{
		{Key: "a", Requires: []string{"b"}},
		{Key: "b", Requires: []string{"a"}},
	}
	if _, err := SortByDeps(in); !errors.Is(err, ErrCyclicDependency) {
		t.Fatalf("err = %v, want ErrCyclicDependency", err)
	}
}

// TestSortByDeps_RejectsCycles_SelfReference covers an addon requiring
// itself (the degenerate 1-node cycle).
func TestSortByDeps_RejectsCycles_SelfReference(t *testing.T) {
	in := []Addon{{Key: "a", Requires: []string{"a"}}}
	if _, err := SortByDeps(in); !errors.Is(err, ErrCyclicDependency) {
		t.Fatalf("err = %v, want ErrCyclicDependency", err)
	}
}

// TestSortByDeps_RejectsUnknownDependency verifies a typo / missing addon
// reference surfaces as ErrUnknownDependency.
func TestSortByDeps_RejectsUnknownDependency(t *testing.T) {
	in := []Addon{{Key: "a", Requires: []string{"missing"}}}
	if _, err := SortByDeps(in); !errors.Is(err, ErrUnknownDependency) {
		t.Fatalf("err = %v, want ErrUnknownDependency", err)
	}
}

// TestInstallPreset_RespectsDependencyOrder wires three addons where the
// preset author lists them in the wrong order but declares `requires` so
// the resolver topo-sorts them. The recording install func captures the
// actual call order — must match the dep order, not the manifest order.
func TestInstallPreset_RespectsDependencyOrder(t *testing.T) {
	r := Resolved{
		Key: "demo",
		Addons: []Addon{
			{Key: "c", Version: "1.0.0", Requires: []string{"b"}},
			{Key: "b", Version: "1.0.0", Requires: []string{"a"}},
			{Key: "a", Version: "1.0.0"},
		},
	}
	// SortByDeps in ResolveFromManifest is the realistic path; here we
	// hit InstallPreset with a pre-sorted Resolved as ResolveFromManifest
	// would emit (depth-first install order).
	sorted, err := SortByDeps(r.Addons)
	if err != nil {
		t.Fatalf("SortByDeps: %v", err)
	}
	r.Addons = sorted

	var order []string
	_, err = InstallPreset(r, func(a Addon) (bool, error) {
		order = append(order, a.Key)
		return true, nil
	}, Options{})
	if err != nil {
		t.Fatalf("InstallPreset: %v", err)
	}
	if len(order) != 3 || order[0] != "a" || order[1] != "b" || order[2] != "c" {
		t.Fatalf("install order = %v, want [a b c]", order)
	}
}
