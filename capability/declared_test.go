package capability

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

func doc(key, version string, ops ...string) Declared {
	d := Declared{Key: key, Version: version}
	for _, o := range ops {
		d.Operations = append(d.Operations, DeclaredOperation{Key: o, Kind: KindAction})
	}
	return d
}

func TestRegistry_KnownKeys(t *testing.T) {
	r := NewRegistry()
	if err := r.Register("acme_core", Declared{Key: "acme.document", Version: "1.2.0", Operations: []DeclaredOperation{
		{Key: "issue", Kind: KindAction}, {Key: "status", Kind: KindQuery},
	}}); err != nil {
		t.Fatal(err)
	}
	for key, wantContract := range map[string]string{"acme.document": "acme.document", "acme.document.issue": "acme.document", "acme.document.status": "acme.document"} {
		info, ok := r.Known(key)
		if !ok || info.Contract != wantContract || info.Version != "1.2.0" || info.Source != "addon:acme_core" {
			t.Errorf("Known(%q) = %+v, %v", key, info, ok)
		}
	}
	for _, key := range []string{"acme.document.ghost", "acme.other", "acme"} {
		if _, ok := r.Known(key); ok {
			t.Errorf("Known(%q) must be false", key)
		}
	}
	if info, ok := r.Known(CommercePaymentsSession); !ok || info.Source != "kernel" || info.Version != DefaultVersion {
		t.Errorf("a kernel contract is known with the default version: %+v %v", info, ok)
	}
	if got := r.Keys(); len(got) != 3 {
		t.Errorf("Keys = %v", got)
	}
	if v, ok := r.ContractVersion("acme.document"); !ok || v != "1.2.0" {
		t.Errorf("ContractVersion = %q %v", v, ok)
	}
	if !r.KnownCapability("acme.document.issue") || r.KnownCapability("acme.nope") {
		t.Error("KnownCapability must mirror Known")
	}
}

// There is no process-wide declared state: two registries never see each
// other's contracts, versions or owners (one tenant's upgrade or private addon
// must not change the answer for another).
func TestRegistry_ScopesAreIsolated(t *testing.T) {
	orgA, orgB := NewRegistry(), NewRegistry()
	if err := orgA.Register("acme_core", doc("acme.document", "2.0.0", "issue")); err != nil {
		t.Fatal(err)
	}
	if err := orgB.Register("acme_core", doc("acme.document", "1.4.0", "issue")); err != nil {
		t.Fatal(err)
	}
	if v, _ := orgB.ContractVersion("acme.document"); v != "1.4.0" {
		t.Fatalf("org B must keep its own version, got %q", v)
	}
	// An addon of org A claiming a key owned by another addon does not touch org B,
	// and a squatter in one scope cannot take the key in another.
	if err := orgA.Register("private_addon", doc("acme.document", "1.0.0")); err == nil {
		t.Fatal("within a scope, the first owner keeps the key")
	}
	if err := orgB.Register("legit_owner", doc("acme.other", "1.0.0")); err != nil {
		t.Fatal(err)
	}
	if _, ok := orgA.Known("acme.other"); ok {
		t.Fatal("org A must not know a contract only org B declared")
	}
	var none *Registry
	if _, ok := none.Known("acme.document"); ok {
		t.Fatal("a nil registry is empty")
	}
	if _, ok := none.Known(CommercePaymentsSession); !ok {
		t.Fatal("a nil registry still knows the kernel's contracts")
	}
	none.Unregister("x")
}

