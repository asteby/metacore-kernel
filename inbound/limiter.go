package inbound

import (
	"container/list"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Limiter decides whether one more request for key may proceed.
type Limiter interface {
	Allow(key string) bool
}

// TokenBucket is a small in-process limiter: Rate tokens/second, Burst
// capacity, per key. The repo has no shared HTTP limiter primitive, so this
// is the default; multi-replica hosts should limit at the edge or plug their
// own Limiter. When MaxKeys is reached the least-recently-used bucket is
// evicted (O(1)); a new key is never refused just because the table is full,
// so key flooding cannot lock out everyone else.
type TokenBucket struct {
	Rate  float64
	Burst int
	// MaxKeys bounds memory (default 10000).
	MaxKeys int
	Now     func() time.Time

	mu  sync.Mutex
	b   map[string]*list.Element
	lru *list.List // front = most recently used
}

type bucket struct {
	key    string
	tokens float64
	last   time.Time
}

// Allow implements Limiter.
func (t *TokenBucket) Allow(key string) bool {
	now := time.Now
	if t.Now != nil {
		now = t.Now
	}
	n := now()
	max := t.MaxKeys
	if max <= 0 {
		max = 10000
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.b == nil {
		t.b = map[string]*list.Element{}
		t.lru = list.New()
	}
	var b *bucket
	if el, ok := t.b[key]; ok {
		t.lru.MoveToFront(el)
		b = el.Value.(*bucket)
	} else {
		for len(t.b) >= max {
			old := t.lru.Back()
			delete(t.b, old.Value.(*bucket).key)
			t.lru.Remove(old)
		}
		b = &bucket{key: key, tokens: float64(t.Burst), last: n}
		t.b[key] = t.lru.PushFront(b)
	}
	b.tokens += n.Sub(b.last).Seconds() * t.Rate
	if b.tokens > float64(t.Burst) {
		b.tokens = float64(t.Burst)
	}
	b.last = n
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// LimiterKeyForIP normalizes an address into a limiter key: IPv4 as is, IPv6
// grouped by /64 (one subscriber owns a whole /64, so per-address keys would
// be free to rotate).
func LimiterKeyForIP(addr string) string {
	ip, err := netip.ParseAddr(strings.TrimSpace(addr))
	if err != nil {
		return addr
	}
	ip = ip.Unmap()
	if ip.Is6() {
		p, _ := ip.Prefix(64)
		return p.String()
	}
	return ip.String()
}

// ClientIPFrom resolves the caller address. RemoteAddr is used unless it
// belongs to one of trusted (reverse proxies the host controls); then
// X-Forwarded-For is walked from the right and the first address NOT in
// trusted is the client. Without trusted proxies X-Forwarded-For is ignored
// (it is trivially forged).
func ClientIPFrom(r *http.Request, trusted []netip.Prefix) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	remote, err := netip.ParseAddr(host)
	if err != nil || len(trusted) == 0 || !inPrefixes(remote, trusted) {
		return host
	}
	parts := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(parts) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(parts[i]))
		if err != nil {
			return host // malformed chain: do not trust it
		}
		if !inPrefixes(a, trusted) {
			return a.String()
		}
	}
	return host
}

func inPrefixes(a netip.Addr, ps []netip.Prefix) bool {
	a = a.Unmap()
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// schemaIssues flattens a jsonschema error into pointer+keyword pairs. Values
// from the payload are never echoed (they may be personal data).
func schemaIssues(err error) []FieldIssue {
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return []FieldIssue{{Path: "/", Rule: "schema"}}
	}
	seen := map[FieldIssue]bool{}
	var walk func(*jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			rule := "schema"
			if kp := e.ErrorKind.KeywordPath(); len(kp) > 0 {
				rule = kp[len(kp)-1]
			}
			seen[FieldIssue{Path: "/" + strings.Join(e.InstanceLocation, "/"), Rule: rule}] = true
			return
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(ve)
	out := make([]FieldIssue, 0, len(seen))
	for i := range seen {
		out = append(out, i)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Rule < out[j].Rule
	})
	if len(out) > maxSchemaDetails {
		out = out[:maxSchemaDetails]
	}
	return out
}
