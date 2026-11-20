package installer

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/lifecycle"
	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
	"github.com/google/uuid"
)

func mf(key, version string, reqs ...manifest.AddonRequirement) manifest.Manifest {
	return manifest.Manifest{Key: key, Version: version, AddonRequires: reqs}
}

func req(key, rng string) manifest.AddonRequirement {
	return manifest.AddonRequirement{Key: key, Range: rng}
}

func opt(key, rng string) manifest.AddonRequirement {
	return manifest.AddonRequirement{Key: key, Range: rng, Optional: true}
}

func ia(m manifest.Manifest) InstalledAddon { return InstalledFromManifest(m) }

func TestFromV3ProjectsPeerRequires(t *testing.T) {
	m := &v3.Manifest{}
	m.Metadata.Key = "x"
	m.Compatibility.Requires = []v3.Requirement{
		{Key: "kernel", Version: ">=3.0.0 <4.0.0"},
		{Key: "metacore-kernel", Version: ">=0.291.0"},
		{Key: "customers", Version: ">=1.0.0"},
		{Key: "mercadopago?", Version: "^2.0.0"},
		{Key: "pos", Version: ">=1", Optional: true},
	}
	out := manifest.FromV3(m)
	want := []manifest.AddonRequirement{
		{Key: "customers", Range: ">=1.0.0"},
		{Key: "mercadopago", Range: "^2.0.0", Optional: true},
		{Key: "pos", Range: ">=1", Optional: true},
	}
	if !reflect.DeepEqual(out.AddonRequires, want) {
		t.Fatalf("AddonRequires = %+v, want %+v", out.AddonRequires, want)
	}
	if out.Kernel != ">=3.0.0 <4.0.0" || out.Runtime != ">=0.291.0" {
		t.Fatalf("reserved ranges lost: %q %q", out.Kernel, out.Runtime)
	}
}

func TestCheckInstallable(t *testing.T) {
	base := mf("customers", "1.2.0")
	cand := mf("workshop", "1.0.0", req("customers", ">=1.0.0 <2.0.0"), opt("mercadopago", ">=9.0.0"))

	if err := CheckInstallable([]InstalledAddon{ia(base)}, cand); err != nil {
		t.Fatalf("satisfied (optional absent) must pass: %v", err)
	}
	// missing
	err := CheckInstallable(nil, cand)
	var miss ErrMissingDependency
	if !errors.As(err, &miss) || miss.Addon != "workshop" || miss.Required != "customers" || miss.Have != "" {
		t.Fatalf("want ErrMissingDependency, got %v", err)
	}
	// out of range
	err = CheckInstallable([]InstalledAddon{ia(mf("customers", "2.0.0"))}, cand)
	var inc ErrIncompatibleVersion
	if !errors.As(err, &inc) || inc.Have != "2.0.0" || inc.Required != "customers" {
		t.Fatalf("want ErrIncompatibleVersion, got %v", err)
	}
	// optional present but out of range never blocks
	if err := CheckInstallable([]InstalledAddon{ia(base), ia(mf("mercadopago", "1.0.0"))}, cand); err != nil {
		t.Fatalf("optional out of range must not block: %v", err)
	}
	// pre-release base satisfies a >= range
	if err := CheckInstallable([]InstalledAddon{ia(mf("customers", "1.3.0-rc.1"))}, cand); err != nil {
		t.Fatalf("prerelease: %v", err)
	}
	// several violations are all reported
	multi := mf("a", "1.0.0", req("b", ""), req("c", ""))
	err = CheckInstallable(nil, multi)
	if err == nil || !strings.Contains(err.Error(), `"b"`) || !strings.Contains(err.Error(), `"c"`) {
		t.Fatalf("want both missing, got %v", err)
	}
	// reserved keys are never peers
	if err := CheckInstallable(nil, mf("a", "1", req("kernel", ">=3"))); err != nil {
		t.Fatalf("reserved key treated as peer: %v", err)
	}
	// provides covers a requirement
	full := manifest.Manifest{Key: "hr", Version: "3.0.0", Provides: []string{"hr_lite"}}
	if err := CheckInstallable([]InstalledAddon{ia(full)}, mf("x", "1", req("hr_lite", ">=1.0.0"))); err != nil {
		t.Fatalf("provides: %v", err)
	}
}

