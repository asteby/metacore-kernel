package capability

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func cleanDeclared(t *testing.T, owners ...string) {
	t.Helper()
	t.Cleanup(func() {
		for _, o := range owners {
			UnregisterDeclared(o)
		}
	})
}

func TestRegisterDeclared_KnownKeys(t *testing.T) {
	cleanDeclared(t, "acme_core")
	err := RegisterDeclared("acme_core", Declared{Key: "acme.document", Version: "1.2.0", Operations: []DeclaredOperation{
		{Key: "issue", Kind: KindAction}, {Key: "status", Kind: KindQuery},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for key, wantContract := range map[string]string{"acme.document": "acme.document", "acme.document.issue": "acme.document", "acme.document.status": "acme.document"} {
		info, ok := Known(key)
		if !ok || info.Contract != wantContract || info.Version != "1.2.0" || info.Source != "addon:acme_core" {
			t.Errorf("Known(%q) = %+v, %v", key, info, ok)
		}
	}
	for _, key := range []string{"acme.document.ghost", "acme.other", "acme"} {
		if _, ok := Known(key); ok {
			t.Errorf("Known(%q) must be false", key)
		}
	}
	if info, ok := Known(CommercePaymentsSession); !ok || info.Source != "kernel" || info.Version != DefaultVersion {
		t.Errorf("a kernel contract is known with the default version: %+v %v", info, ok)
	}
	if got := DeclaredKeys(); len(got) != 3 {
		t.Errorf("DeclaredKeys = %v", got)
	}
}

func TestRegisterDeclared_Rejections(t *testing.T) {
	cleanDeclared(t, "acme_core", "other_core")
	good := Declared{Key: "acme.document", Version: "1.0.0", Operations: []DeclaredOperation{{Key: "issue"}}}
	if err := RegisterDeclared("acme_core", good); err != nil {
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
		"repeated operation":  {"acme_core", []Declared{{Key: "acme.two", Version: "1.0.0", Operations: []DeclaredOperation{{Key: "a"}, {Key: "a"}}}}, "repeats operation"},
		"bad operation":       {"acme_core", []Declared{{Key: "acme.two", Version: "1.0.0", Operations: []DeclaredOperation{{Key: "Bad-Op"}}}}, "malformed operation"},
		"kernel collision":    {"acme_core", []Declared{{Key: CommercePaymentsSession, Version: "1.0.0"}}, "collides with a kernel contract"},
		"kernel op collision": {"acme_core", []Declared{{Key: "commerce.payments", Version: "1.0.0", Operations: []DeclaredOperation{{Key: "session"}}}}, "collides with a kernel contract"},
		"owned by another":    {"other_core", []Declared{good}, "already owned by addon"},
		"twice in one call":   {"acme_core", []Declared{{Key: "acme.x", Version: "1.0.0"}, {Key: "acme.x", Version: "1.0.0"}}, "declared twice"},
	}
	for name, c := range cases {
		err := RegisterDeclared(c.owner, c.d...)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want %q, got %v", name, c.want, err)
		}
	}
	// All-or-nothing: the failed batches left the registry as it was.
	if got := DeclaredKeys(); len(got) != 2 {
		t.Errorf("failed registrations must not leave residue: %v", got)
	}
}

func TestRegisterDeclared_SameOwnerReplacesOnUpgrade(t *testing.T) {
	cleanDeclared(t, "acme_core")
	_ = RegisterDeclared("acme_core", Declared{Key: "acme.document", Version: "1.0.0", Operations: []DeclaredOperation{{Key: "issue"}}})
	if err := RegisterDeclared("acme_core", Declared{Key: "acme.document", Version: "1.1.0", Operations: []DeclaredOperation{{Key: "issue"}, {Key: "void"}}}); err != nil {
		t.Fatal(err)
	}
	if info, ok := Known("acme.document.void"); !ok || info.Version != "1.1.0" {
		t.Fatalf("upgrade must replace: %+v %v", info, ok)
	}
	// A contract dropped by the upgrade disappears with it.
	if err := RegisterDeclared("acme_core", Declared{Key: "acme.other", Version: "1.0.0"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := Known("acme.document"); ok {
		t.Fatal("a contract no longer declared by its owner must be forgotten")
	}
	UnregisterDeclared("acme_core")
	if _, ok := Known("acme.other"); ok {
		t.Fatal("UnregisterDeclared must forget the owner's contracts")
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

// Registering, unregistering and looking up from many goroutines is race-free
// (run with -race) and never leaves a half-written declaration visible: every
// contract a reader sees (one LookupDeclared call = one atomic snapshot) carries
// all of its operations.
func TestDeclaredRegistry_Concurrent(t *testing.T) {
	cleanDeclared(t, "w0", "w1", "w2", "w3")
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for w := 0; w < 4; w++ {
		owner := fmt.Sprintf("w%d", w)
		key := fmt.Sprintf("acme.c%d", w)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 300; n++ {
				ops := []DeclaredOperation{{Key: "a"}, {Key: "b"}}
				if err := RegisterDeclared(owner, Declared{Key: key, Version: "1.0.0", Operations: ops}); err != nil {
					t.Errorf("register: %v", err)
					return
				}
				if n%3 == 0 {
					UnregisterDeclared(owner)
				}
			}
		}()
	}
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for w := 0; w < 4; w++ {
					key := fmt.Sprintf("acme.c%d", w)
					_, _ = Known(key + ".a")
					if d, _, ok := LookupDeclared(key); ok && len(d.Operations) != 2 {
						t.Errorf("half-written declaration: %+v", d)
						return
					}
					_ = DeclaredKeys()
				}
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	// writers finish on their own; stop the readers after them.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	time.Sleep(50 * time.Millisecond)
	close(stop)
	<-done
}
