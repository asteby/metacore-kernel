package inbound

import (
	"errors"
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
// own Limiter.
type TokenBucket struct {
	Rate  float64
	Burst int
	// MaxKeys bounds memory (default 10000); idle buckets are dropped first.
	MaxKeys int
	Now     func() time.Time

	mu sync.Mutex
	b  map[string]*bucket
}

type bucket struct {
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
		t.b = map[string]*bucket{}
	}
	b, ok := t.b[key]
	if !ok {
		if len(t.b) >= max {
			for k, v := range t.b {
				if n.Sub(v.last) > time.Minute {
					delete(t.b, k)
				}
			}
			if len(t.b) >= max {
				return false // fail closed under key flooding
			}
		}
		b = &bucket{tokens: float64(t.Burst), last: n}
		t.b[key] = b
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