func TestResolveOrder_Diamond(t *testing.T) {
	ms := []manifest.Manifest{
		mf("top", "1.0.0", req("left", ""), req("right", "")),
		mf("left", "1.0.0", req("base", "")),
		mf("right", "1.0.0", req("base", "")),
		mf("base", "1.0.0"),
	}
	order, err := ResolveOrder(ms)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"base", "left", "right", "top"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
}

func TestResolveOrder_CycleHasPath(t *testing.T) {
	ms := []manifest.Manifest{
		mf("a", "1", req("b", "")), mf("b", "1", req("c", "")), mf("c", "1", req("a", "")), mf("z", "1"),
	}
	_, err := ResolveOrder(ms)
	var cyc ErrDependencyCycle
	if !errors.As(err, &cyc) {
		t.Fatalf("want cycle, got %v", err)
	}
	if got := strings.Join(cyc.Path, ">"); got != "a>b>c>a" {
		t.Fatalf("path = %s", got)
	}
}

func TestResolveOrder_OptionalNeverCycles(t *testing.T) {
	// a hard-requires b; b optionally wants a: the soft edge is dropped.
	ms := []manifest.Manifest{mf("a", "1", req("b", "")), mf("b", "1", opt("a", ""))}
	order, err := ResolveOrder(ms)
	if err != nil || !reflect.DeepEqual(order, []string{"b", "a"}) {
		t.Fatalf("order=%v err=%v", order, err)
	}
	// optional edge to a member orders it first when no cycle
	order, _ = ResolveOrder([]manifest.Manifest{mf("a", "1", opt("z", "")), mf("z", "1")})
	if !reflect.DeepEqual(order, []string{"z", "a"}) {
		t.Fatalf("optional order = %v", order)
	}
	// requirement outside the set is ignored by the order
	order, err = ResolveOrder([]manifest.Manifest{mf("a", "1", req("ghost", ""))})
	if err != nil || len(order) != 1 {
		t.Fatalf("ghost: %v %v", order, err)
	}
}

func TestResolveInstall_PresetBatch(t *testing.T) {
	batch := []manifest.Manifest{
		mf("workshop", "1.0.0", req("customers", ">=1.0.0")),
		mf("customers", "1.1.0", req("kernelish_base", "")),
	}
	_, err := ResolveInstall(nil, batch)
	var miss ErrMissingDependency
	if !errors.As(err, &miss) || miss.Required != "kernelish_base" {
		t.Fatalf("want missing kernelish_base, got %v", err)
	}
	order, err := ResolveInstall([]InstalledAddon{{Key: "kernelish_base", Version: "1.0.0"}}, batch)
	if err != nil || !reflect.DeepEqual(order, []string{"customers", "workshop"}) {
		t.Fatalf("order=%v err=%v", order, err)
	}
}

func TestUninstallDependentsAndCascade(t *testing.T) {
	inst := []InstalledAddon{
		ia(mf("base", "1.0.0")),
		ia(mf("mid", "1.0.0", req("base", ""))),
		ia(mf("leaf", "1.0.0", req("mid", ""))),
		ia(mf("soft", "1.0.0", opt("base", ""))),
		ia(mf("other", "1.0.0")),
	}
	err := CheckUninstallable(inst, "base")
	var hd ErrHasDependents
	if !errors.As(err, &hd) || !reflect.DeepEqual(hd.Dependents, []string{"mid"}) {
		t.Fatalf("want dependents [mid], got %v", err)
	}
	if err := CheckUninstallable(inst, "leaf"); err != nil {
		t.Fatalf("leaf: %v", err)
	}
	if got := UninstallOrder(inst, "base"); !reflect.DeepEqual(got, []string{"leaf", "mid", "base"}) {
		t.Fatalf("cascade order = %v", got)
	}
}

func TestCheckUpgradable_Matrix(t *testing.T) {
	inst := []InstalledAddon{
		ia(mf("base", "1.5.0")),
		ia(mf("dep", "1.0.0", req("base", ">=1.0.0 <2.0.0"))),
		ia(mf("soft", "1.0.0", opt("base", "<1.2.0"))),
	}
	if err := CheckUpgradable(inst, mf("base", "1.9.0")); err != nil {
		t.Fatalf("in range: %v", err)
	}
	err := CheckUpgradable(inst, mf("base", "2.0.0"))
	var inc ErrIncompatibleVersion
	if !errors.As(err, &inc) || inc.Addon != "dep" || inc.Required != "base" || inc.Have != "2.0.0" {
		t.Fatalf("want dep left out of range, got %v", err)
	}
}

