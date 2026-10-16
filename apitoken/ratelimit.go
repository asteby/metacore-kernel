package apitoken

import (
	"math"
	"sync"
	"time"
)

// DefaultRateLimitPerMinute applies to tokens whose RateLimitPerMinute is 0.
const DefaultRateLimitPerMinute = 600

// Limiter decides whether one more request for key fits in a budget of
// perMinute requests. When it does not, retryAfter is how long until it
// would. Hosts with several replicas swap in a shared implementation (Redis).
type Limiter interface {
	Allow(key string, perMinute int) (ok bool, retryAfter time.Duration)
}

// MemoryLimiter is a per-process token bucket per key: capacity perMinute,
// refilled continuously at perMinute/60 per second. Safe for concurrent use.
type MemoryLimiter struct {
	mu      sync.Mutex
	now     func() time.Time
	buckets map[string]*bucket
	calls   int
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewMemoryLimiter returns an empty limiter. now may be nil (time.Now).
func NewMemoryLimiter(now func() time.Time) *MemoryLimiter {
	if now == nil {
		now = time.Now
	}
	return &MemoryLimiter{now: now, buckets: map[string]*bucket{}}
}

// Allow implements Limiter. perMinute <= 0 never limits.
func (l *MemoryLimiter) Allow(key string, perMinute int) (bool, time.Duration) {
	if perMinute <= 0 {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	capacity := float64(perMinute)
	rate := capacity / 60 // tokens per second
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: capacity, last: now}
		l.buckets[key] = b
	} else {
		b.tokens = math.Min(capacity, b.tokens+now.Sub(b.last).Seconds()*rate)
		b.last = now
	}
	l.calls++
	if l.calls%1024 == 0 {
		l.sweep(now)
	}
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration((1 - b.tokens) / rate * float64(time.Second))
	return false, wait
}

// sweep drops buckets idle long enough to be full again (> 1 minute).
func (l *MemoryLimiter) sweep(now time.Time) {
	for k, b := range l.buckets {
		if now.Sub(b.last) > time.Minute {
			delete(l.buckets, k)
		}
	}
}
