package setupflow

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

func ok(msg string) []byte {
	return []byte(fmt.Sprintf(`{"success":true,"data":{"status":"ok","message_key":%q}}`, msg))
}
func pending(secs int) []byte {
	return []byte(fmt.Sprintf(`{"success":true,"data":{"status":"pending","message_key":"x.wait","retry_after_s":%d}}`, secs))
}

func req(user string) Request {
	return Request{
		AddonKey: "mail", FlowKey: "main", StepKey: "dns",
		Handler:   v3.SetupHandler{Type: "wasm", Function: "VerifyDns"},
		Principal: Principal{UserID: user, OrgID: "org1"},
	}
}

// fakeSleep records delays and advances a fake clock.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	c.sleeps = append(c.sleeps, d)
	c.now = c.now.Add(d)
	c.mu.Unlock()
	return ctx.Err()
}

func newExec(inv Invoker) (*Executor, *fakeClock) {
	c := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	return &Executor{Invoker: inv, Sleep: c.Sleep, Now: c.Now}, c
}

func TestRun_PassesPrincipalAndParsesResult(t *testing.T) {
	var got Request
	e, _ := newExec(InvokerFunc(func(_ context.Context, r Request) ([]byte, error) { got = r; return ok("x.ok"), nil }))
	res, err := e.Run(context.Background(), v3.SetupCheck{}, req("u1"))
	if err != nil || res.Status != StatusOK {
		t.Fatalf("%+v %v", res, err)
	}
	if got.Principal.UserID != "u1" || got.Principal.OrgID != "org1" || got.Handler.Function != "VerifyDns" {
		t.Fatalf("invoker got %+v", got)
	}
}

func TestRun_RequiresPrincipalAndInvoker(t *testing.T) {
	e, _ := newExec(InvokerFunc(func(context.Context, Request) ([]byte, error) { return ok("a.b"), nil }))
	r := req("")
	if _, err := e.Run(context.Background(), v3.SetupCheck{}, r); !errors.Is(err, ErrNoPrincipal) {
		t.Fatalf("want ErrNoPrincipal, got %v", err)
	}
	if _, err := (&Executor{}).Run(context.Background(), v3.SetupCheck{}, req("u")); err == nil {
		t.Fatal("missing invoker must fail")
	}
}

func TestRun_Timeout(t *testing.T) {
	e, _ := newExec(InvokerFunc(func(ctx context.Context, _ Request) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}))
	start := time.Now()
	_, err := e.Run(context.Background(), v3.SetupCheck{Timeout: "50ms"}, req("u"))
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("want ErrTimeout, got %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("timeout not enforced")
	}
	// A handler that ignores its context and answers late is still a timeout.
	e2, _ := newExec(InvokerFunc(func(ctx context.Context, _ Request) ([]byte, error) {
		time.Sleep(120 * time.Millisecond)
		return ok("a.b"), nil
	}))
	if _, err := e2.Run(context.Background(), v3.SetupCheck{Timeout: "30ms"}, req("u")); !errors.Is(err, ErrTimeout) {
		t.Fatalf("late answer: want ErrTimeout, got %v", err)
	}
	if _, err := e.Run(context.Background(), v3.SetupCheck{Timeout: "1h"}, req("u")); err == nil {
		t.Fatal("timeout above the maximum must be rejected")
	}
}

func TestRun_RejectsInvalidResultAndHandlerFailure(t *testing.T) {
	e, _ := newExec(InvokerFunc(func(context.Context, Request) ([]byte, error) {
		return []byte(`{"success":true,"data":{"status":"nope","message_key":"a.b"}}`), nil
	}))
	if _, err := e.Run(context.Background(), v3.SetupCheck{}, req("u")); err == nil {
		t.Fatal("invalid status must fail")
	}
	e2, _ := newExec(InvokerFunc(func(context.Context, Request) ([]byte, error) { return []byte(`{"success":false,"error":"x"}`), nil }))
	if _, err := e2.Run(context.Background(), v3.SetupCheck{}, req("u")); !errors.Is(err, ErrHandlerFailed) {
		t.Fatalf("got %v", err)
	}
	e3, _ := newExec(InvokerFunc(func(context.Context, Request) ([]byte, error) { panic("boom") }))
	if _, err := e3.Run(context.Background(), v3.SetupCheck{}, req("u")); err == nil {
		t.Fatal("a panicking handler must surface as an error, not crash")
	}
}