// --- Installer wiring -----------------------------------------------------

func requiresInstaller(t *testing.T, mode RequiresMode) (*Installer, uuid.UUID) {
	t.Helper()
	db := upgradeTestDB(t)
	i := &Installer{
		DB: db, KernelVersion: "2.0.0", Lifecycles: lifecycle.NewRegistry(),
		Interceptors: lifecycle.NewInterceptorRegistry(), AllowUnsigned: true,
		schemaApplier: &recordingApplier{}, Broadcaster: NoopBroadcaster{},
		EnforceAddonRequires: mode,
	}
	return i, uuid.New()
}

func seed(t *testing.T, i *Installer, org uuid.UUID, m manifest.Manifest) {
	t.Helper()
	seedInstallation(t, i.DB, org, m.Key, m.Version, nil)
	i.Lifecycles.Register(m.Key, &lifecycle.ManifestOnly{Data: m})
}

func TestInstaller_UpgradeMatrix_Modes(t *testing.T) {
	for _, tc := range []struct {
		mode    RequiresMode
		blocked bool
	}{{RequiresEnforce, true}, {RequiresWarn, false}, {"", false}, {RequiresOff, false}} {
		i, org := requiresInstaller(t, tc.mode)
		seed(t, i, org, mf("base", "1.0.0"))
		seed(t, i, org, mf("dep", "1.0.0", req("base", "<2.0.0")))
		b := makeBundle("base", "2.0.0", nil)
		_, err := i.Upgrade(context.Background(), org, b)
		var inc ErrIncompatibleVersion
		if tc.blocked != errors.As(err, &inc) {
			t.Fatalf("mode %q: blocked=%v err=%v", tc.mode, tc.blocked, err)
		}
		if !tc.blocked && err != nil {
			t.Fatalf("mode %q: unexpected %v", tc.mode, err)
		}
	}
}

func TestInstaller_UpgradeOwnRequiresMissing(t *testing.T) {
	i, org := requiresInstaller(t, RequiresEnforce)
	seed(t, i, org, mf("workshop", "1.0.0"))
	b := makeBundle("workshop", "1.1.0", nil)
	b.Manifest.AddonRequires = []manifest.AddonRequirement{req("customers", ">=1.0.0")}
	_, err := i.Upgrade(context.Background(), org, b)
	var miss ErrMissingDependency
	if !errors.As(err, &miss) {
		t.Fatalf("want ErrMissingDependency, got %v", err)
	}
}

func TestInstaller_UninstallBlocksAndCascades(t *testing.T) {
	i, org := requiresInstaller(t, RequiresEnforce)
	seed(t, i, org, mf("base", "1.0.0"))
	seed(t, i, org, mf("dep", "1.0.0", req("base", "")))

	err := i.Uninstall(org, "base", false)
	var hd ErrHasDependents
	if !errors.As(err, &hd) {
		t.Fatalf("want ErrHasDependents, got %v", err)
	}
	if ok, _ := i.IsInstalled(org, "base"); !ok {
		t.Fatal("base must stay installed after a blocked uninstall")
	}
	if err := i.UninstallWithOptions(org, "base", UninstallOptions{}); !errors.As(err, &hd) {
		t.Fatalf("default options must not cascade, got %v", err)
	}
	if err := i.UninstallWithOptions(org, "base", UninstallOptions{Cascade: true}); err != nil {
		t.Fatalf("cascade: %v", err)
	}
	for _, k := range []string{"base", "dep"} {
		if ok, _ := i.IsInstalled(org, k); ok {
			t.Fatalf("%s still installed after cascade", k)
		}
	}
}

func TestInstaller_UninstallWarnModeProceeds(t *testing.T) {
	i, org := requiresInstaller(t, "")
	seed(t, i, org, mf("base", "1.0.0"))
	seed(t, i, org, mf("dep", "1.0.0", req("base", "")))
	if err := i.Uninstall(org, "base", false); err != nil {
		t.Fatalf("warn mode must proceed: %v", err)
	}
}

func TestParseRequiresMode(t *testing.T) {
	for in, want := range map[string]RequiresMode{"": RequiresWarn, "OFF": RequiresOff, "enforce": RequiresEnforce, "x": RequiresWarn} {
		if got := ParseRequiresMode(in); got != want {
			t.Fatalf("%q -> %q", in, got)
		}
	}
}
