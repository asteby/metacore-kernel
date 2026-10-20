package cachecoherence

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
)

func TestInvalidatorsRunInOrderAndSeeNewEpoch(t *testing.T) {
	r := NewRegistry()
	var got []string
	var seen Epoch
	r.Register("nav", OrderNavigation, func(_ context.Context, ev Event) error { got = append(got, "nav"); return nil })
	r.Register("db", OrderDatabase, func(_ context.Context, ev Event) error { got = append(got, "db"); seen = ev.Epoch; return nil })
	r.Register("perm", OrderPermission, func(_ context.Context, ev Event) error { got = append(got, "perm"); return nil })
	r.Register("a-same-order", OrderPermission, func(_ context.Context, ev Event) error { got = append(got, "a-same-order"); return nil })

	before := r.Epoch("org1", "crm")
	ev := r.Bump("addon.upgrade", WithOrg("org1"), WithAddon("crm"))
	want := []string{"db", "a-same-order", "perm", "nav"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	if seen.Org != before.Org+1 || seen.Addon != before.Addon+1 {
		t.Fatalf("invalidator saw %+v, before %+v", seen, before)
	}
	if after := r.Epoch("org1", "crm"); after != ev.Epoch || after == before {
		t.Fatalf("epoch after = %+v event = %+v before = %+v", after, ev.Epoch, before)
	}
}

// The epoch is published only after every invalidator ran: a reader that
// observes the new token is guaranteed clean caches.
func TestEpochPublishedAfterInvalidators(t *testing.T) {
	r := NewRegistry()
	before := r.Epoch("o", "a")
	var during Epoch
	r.Register("x", 1, func(context.Context, Event) error { during = r.Epoch("o", "a"); return nil })
	r.Bump("t", WithOrg("o"), WithAddon("a"))
	if during != before {
		t.Fatalf("epoch visible during invalidation: %+v vs %+v", during, before)
	}
	if r.Epoch("o", "a") == before {
		t.Fatal("epoch did not move")
	}
}

func TestScopes(t *testing.T) {
	r := NewRegistry()
	a1 := r.Epoch("A", "crm")
	b1 := r.Epoch("B", "crm")
	o1 := r.Epoch("A", "erp")
	r.Bump("x", WithOrg("A"), WithAddon("crm"))
	if r.Epoch("A", "crm") == a1 {
		t.Fatal("A/crm unchanged")
	}
	if r.Epoch("B", "erp") != r.Epoch("B", "erp") {
		t.Fatal("unstable")
	}
	// Org B watching crm sees the addon move; A watching erp sees its org move.
	if r.Epoch("B", "crm") == b1 {
		t.Fatal("addon bump must reach other orgs' view of that addon")
	}
	if r.Epoch("A", "erp") == o1 {
		t.Fatal("org bump must reach that org's view of other addons")
	}
	bErp := r.Epoch("B", "erp")
	r.Bump("only-A", WithOrg("A"))
	if r.Epoch("B", "erp") != bErp {
		t.Fatal("org-scoped bump leaked into another org")
	}
	g := r.Epoch("", "")
	r.Bump("global")
	if r.Epoch("", "") == g || r.Epoch("B", "erp") == bErp {
		t.Fatal("global bump must move everything")
	}
}

func TestFailingAndPanickingInvalidatorsDoNotStopTheRest(t *testing.T) {
	r := NewRegistry()
	ran := map[string]bool{}
	r.Register("boom", 1, func(context.Context, Event) error { panic("kaboom") })
	r.Register("err", 2, func(context.Context, Event) error { return errors.New("nope") })
	r.Register("ok", 3, func(context.Context, Event) error { ran["ok"] = true; return nil })
	_, err := r.BumpContext(context.Background(), "t")
	if err == nil || !ran["ok"] {
		t.Fatalf("err=%v ran=%v", err, ran)
	}
	st := r.Status()
	if st.AllUpToDate {
		t.Fatal("status claims everything is up to date")
	}
	byName := map[string]InvalidatorStatus{}
	for _, i := range st.Invalidated {
		byName[i.Name] = i
	}
	if byName["boom"].UpToDate || byName["err"].UpToDate || !byName["ok"].UpToDate {
		t.Fatalf("statuses: %+v", byName)
	}
	if byName["err"].LastError != "nope" {
		t.Fatalf("last error: %q", byName["err"].LastError)
	}
	// A later healthy run clears the flag.
	r.Register("boom", 1, func(context.Context, Event) error { return nil })
	r.Register("err", 2, func(context.Context, Event) error { return nil })
	r.Bump("again")
	if !r.Status().AllUpToDate {
		t.Fatalf("not up to date after healthy bump: %+v", r.Status().Invalidated)
	}
}

func TestRegisterReplacesAndUnregisterIsGuarded(t *testing.T) {
	r := NewRegistry()
	var calls []string
	un1 := r.Register("svc", 1, func(context.Context, Event) error { calls = append(calls, "old"); return nil })
	un2 := r.Register("svc", 1, func(context.Context, Event) error { calls = append(calls, "new"); return nil })
	un1() // the old registration must not remove the replacement
	r.Bump("t")
	if !reflect.DeepEqual(calls, []string{"new"}) {
		t.Fatalf("calls = %v", calls)
	}
	un2()
	r.Bump("t")
	if len(calls) != 1 {
		t.Fatalf("unregistered layer still called: %v", calls)
	}
}

func TestWatcher(t *testing.T) {
	r := NewRegistry()
	w := r.NewWatcher("o", "a")
	if w.Stale() {
		t.Fatal("fresh watcher is stale")
	}
	r.Bump("x", WithAddon("other"))
	if w.Stale() {
		t.Fatal("unrelated addon bump made the watcher stale")
	}
	r.Bump("x", WithOrg("o"))
	if !w.Stale() {
		t.Fatal("org bump not noticed")
	}
	if !w.Refresh() || w.Stale() || w.Refresh() {
		t.Fatal("refresh semantics")
	}
}

func TestTokenRoundTrip(t *testing.T) {
	e := Epoch{Global: 1760000000000, Org: 3, Addon: 12}
	got, ok := ParseToken(e.Token())
	if !ok || got != e {
		t.Fatalf("%v %v", got, ok)
	}
	for _, bad := range []string{"", "a.b", "1.2.3.4", "zz!.1.1"} {
		if _, ok := ParseToken(bad); ok {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestSubscribeAndRemote(t *testing.T) {
	a, b := NewRegistry(), NewRegistry()
	var ran bool
	b.Register("x", 1, func(context.Context, Event) error { ran = true; return nil })
	var forwarded []Event
	a.Subscribe(func(ev Event) { forwarded = append(forwarded, ev) })
	a.Bump("upgrade", WithOrg("o"), WithAddon("a"))
	if len(forwarded) != 1 {
		t.Fatal("subscriber not called")
	}
	ev, err := b.ApplyRemote(context.Background(), forwarded[0])
	if err != nil || !ran || !ev.Remote {
		t.Fatalf("remote apply: %v %v %+v", err, ran, ev)
	}
	if got := b.Epoch("o", "a"); got.Org < forwarded[0].Epoch.Org || got.Addon < forwarded[0].Epoch.Addon {
		t.Fatalf("remote epoch behind: %+v vs %+v", got, forwarded[0].Epoch)
	}
}

func TestConcurrentBumps(t *testing.T) {
	r := NewRegistry()
	var mu sync.Mutex
	n := 0
	r.Register("c", 1, func(context.Context, Event) error { mu.Lock(); n++; mu.Unlock(); return nil })
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); r.Bump("x", WithOrg("o")) }()
	}
	wg.Wait()
	if n != 50 || r.Epoch("o", "").Org != 50 || r.Status().Seq != 50 {
		t.Fatalf("n=%d org=%d seq=%d", n, r.Epoch("o", "").Org, r.Status().Seq)
	}
}

func TestStartEpochDiffersAcrossRegistries(t *testing.T) {
	// The global base derives from the start time (ms): a restarted process
	// never hands out a token an older client could take for "unchanged".
	if NewRegistry().Epoch("", "").Global == 0 {
		t.Fatal("global epoch must start non-zero")
	}
}

func TestViewEpochMovesOnAnyBumpThatCanChangeTheOrgsView(t *testing.T) {
	r := NewRegistry()
	v0 := r.ViewEpoch("A")
	r.Bump("addon-only", WithAddon("crm"))
	v1 := r.ViewEpoch("A")
	if v1 == v0 {
		t.Fatal("an addon bump must move every organization's view")
	}
	r.Bump("other-org", WithOrg("B"))
	if r.ViewEpoch("A") != v1 {
		t.Fatal("another organization's bump moved this organization's view")
	}
	r.Bump("mine", WithOrg("A"))
	v2 := r.ViewEpoch("A")
	if v2 == v1 {
		t.Fatal("own bump did not move the view")
	}
	r.Bump("global")
	if r.ViewEpoch("A") == v2 {
		t.Fatal("global bump did not move the view")
	}
}