func TestRun_DedupesConcurrentRunsPerUser(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	started := make(chan struct{}, 8)
	e, _ := newExec(InvokerFunc(func(_ context.Context, r Request) ([]byte, error) {
		calls.Add(1)
		started <- struct{}{}
		<-release
		return ok("x." + r.Principal.UserID), nil
	}))
	var wg sync.WaitGroup
	results := make([]CheckResult, 6)
	for i := 0; i < 5; i++ { // five callers, same user
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := e.Run(context.Background(), v3.SetupCheck{}, req("alice"))
			if err != nil {
				t.Error(err)
			}
			results[i] = r
		}(i)
	}
	wg.Add(1) // one caller, other user: must NOT share alice's run
	go func() {
		defer wg.Done()
		r, err := e.Run(context.Background(), v3.SetupCheck{}, req("bob"))
		if err != nil {
			t.Error(err)
		}
		results[5] = r
	}()
	<-started
	<-started
	time.Sleep(50 * time.Millisecond) // let the other callers attach
	close(release)
	wg.Wait()
	if n := calls.Load(); n != 2 {
		t.Fatalf("handler ran %d times, want 2 (one per user)", n)
	}
	for i := 0; i < 5; i++ {
		if results[i].MessageKey != "x.alice" {
			t.Errorf("result %d = %+v", i, results[i])
		}
	}
	if results[5].MessageKey != "x.bob" {
		t.Errorf("bob got %+v", results[5])
	}
}

func TestRun_NoStaleCacheAfterCompletion(t *testing.T) {
	var calls atomic.Int32
	e, _ := newExec(InvokerFunc(func(context.Context, Request) ([]byte, error) { calls.Add(1); return ok("a.b"), nil }))
	for i := 0; i < 3; i++ {
		if _, err := e.Run(context.Background(), v3.SetupCheck{}, req("alice")); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("sequential runs must each execute, got %d", calls.Load())
	}
}

func TestRun_CallerCancelDoesNotAbortSharedRun(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	e, _ := newExec(InvokerFunc(func(ctx context.Context, _ Request) ([]byte, error) {
		close(started)
		select {
		case <-release:
			return ok("a.b"), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}))
	ctx1, cancel1 := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { _, err := e.Run(ctx1, v3.SetupCheck{}, req("u")); errc <- err }()
	<-started
	resc := make(chan CheckResult, 1)
	go func() { r, _ := e.Run(context.Background(), v3.SetupCheck{}, req("u")); resc <- r }()
	time.Sleep(30 * time.Millisecond)
	cancel1()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("first caller: %v", err)
	}
	close(release)
	if r := <-resc; r.Status != StatusOK {
		t.Fatalf("second caller should still get the result, got %+v", r)
	}
}

func TestPollDelay(t *testing.T) {
	p := v3.SetupPoll{Every: "30s", MaxEvery: "1h", Factor: 2}
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 32 * time.Minute, time.Hour, time.Hour}
	for i, w := range want {
		got, err := PollDelay(p, i, 0)
		if err != nil || got != w {
			t.Errorf("attempt %d = %s, %v; want %s", i, got, err, w)
		}
	}
	// default factor 2, default max 1h
	if d, _ := PollDelay(v3.SetupPoll{Every: "10m"}, 3, 0); d != time.Hour {
		t.Errorf("default max: %s", d)
	}
	// max below every is lifted to every
	if d, _ := PollDelay(v3.SetupPoll{Every: "10m", MaxEvery: "1m"}, 0, 0); d != 10*time.Minute {
		t.Errorf("max<every: %s", d)
	}
	// hint replaces the schedule, clamped to [1s, max]
	if d, _ := PollDelay(p, 0, 5*time.Minute); d != 5*time.Minute {
		t.Errorf("hint: %s", d)
	}
	if d, _ := PollDelay(p, 0, 10*time.Hour); d != time.Hour {
		t.Errorf("hint over max: %s", d)
	}
	if d, _ := PollDelay(p, 0, 10*time.Millisecond); d != time.Second {
		t.Errorf("hint under min: %s", d)
	}
	// overflow stays capped
	if d, _ := PollDelay(p, 5000, 0); d != time.Hour {
		t.Errorf("overflow: %s", d)
	}
	for _, bad := range []v3.SetupPoll{{Every: ""}, {Every: "1ms"}, {Every: "30s", MaxEvery: "x"}, {Every: "30s", Factor: 0.5}} {
		if _, err := PollDelay(bad, 0, 0); err == nil {
			t.Errorf("%+v must be rejected", bad)
		}
	}
}