func TestRegistry_Rejections(t *testing.T) {
	r := NewRegistry()
	good := doc("acme.document", "1.0.0", "issue")
	if err := r.Register("acme_core", good); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		owner string
		d     []Declared
		want  string
	}{
		"no owner":            {"", []Declared{good}, "owner"},
		"bad key":             {"acme_core", []Declared{{Key: "NoDot", Version: "1.0.0"}}, "invalid declared contract key"},
		"bad version":         {"acme_core", []Declared{{Key: "acme.two", Version: "one"}}, "invalid version"},
		"repeated operation":  {"acme_core", []Declared{doc("acme.two", "1.0.0", "a", "a")}, "repeats operation"},
		"bad operation":       {"acme_core", []Declared{doc("acme.two", "1.0.0", "Bad-Op")}, "malformed operation"},
		"kernel collision":    {"acme_core", []Declared{doc(CommercePaymentsSession, "1.0.0")}, "collides with a kernel contract"},
		"kernel op collision": {"acme_core", []Declared{doc("commerce.payments", "1.0.0", "session")}, "collides with a kernel contract"},
		"owned by another":    {"other_core", []Declared{good}, "already owned by addon"},
		"twice in one call":   {"acme_core", []Declared{doc("acme.x", "1.0.0"), doc("acme.x", "1.0.0")}, "declared twice"},
	}
	for name, c := range cases {
		err := r.Register(c.owner, c.d...)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want %q, got %v", name, c.want, err)
		}
	}
	// All-or-nothing: the failed batches left the registry as it was.
	if got := r.Keys(); len(got) != 2 {
		t.Errorf("failed registrations must not leave residue: %v", got)
	}
}

func TestRegistry_SameOwnerReplacesOnUpgrade(t *testing.T) {
	r := NewRegistry()
	_ = r.Register("acme_core", doc("acme.document", "1.0.0", "issue"))
	if err := r.Register("acme_core", doc("acme.document", "1.1.0", "issue", "void")); err != nil {
		t.Fatal(err)
	}
	if info, ok := r.Known("acme.document.void"); !ok || info.Version != "1.1.0" {
		t.Fatalf("upgrade must replace: %+v %v", info, ok)
	}
	if err := r.Register("acme_core", doc("acme.other", "1.0.0")); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Known("acme.document"); ok {
		t.Fatal("a contract no longer declared by its owner must be forgotten")
	}
	r.Unregister("acme_core")
	if _, ok := r.Known("acme.other"); ok {
		t.Fatal("Unregister must forget the owner's contracts")
	}
}

// Rehydrate: what an install registered is gone after a restart (a fresh
// Registry); Rehydrate with the persisted installs restores the same keys and
// versions, replaces whatever was there, keeps install order for conflicts and
// never aborts on a failing owner.
func TestRegistry_RehydrateRestoresInstallState(t *testing.T) {
	installed := []InstalledContracts{
		{Owner: "acme_core", Contracts: []Declared{doc("acme.document", "1.3.0", "issue", "status"), doc("acme.ledger", "2.0.0", "post")}},
		{Owner: "plain"},
		{Owner: "rival_core", Contracts: []Declared{doc("acme.document", "9.0.0"), doc("rival.thing", "1.0.0")}},
		{Owner: "late_core", Contracts: []Declared{doc("late.thing", "0.1.0", "go")}},
	}
	before := NewRegistry()
	for _, in := range installed { // the live process: installs registering one by one
		if len(in.Contracts) > 0 {
			_ = before.Register(in.Owner, in.Contracts...)
		}
	}
	restarted := NewRegistry() // empty after a restart
	if _, ok := restarted.Known("acme.document.issue"); ok {
		t.Fatal("a fresh registry knows nothing declared")
	}
	rep := restarted.Rehydrate(installed)
	for _, key := range []string{"acme.document", "acme.document.issue", "acme.ledger.post", "late.thing.go"} {
		a, aok := before.Known(key)
		b, bok := restarted.Known(key)
		if !aok || !bok || a != b {
			t.Errorf("key %q differs after rehydrate: before=%+v/%v after=%+v/%v", key, a, aok, b, bok)
		}
	}
	if v, _ := restarted.ContractVersion("acme.document"); v != "1.3.0" {
		t.Errorf("first owner keeps the contract: version %q", v)
	}
	if _, ok := rep.Failed["rival_core"]; !ok || len(rep.Failed) != 1 {
		t.Errorf("only the conflicting owner fails: %+v", rep.Failed)
	}
	if len(rep.Registered) != 2 || rep.Registered[0] != "acme_core" || rep.Registered[1] != "late_core" {
		t.Errorf("registered = %v", rep.Registered)
	}
	if _, ok := restarted.Known("rival.thing"); ok {
		t.Error("a failing owner registers nothing (all-or-nothing)")
	}
	// A re-run is a clean slate, not an accumulation.
	rep2 := restarted.Rehydrate([]InstalledContracts{{Owner: "late_core", Contracts: []Declared{doc("late.thing", "0.2.0", "go")}}})
	if _, ok := restarted.Known("acme.document"); ok || len(rep2.Failed) != 0 {
		t.Errorf("Rehydrate must replace the content: %+v", rep2)
	}
	if v, _ := restarted.ContractVersion("late.thing"); v != "0.2.0" {
		t.Errorf("rehydrated version = %q", v)
	}
}

