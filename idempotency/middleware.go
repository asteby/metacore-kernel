package idempotency

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/gofiber/fiber/v3"
)

const (
	// HeaderKey is the conventional HTTP header (Stripe / IETF draft-09).
	HeaderKey = "Idempotency-Key"

	// DefaultTTL is the replay window. 24h matches Stripe; long enough for
	// any reasonable retry storm, short enough that the in-memory store
	// stays cheap.
	DefaultTTL = 24 * time.Hour
)

// Config tunes the middleware. Zero-value uses sensible defaults.
type Config struct {
	// Store backs the replay cache. Required.
	Store Store

	// TTL overrides DefaultTTL. Useful for endpoints that should expire
	// faster (e.g. login) or never (which would defeat the purpose, but
	// some teams want it).
	TTL time.Duration

	// UserKey lets the middleware namespace cache keys by user, so two
	// users sending the same Idempotency-Key don't collide. Receives the
	// fiber context and returns a stable per-user identifier (typically
	// the JWT subject). When nil, the middleware namespaces by remote IP.
	UserKey func(c fiber.Ctx) string

	// CacheStatus chooses which response statuses are stored. Default
	// caches 2xx — clients should not see "200" once, "500" on retry.
	CacheStatus func(status int) bool

	// LockTimeout is the lease a Locker store holds on a key while the
	// first request runs. Zero uses DefaultLockTimeout. Ignored when the
	// Store does not implement Locker.
	LockTimeout time.Duration

	// InFlightWait is how long a request whose key is held by another,
	// still-running request waits for that request to finish (and then
	// replays its response) before giving up with 409 Conflict. Zero
	// answers 409 immediately, which is what Stripe does. Ignored when the
	// Store does not implement Locker.
	InFlightWait time.Duration
}

// inFlightPoll is how often a waiting request re-checks the key.
const inFlightPoll = 50 * time.Millisecond

func defaultCacheStatus(status int) bool { return status >= 200 && status < 300 }

// Middleware returns a Fiber middleware that short-circuits requests
// carrying a known `Idempotency-Key` with the previously stored response.
//
// When the Store also implements Locker (GormStore does), the key is
// claimed before the handler runs: a duplicate that arrives while the
// first request is still processing never executes the handler. It waits
// up to Config.InFlightWait for the first request to finish and replays
// its response, or gets 409 Conflict (`Retry-After: 1`, code
// `idempotency_key_in_flight`). A handler error, panic or non-cacheable
// status releases the claim so the client may retry. Plain Stores keep
// the original check-then-store behaviour.
//
// Mount it on POST routes that mutate state (create, import, payments).
//
//	api.Post("/dynamic/:model",
//	    idempotency.Middleware(idempotency.Config{Store: store}),
//	    h.create)
func Middleware(cfg Config) fiber.Handler {
	if cfg.Store == nil {
		panic("idempotency: Config.Store is required")
	}
	ttl := cfg.TTL
	if ttl == 0 {
		ttl = DefaultTTL
	}
	cache := cfg.CacheStatus
	if cache == nil {
		cache = defaultCacheStatus
	}
	userKey := cfg.UserKey
	if userKey == nil {
		userKey = func(c fiber.Ctx) string { return c.IP() }
	}

	lease := cfg.LockTimeout
	if lease <= 0 {
		lease = DefaultLockTimeout
	}
	locker, _ := cfg.Store.(Locker)

	return func(c fiber.Ctx) error {
		clientKey := c.Get(HeaderKey)
		if clientKey == "" {
			// No header → skip the middleware entirely. Behaviour matches
			// Stripe: idempotency is opt-in per request.
			return c.Next()
		}

		ns := userKey(c)
		composite := compositeKey(ns, c.Method(), c.Path(), clientKey)

		if locker != nil {
			return serveLocked(c, cfg, locker, composite, lease, ttl, cache)
		}

		if hit, ok := cfg.Store.Get(composite); ok {
			return replay(c, hit)
		}

		if err := c.Next(); err != nil {
			return err
		}
		store(c, cfg.Store, composite, ttl, cache)
		return nil
	}
}

// serveLocked is the flow for stores that implement Locker: claim the key
// first, so a concurrent duplicate never reaches the handler.
func serveLocked(c fiber.Ctx, cfg Config, locker Locker, key string, lease, ttl time.Duration, cache func(int) bool) error {
	ctx := c.Context()
	res, err := locker.Reserve(ctx, key, lease)
	if err != nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "idempotency store unavailable")
	}
	if res.InFlight() && cfg.InFlightWait > 0 {
		deadline := time.Now().Add(cfg.InFlightWait)
		for res.InFlight() && time.Now().Before(deadline) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(min(inFlightPoll, time.Until(deadline))):
			}
			if res, err = locker.Reserve(ctx, key, lease); err != nil {
				return fiber.NewError(fiber.StatusServiceUnavailable, "idempotency store unavailable")
			}
		}
	}
	switch {
	case res.Replay != nil:
		return replay(c, res.Replay)
	case !res.Acquired:
		c.Set(fiber.HeaderRetryAfter, "1")
		return c.Status(fiber.StatusConflict).JSON(fiber.Map{
			"success": false,
			"code":    "idempotency_key_in_flight",
			"message": ErrInFlight.Error(),
		})
	}

	stored := false
	defer func() {
		if !stored {
			// Handler failed, panicked or produced an uncacheable status:
			// free the key so the client's retry runs the handler again.
			_ = locker.Release(context.WithoutCancel(ctx), key, res.Token)
		}
	}()
	if err := c.Next(); err != nil {
		return err
	}
	stored = store(c, cfg.Store, key, ttl, cache)
	return nil
}

func replay(c fiber.Ctx, hit *Stored) error {
	if hit.ContentType != "" {
		c.Set(fiber.HeaderContentType, hit.ContentType)
	}
	c.Set("Idempotent-Replay", "true")
	return c.Status(hit.StatusCode).Send(hit.Body)
}

// store caches the current response when its status qualifies and reports
// whether it did.
func store(c fiber.Ctx, s Store, key string, ttl time.Duration, cache func(int) bool) bool {
	status := c.Response().StatusCode()
	if !cache(status) {
		return false
	}
	body := append([]byte(nil), c.Response().Body()...)
	s.Put(key, Stored{
		StatusCode:  status,
		Body:        body,
		ContentType: string(c.Response().Header.ContentType()),
		ExpiresAt:   time.Now().Add(ttl),
	})
	return true
}

// compositeKey hashes namespace + method + path + client key into a single
// stable ID. Hashing avoids accidental key bloat and keeps user-supplied
// values out of the in-memory map verbatim.
func compositeKey(ns, method, path, clientKey string) string {
	h := sha256.New()
	h.Write([]byte(ns))
	h.Write([]byte{0})
	h.Write([]byte(method))
	h.Write([]byte{0})
	h.Write([]byte(path))
	h.Write([]byte{0})
	h.Write([]byte(clientKey))
	return hex.EncodeToString(h.Sum(nil))
}