func TestPoll_BackoffUntilSettled(t *testing.T) {
	var n atomic.Int32
	e, clock := newExec(InvokerFunc(func(context.Context, Request) ([]byte, error) {
		if n.Add(1) < 4 {
			return pending(0), nil
		}
		return ok("x.done"), nil
	}))
	var seen []Status
	check := v3.SetupCheck{Poll: &v3.SetupPoll{Every: "30s", MaxEvery: "5m", Factor: 2}}
	res, err := e.Poll(context.Background(), check, req("u"), func(attempt int, r CheckResult) { seen = append(seen, r.Status) })
	if err != nil || res.Status != StatusOK {
		t.Fatalf("%+v %v", res, err)
	}
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute}
	if fmt.Sprint(clock.sleeps) != fmt.Sprint(want) {
		t.Fatalf("sleeps = %v want %v", clock.sleeps, want)
	}
	if fmt.Sprint(seen) != "[pending pending pending ok]" {
		t.Fatalf("onResult saw %v", seen)
	}
}

func TestPoll_RetryAfterHintAndAttemptNumber(t *testing.T) {
	var attempts []int
	var n atomic.Int32
	e, clock := newExec(InvokerFunc(func(_ context.Context, r Request) ([]byte, error) {
		attempts = append(attempts, r.Attempt)
		if n.Add(1) == 1 {
			return pending(90), nil
		}
		return ok("x.done"), nil
	}))
	_, err := e.Poll(context.Background(), v3.SetupCheck{Poll: &v3.SetupPoll{Every: "30s", MaxEvery: "1h"}}, req("u"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(clock.sleeps) != 1 || clock.sleeps[0] != 90*time.Second {
		t.Fatalf("hint not honoured: %v", clock.sleeps)
	}
	if fmt.Sprint(attempts) != "[0 1]" {
		t.Fatalf("attempts = %v", attempts)
	}
}

func TestPoll_StopsOnErrorBlockedWarn(t *testing.T) {
	for _, st := range []string{"error", "blocked", "warn"} {
		e, clock := newExec(InvokerFunc(func(context.Context, Request) ([]byte, error) {
			return []byte(fmt.Sprintf(`{"status":%q,"message_key":"x.y"}`, st)), nil
		}))
		res, err := e.Poll(context.Background(), v3.SetupCheck{Poll: &v3.SetupPoll{Every: "30s"}}, req("u"), nil)
		if err != nil || string(res.Status) != st || len(clock.sleeps) != 0 {
			t.Errorf("%s: %+v %v sleeps=%v", st, res, err, clock.sleeps)
		}
	}
}

func TestPoll_GivesUp(t *testing.T) {
	e, clock := newExec(InvokerFunc(func(context.Context, Request) ([]byte, error) { return pending(0), nil }))
	res, err := e.Poll(context.Background(), v3.SetupCheck{Poll: &v3.SetupPoll{Every: "1m", MaxEvery: "1m", Factor: 1, GiveUpAfter: "5m"}}, req("u"), nil)
	if !errors.Is(err, ErrGaveUp) {
		t.Fatalf("want ErrGaveUp, got %v", err)
	}
	if res.Status != StatusPending {
		t.Fatalf("last result must be returned: %+v", res)
	}
	var total time.Duration
	for _, d := range clock.sleeps {
		total += d
	}
	if total >= 5*time.Minute {
		t.Fatalf("slept %s, must stay below give_up_after", total)
	}
}

func TestPoll_TransientErrorsRetriedThenCapped(t *testing.T) {
	var n atomic.Int32
	e, clock := newExec(InvokerFunc(func(context.Context, Request) ([]byte, error) {
		if n.Add(1) <= 2 {
			return nil, errors.New("upstream 502")
		}
		return ok("x.done"), nil
	}))
	res, err := e.Poll(context.Background(), v3.SetupCheck{Poll: &v3.SetupPoll{Every: "10s"}}, req("u"), nil)
	if err != nil || res.Status != StatusOK || len(clock.sleeps) != 2 {
		t.Fatalf("recovery: %+v %v %v", res, err, clock.sleeps)
	}

	e2, _ := newExec(InvokerFunc(func(context.Context, Request) ([]byte, error) { return nil, errors.New("always down") }))
	e2.MaxConsecutiveErrors = 3
	_, err = e2.Poll(context.Background(), v3.SetupCheck{Poll: &v3.SetupPoll{Every: "10s"}}, req("u"), nil)
	if !errors.Is(err, ErrTooManyErrors) {
		t.Fatalf("want ErrTooManyErrors, got %v", err)
	}
}

func TestPoll_ContextCancelStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	e, _ := newExec(InvokerFunc(func(context.Context, Request) ([]byte, error) { return pending(0), nil }))
	e.Sleep = func(ctx context.Context, d time.Duration) error { cancel(); return ctx.Err() }
	res, err := e.Poll(ctx, v3.SetupCheck{Poll: &v3.SetupPoll{Every: "10s"}}, req("u"), nil)
	if !errors.Is(err, context.Canceled) || res.Status != StatusPending {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestPoll_WithoutPollRunsOnce(t *testing.T) {
	var n atomic.Int32
	e, clock := newExec(InvokerFunc(func(context.Context, Request) ([]byte, error) { n.Add(1); return pending(0), nil }))
	res, err := e.Poll(context.Background(), v3.SetupCheck{}, req("u"), nil)
	if err != nil || res.Status != StatusPending || n.Load() != 1 || len(clock.sleeps) != 0 {
		t.Fatalf("%+v %v calls=%d", res, err, n.Load())
	}
}

type ctxKey struct{}

func TestRun_CallerContextValuesNeverReachTheHandler(t *testing.T) {
	var seen any
	var baseSeen any
	e, _ := newExec(InvokerFunc(func(ctx context.Context, _ Request) ([]byte, error) {
		seen = ctx.Value(ctxKey{})
		baseSeen = ctx.Value(ctxKey{}) // same key, base context sets it below
		return ok("a.b"), nil
	}))
	callerCtx := context.WithValue(context.Background(), ctxKey{}, "caller-request-state")
	if _, err := e.Run(callerCtx, v3.SetupCheck{}, req("u1")); err != nil {
		t.Fatal(err)
	}
	if seen != nil {
		t.Fatalf("the caller's request context leaked into the shared execution: %v", seen)
	}
	e.BaseContext = func(r Request) context.Context {
		return context.WithValue(context.Background(), ctxKey{}, "principal:"+r.Principal.UserID)
	}
	if _, err := e.Run(callerCtx, v3.SetupCheck{}, req("u1")); err != nil {
		t.Fatal(err)
	}
	if baseSeen != "principal:u1" {
		t.Fatalf("BaseContext must be built from the request principal: %v", baseSeen)
	}
}

func TestRun_TimeoutReleasesInflightEvenIfInvokerNeverReturns(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	var calls atomic.Int32
	e, _ := newExec(InvokerFunc(func(context.Context, Request) ([]byte, error) {
		if calls.Add(1) == 1 {
			<-block // ignores its context: a stuck handler
		}
		return ok("a.b"), nil
	}))
	_, err := e.Run(context.Background(), v3.SetupCheck{Timeout: "50ms"}, req("u1"))
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("want ErrTimeout, got %v", err)
	}
	e.mu.Lock()
	n := len(e.inflight)
	e.mu.Unlock()
	if n != 0 {
		t.Fatalf("inflight must be released at the timeout, have %d", n)
	}
	// The next run is a fresh execution, not a wait on the stuck one.
	if _, err := e.Run(context.Background(), v3.SetupCheck{Timeout: "1s"}, req("u1")); err != nil {
		t.Fatalf("fresh run: %v", err)
	}
}

func TestRun_AbandonedExecutionsAreBounded(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	e, _ := newExec(InvokerFunc(func(context.Context, Request) ([]byte, error) { <-block; return nil, nil }))
	e.MaxAbandoned = 2
	for i := 0; i < 2; i++ {
		if _, err := e.Run(context.Background(), v3.SetupCheck{Timeout: "20ms"}, req("u1")); !errors.Is(err, ErrTimeout) {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	if _, err := e.Run(context.Background(), v3.SetupCheck{Timeout: "20ms"}, req("u1")); !errors.Is(err, ErrBusy) {
		t.Fatalf("want ErrBusy, got %v", err)
	}
}

func TestRun_OrgScopedFlowRequiresOrg(t *testing.T) {
	e, _ := newExec(InvokerFunc(func(context.Context, Request) ([]byte, error) { return ok("a.b"), nil }))
	r := req("u1")
	r.Principal.OrgID = ""
	for _, scope := range []string{"", v3.SetupScopeOrg} {
		r.Scope = scope
		if _, err := e.Run(context.Background(), v3.SetupCheck{}, r); !errors.Is(err, ErrNoPrincipal) {
			t.Errorf("scope %q without org must be refused: %v", scope, err)
		}
	}
	r.Scope = v3.SetupScopeInstance
	if _, err := e.Run(context.Background(), v3.SetupCheck{}, r); err != nil {
		t.Errorf("instance flows need no org: %v", err)
	}
}
