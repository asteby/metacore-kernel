package setupflow

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// Principal is the identity a check runs as: the acting USER, never a host
// service account. Hosts build their handler execution context from it so the
// addon's RBAC and tenant scoping apply to the check exactly as they would to
// a click in the UI.
type Principal struct {
	UserID string
	OrgID  string
	Roles  []string
}

func (p Principal) valid() bool { return p.UserID != "" }

// Request is one handler invocation.
type Request struct {
	AddonKey string
	FlowKey  string
	StepKey  string
	Handler  v3.SetupHandler
	// Principal is the acting user (required).
	Principal Principal
	// Attempt is the 0-based execution number inside a Poll (0 for Run).
	Attempt int
}

// Invoker dispatches a SetupHandler (wasm export or native operation) with
// the request's principal and returns the raw handler response (the kernel
// envelope). Hosts implement it over their wasm/native dispatchers.
type Invoker interface {
	Invoke(ctx context.Context, req Request) ([]byte, error)
}

// InvokerFunc adapts a function to Invoker.
type InvokerFunc func(ctx context.Context, req Request) ([]byte, error)

func (f InvokerFunc) Invoke(ctx context.Context, req Request) ([]byte, error) { return f(ctx, req) }

// Errors.
var (
	// ErrNoPrincipal: the request carries no acting user.
	ErrNoPrincipal = errors.New("setupflow: a check must run as a user principal")
	// ErrTimeout: one execution exceeded the check timeout.
	ErrTimeout = errors.New("setupflow: check timed out")
	// ErrGaveUp: polling reached give_up_after (or the retry budget) while the
	// result was still pending; the last result is returned alongside.
	ErrGaveUp = errors.New("setupflow: gave up polling")
	// ErrTooManyErrors: MaxConsecutiveErrors transient failures in a row.
	ErrTooManyErrors = errors.New("setupflow: handler kept failing")
)

// Defaults of the executor.
const (
	DefaultMaxConsecutiveErrors = 5
	DefaultPollMaxEvery         = time.Hour
)

// Executor runs setup checks. The zero value is not usable: set Invoker.
// It is safe for concurrent use.
type Executor struct {
	Invoker Invoker
	// DefaultTimeout applies when the check declares none (default 30s).
	DefaultTimeout time.Duration
	// MaxConsecutiveErrors bounds transient handler failures inside a Poll
	// (default 5).
	MaxConsecutiveErrors int
	// Sleep waits d or until ctx ends; nil uses a timer. Tests inject a fake.
	Sleep func(ctx context.Context, d time.Duration) error
	// Now defaults to time.Now; used for give_up_after.
	Now func() time.Time

	mu       sync.Mutex
	inflight map[string]*flight
}

type flight struct {
	done chan struct{}
	res  CheckResult
	err  error
}

