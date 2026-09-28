package idempotency

import (
	"context"
	"errors"
	"time"
)

// DefaultLockTimeout is how long a Reserve holds a key while the first
// request runs its handler. A request that has not finished by then is
// presumed dead (crashed replica, killed pod) and the key can be claimed
// again. Set Config.LockTimeout above the p99 latency of the slowest route
// that uses the middleware.
const DefaultLockTimeout = time.Minute

// ErrInFlight is the error the middleware reports (as HTTP 409) when a
// second request arrives with a key that another request is still
// processing.
var ErrInFlight = errors.New("idempotency: a request with this key is still in flight")

// Locker is an optional extension of Store. A store that implements it
// lets the middleware claim a key BEFORE the handler runs, so two requests
// with the same key that arrive together never both execute the handler.
//
// Plain Store implementations (InMemoryStore, custom Redis stores written
// against the two-method interface) keep working unchanged: the middleware
// detects Locker with a type assertion and falls back to the
// check-then-store flow when it is absent.
//
// Stability: new in this release; the method set is frozen once tagged.
// Extending it means a new optional interface, never a new method here.
type Locker interface {
	// Reserve atomically claims key for at most lease. The result says
	// which of three states the key is in:
	//
	//   - Acquired: the caller owns the key. It must run the handler and
	//     then call Store.Put (success) or Release (failure).
	//   - Replay != nil: a completed response is stored; replay it.
	//   - neither: another request holds the key right now (in flight).
	//
	// An error means the store could not answer; the middleware fails
	// closed with 503 rather than risk a double execution.
	Reserve(ctx context.Context, key string, lease time.Duration) (Reservation, error)

	// Release drops a reservation that did not produce a cacheable
	// response, so the client can retry with the same key. token is the
	// Reservation.Token Reserve returned; a reservation already taken over
	// by someone else (lease expired) is left untouched.
	Release(ctx context.Context, key, token string) error
}

// Reservation is the result of Locker.Reserve.
type Reservation struct {
	// Acquired is true when the caller now owns the key.
	Acquired bool
	// Token identifies this reservation for Release. Set iff Acquired.
	Token string
	// Replay is the stored response when the key already completed.
	Replay *Stored
}

// InFlight reports whether another request holds the key.
func (r Reservation) InFlight() bool { return !r.Acquired && r.Replay == nil }
