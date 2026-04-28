package idempotency

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/gofiber/fiber/v2"
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
	UserKey func(c *fiber.Ctx) string

	// CacheStatus chooses which response statuses are stored. Default
	// caches 2xx — clients should not see "200" once, "500" on retry.
	CacheStatus func(status int) bool
}

func defaultCacheStatus(status int) bool { return status >= 200 && status < 300 }

// Middleware returns a Fiber middleware that short-circuits requests
// carrying a known `Idempotency-Key` with the previously stored response.
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
		userKey = func(c *fiber.Ctx) string { return c.IP() }
	}

	return func(c *fiber.Ctx) error {
		clientKey := c.Get(HeaderKey)
		if clientKey == "" {
			// No header → skip the middleware entirely. Behaviour matches
			// Stripe: idempotency is opt-in per request.
			return c.Next()
		}

		ns := userKey(c)
		composite := compositeKey(ns, c.Method(), c.Path(), clientKey)

		if hit, ok := cfg.Store.Get(composite); ok {
			if hit.ContentType != "" {
				c.Set(fiber.HeaderContentType, hit.ContentType)
			}
			c.Set("Idempotent-Replay", "true")
			return c.Status(hit.StatusCode).Send(hit.Body)
		}

		if err := c.Next(); err != nil {
			return err
		}

		status := c.Response().StatusCode()
		if !cache(status) {
			return nil
		}
		body := append([]byte(nil), c.Response().Body()...)
		cfg.Store.Put(composite, Stored{
			StatusCode:  status,
			Body:        body,
			ContentType: string(c.Response().Header.ContentType()),
			ExpiresAt:   time.Now().Add(ttl),
		})
		return nil
	}
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