func TestContract_FamilyAndSemver(t *testing.T) {
	c, _ := Lookup(CommercePaymentsSession)
	if c.Family() != "commerce" || c.Semver() != DefaultVersion {
		t.Errorf("family=%q semver=%q", c.Family(), c.Semver())
	}
	if (Contract{Key: "single"}).Family() != "single" {
		t.Error("a key without dot is its own family")
	}
	if (Contract{Key: "a.b", Version: "2.1.0"}).Semver() != "2.1.0" {
		t.Error("explicit version wins")
	}
}

func TestSatisfies(t *testing.T) {
	cases := []struct {
		v, r string
		want bool
		err  bool
	}{
		{"1.2.0", "^1.0", true, false},
		{"2.0.0", "^1.0", false, false},
		{"1.9.3", ">=1.0.0 <2.0.0", true, false},
		// A prerelease does not satisfy a plain range (semver): ship betas under
		// a range that names a prerelease, or not at all.
		{"1.1.0-beta", "^1.0", false, false},
		{"1.1.0-beta", ">=1.1.0-0", true, false},
		{"1.0.0", "garbage", false, true},
		{"nope", "^1", false, true},
	}
	for _, c := range cases {
		got, err := Satisfies(c.v, c.r)
		if (err != nil) != c.err || got != c.want {
			t.Errorf("Satisfies(%q, %q) = %v, %v", c.v, c.r, got, err)
		}
	}
}

// Writers, readers and Rehydrate share ONE registry from many goroutines (run
// with -race). Every contract a reader sees through one Lookup call is a whole
// declaration: all of its operations.
func TestRegistry_ConcurrentRace(t *testing.T) {
	r := NewRegistry()
	const writers, rounds = 4, 100
	var wg sync.WaitGroup
	start := make(chan struct{})
	var writersDone sync.WaitGroup
	for w := 0; w < writers; w++ {
		owner := fmt.Sprintf("w%d", w)
		key := fmt.Sprintf("acme.c%d", w)
		wg.Add(1)
		writersDone.Add(1)
		go func() {
			defer wg.Done()
			defer writersDone.Done()
			<-start
			for n := 0; n < rounds; n++ {
				if err := r.Register(owner, doc(key, "1.0.0", "a", "b")); err != nil {
					t.Errorf("register: %v", err)
					return
				}
				switch n % 5 {
				case 0:
					r.Unregister(owner)
				case 3:
					r.Rehydrate([]InstalledContracts{{Owner: owner, Contracts: []Declared{doc(key, "1.0.1", "a", "b")}}})
				}
			}
		}()
	}
	stop := make(chan struct{})
	var readers sync.WaitGroup
	for k := 0; k < 6; k++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			<-start
			for {
				select {
				case <-stop:
					return
				default:
				}
				for w := 0; w < writers; w++ {
					key := fmt.Sprintf("acme.c%d", w)
					if d, _, ok := r.Lookup(key); ok && len(d.Operations) != 2 {
						t.Errorf("half-written declaration: %+v", d)
						return
					}
					_, _ = r.Known(key + ".a")
					_, _ = r.ContractVersion(key)
					_ = r.Keys()
				}
			}
		}()
	}
	close(start)
	writersDone.Wait()
	close(stop)
	readers.Wait()
	wg.Wait()
}