func (e *Executor) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Executor) sleep(ctx context.Context, d time.Duration) error {
	if e.Sleep != nil {
		return e.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (e *Executor) timeout(c v3.SetupCheck) (time.Duration, error) {
	if c.Timeout == "" {
		if e.DefaultTimeout > 0 {
			return e.DefaultTimeout, nil
		}
		return v3.DefaultSetupTimeout, nil
	}
	d, err := time.ParseDuration(c.Timeout)
	if err != nil || d <= 0 || d > v3.MaxSetupTimeout {
		return 0, fmt.Errorf("setupflow: invalid check timeout %q", c.Timeout)
	}
	return d, nil
}

func flightKey(r Request) string {
	return strings.Join([]string{r.Principal.OrgID, r.AddonKey, r.FlowKey, r.StepKey, r.Principal.UserID}, "\x00")
}

// Run executes the check once as the request's principal and returns its
// validated result.
//
// Concurrent Runs for the same (org, addon, flow, step, user) share ONE
// execution: a user double-clicking "verify", or the page and a poll racing,
// never run the handler twice at once. The shared execution is detached from
// any single caller's cancellation (it is bounded by the check timeout), so a
// caller that gives up does not abort the others; it just stops waiting.
// Different users never share an execution: results are permission-scoped.
func (e *Executor) Run(ctx context.Context, check v3.SetupCheck, req Request) (CheckResult, error) {
	if e.Invoker == nil {
		return CheckResult{}, errors.New("setupflow: executor has no Invoker")
	}
	if !req.Principal.valid() {
		return CheckResult{}, ErrNoPrincipal
	}
	timeout, err := e.timeout(check)
	if err != nil {
		return CheckResult{}, err
	}
	key := flightKey(req)

	e.mu.Lock()
	if e.inflight == nil {
		e.inflight = map[string]*flight{}
	}
	f, shared := e.inflight[key]
	if !shared {
		f = &flight{done: make(chan struct{})}
		e.inflight[key] = f
		go e.execute(context.WithoutCancel(ctx), key, f, timeout, req)
	}
	e.mu.Unlock()

	select {
	case <-f.done:
		return f.res, f.err
	case <-ctx.Done():
		return CheckResult{}, ctx.Err()
	}
}

func (e *Executor) execute(ctx context.Context, key string, f *flight, timeout time.Duration, req Request) {
	defer func() {
		if r := recover(); r != nil {
			f.res, f.err = CheckResult{}, fmt.Errorf("setupflow: handler panicked: %v", r)
		}
		e.mu.Lock()
		delete(e.inflight, key)
		e.mu.Unlock()
		close(f.done)
	}()
	ctx, cancel := context.WithTimeoutCause(ctx, timeout, ErrTimeout)
	defer cancel()
	raw, err := e.Invoker.Invoke(ctx, req)
	if err != nil {
		if cause := context.Cause(ctx); errors.Is(cause, ErrTimeout) {
			err = fmt.Errorf("%w after %s: %v", ErrTimeout, timeout, err)
		}
		f.err = err
		return
	}
	if errors.Is(context.Cause(ctx), ErrTimeout) {
		f.err = fmt.Errorf("%w after %s", ErrTimeout, timeout)
		return
	}
	f.res, f.err = ParseCheckResult(raw)
}

// PollDelay is the wait before poll re-run number attempt (0-based): Every,
// then x Factor each time, capped at MaxEvery (default 1h, never below Every).
// A handler's retry_after_s hint, when positive, replaces the computed delay
// but is clamped into [1s, cap]. Pure function; no jitter.
func PollDelay(p v3.SetupPoll, attempt int, hint time.Duration) (time.Duration, error) {
	every, err := time.ParseDuration(p.Every)
	if err != nil || every < v3.MinSetupPollEvery {
		return 0, fmt.Errorf("setupflow: invalid poll.every %q", p.Every)
	}
	maxEvery := DefaultPollMaxEvery
	if p.MaxEvery != "" {
		if maxEvery, err = time.ParseDuration(p.MaxEvery); err != nil || maxEvery < v3.MinSetupPollEvery {
			return 0, fmt.Errorf("setupflow: invalid poll.max_every %q", p.MaxEvery)
		}
	}
	if maxEvery < every {
		maxEvery = every
	}
	factor := p.Factor
	if factor == 0 {
		factor = v3.DefaultSetupPollFact
	}
	if factor < 1 {
		return 0, fmt.Errorf("setupflow: invalid poll.factor %v", p.Factor)
	}
	if hint > 0 {
		return min(max(hint, v3.MinSetupPollEvery), maxEvery), nil
	}
	f := float64(every) * math.Pow(factor, float64(attempt))
	if math.IsInf(f, 0) || f > float64(maxEvery) {
		return maxEvery, nil
	}
	return time.Duration(f), nil
}

// Poll runs the check and, while the result is pending or running and the
// check declares a poll schedule, re-runs it with backoff until it settles.
//
//   - onResult (optional) is called after every execution with the 0-based
//     attempt and the result, so the host can persist progress as it evolves.
//   - A transient failure (handler error, timeout, malformed result) is retried
//     on the same schedule; MaxConsecutiveErrors in a row returns
//     ErrTooManyErrors (wrapping the last cause) with the last good result.
//   - give_up_after (default 24h) bounds the whole wait: the last result comes
//     back with ErrGaveUp.
//   - ctx cancellation returns ctx.Err() with the last result.
//   - Without check.poll it behaves like Run.
func (e *Executor) Poll(ctx context.Context, check v3.SetupCheck, req Request, onResult func(attempt int, r CheckResult)) (CheckResult, error) {
	if check.Poll == nil {
		r, err := e.Run(ctx, check, req)
		if err == nil && onResult != nil {
			onResult(0, r)
		}
		return r, err
	}
	giveUp := v3.DefaultSetupGiveUpAft
	if check.Poll.GiveUpAfter != "" {
		d, err := time.ParseDuration(check.Poll.GiveUpAfter)
		if err != nil || d <= 0 {
			return CheckResult{}, fmt.Errorf("setupflow: invalid poll.give_up_after %q", check.Poll.GiveUpAfter)
		}
		giveUp = d
	}
	maxErrs := e.MaxConsecutiveErrors
	if maxErrs <= 0 {
		maxErrs = DefaultMaxConsecutiveErrors
	}
	deadline := e.now().Add(giveUp)

	var (
		last     CheckResult
		haveLast bool
		errRun   int
	)
	for attempt := 0; ; attempt++ {
		req.Attempt = attempt
		r, err := e.Run(ctx, check, req)
		var hint time.Duration
		switch {
		case err == nil:
			errRun = 0
			last, haveLast = r, true
			if onResult != nil {
				onResult(attempt, r)
			}
			if r.Status.Settled() {
				return r, nil
			}
			hint = time.Duration(r.RetryAfterS) * time.Second
		case ctx.Err() != nil:
			return last, ctx.Err()
		case errors.Is(err, ErrNoPrincipal):
			return last, err
		default:
			errRun++
			if errRun >= maxErrs {
				return last, fmt.Errorf("%w (%d in a row): %w", ErrTooManyErrors, errRun, err)
			}
		}
		delay, derr := PollDelay(*check.Poll, attempt, hint)
		if derr != nil {
			return last, derr
		}
		if !e.now().Add(delay).Before(deadline) {
			if !haveLast {
				return last, fmt.Errorf("%w before any successful result", ErrGaveUp)
			}
			return last, ErrGaveUp
		}
		if err := e.sleep(ctx, delay); err != nil {
			return last, err
		}
	}
}
