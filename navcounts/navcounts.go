// Package navcounts is the generic "where do I go next" primitive: a registry
// of named counters (a model + a declarative where-map + a tone) that the host
// evaluates in ONE batch request and serves at GET /api/nav/counts. The SDK
// paints them as sidebar badges, tab-title counter, bell and favicon dot.
//
// It deliberately reuses query.Aggregate (org scope + soft-delete + the
// validated where vocabulary) so a counter can never run free SQL, and so the
// number a badge shows is computed by the same engine as dashboard widgets.
//
// Scope: counters are org-scoped. Hosts that narrow lists further (branch,
// locked scopes) must pass the same narrowing through Spec.Where so badge ==
// list total. Manifest-declared counters (contributions.badges[]) are a
// separate, manifest-v3 change and are NOT part of this package.
package navcounts

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/asteby/metacore-kernel/query"
	"gorm.io/gorm"
)

// Tone is the single severity vocabulary shared by badges, bell, toast, push.
type Tone string

const (
	ToneInfo      Tone = "info"
	ToneAttention Tone = "attention"
	ToneUrgent    Tone = "urgent"
	ToneCritical  Tone = "critical"
)

func (t Tone) valid() bool {
	switch t {
	case ToneInfo, ToneAttention, ToneUrgent, ToneCritical:
		return true
	}
	return false
}

func (t Tone) rank() int {
	switch t {
	case ToneAttention:
		return 1
	case ToneUrgent:
		return 2
	case ToneCritical:
		return 3
	}
	return 0
}

// MaxItems bounds how many counters one host may register and one request
// evaluates (each is one COUNT(*)).
const MaxItems = 64

// Escalation raises the tone when a narrower predicate also has rows. The
// displayed number stays the base counter's; only the tone/label change.
type Escalation struct {
	Where map[string]any
	Tone  Tone
	Label string
}

// Counter declares one badge.
type Counter struct {
	// ID is unique per host, "<module>.<key>".
	ID string
	// Label is an i18n key (or literal) the SDK uses for aria-label/tooltip.
	Label string
	// Table is the logical table the counter counts.
	Table string
	// Where is the declarative predicate (query.ValidateWhereMap vocabulary).
	Where map[string]any
	Tone  Tone
	// Escalate is evaluated in order; the highest tone with rows > 0 wins.
	Escalate []Escalation
	// Permission the caller needs; empty means always allowed.
	Permission string
	// Target is the UI route the click opens (e.g. "/m/alerts").
	Target string
	// Filter is appended to Target as query params so the click lands on the
	// same predicate the count used (e.g. {"status":"EQ:open"}).
	Filter map[string]string
	// RefreshOn lists data-event models whose changes invalidate this counter.
	RefreshOn []string
}

// Value is one evaluated counter (zero counts are omitted by Compute).
type Value struct {
	ID        string   `json:"id"`
	Count     int64    `json:"count"`
	Tone      Tone     `json:"tone"`
	Label     string   `json:"label"`
	Href      string   `json:"href,omitempty"`
	RefreshOn []string `json:"refresh_on,omitempty"`
}

// Registry holds counters. The zero value is not usable; call New.
type Registry struct {
	mu    sync.RWMutex
	items map[string]Counter
}

func New() *Registry { return &Registry{items: map[string]Counter{}} }

func safeIdent(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r == '_' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

// Register validates and stores counters. It fails loud on the first invalid
// one (unsafe table, bad tone, invalid where, duplicate id) and stores none of
// that call.
func (r *Registry) Register(cs ...Counter) error {
	for _, c := range cs {
		if c.ID == "" {
			return fmt.Errorf("navcounts: counter without id")
		}
		if !safeIdent(c.Table) {
			return fmt.Errorf("navcounts: %s: unsafe table %q", c.ID, c.Table)
		}
		if !c.Tone.valid() {
			return fmt.Errorf("navcounts: %s: invalid tone %q", c.ID, c.Tone)
		}
		if err := query.ValidateWhereMap(c.Where); err != nil {
			return fmt.Errorf("navcounts: %s: %w", c.ID, err)
		}
		for _, e := range c.Escalate {
			if !e.Tone.valid() {
				return fmt.Errorf("navcounts: %s: invalid escalation tone %q", c.ID, e.Tone)
			}
			if err := query.ValidateWhereMap(e.Where); err != nil {
				return fmt.Errorf("navcounts: %s: escalate: %w", c.ID, err)
			}
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := map[string]bool{}
	for _, c := range cs {
		if _, dup := r.items[c.ID]; dup || seen[c.ID] {
			return fmt.Errorf("navcounts: duplicate id %q", c.ID)
		}
		seen[c.ID] = true
	}
	if len(r.items)+len(cs) > MaxItems {
		return fmt.Errorf("navcounts: more than %d counters", MaxItems)
	}
	for _, c := range cs {
		r.items[c.ID] = c
	}
	return nil
}

func mergeWhere(base, extra map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// Compute evaluates every counter the caller may see. allowed may be nil
// (everything allowed). Counters whose table does not exist or whose query
// errors are skipped (an addon not installed must not break the sidebar);
// zero counts are omitted. Output is sorted by id.
func (r *Registry) Compute(ctx context.Context, db *gorm.DB, orgID string, allowed func(permission string) bool, now time.Time) []Value {
	r.mu.RLock()
	cs := make([]Counter, 0, len(r.items))
	for _, c := range r.items {
		cs = append(cs, c)
	}
	r.mu.RUnlock()
	sort.Slice(cs, func(i, j int) bool { return cs[i].ID < cs[j].ID })

	count := func(table string, where map[string]any) (int64, bool) {
		res, err := query.Aggregate(ctx, db, query.AggregateSpec{Table: table, OrgID: orgID, Aggregate: "count", Where: where, Now: now})
		if err != nil || res.Scalar == nil {
			return 0, false
		}
		return int64(*res.Scalar), true
	}

	out := make([]Value, 0, len(cs))
	for _, c := range cs {
		if c.Permission != "" && allowed != nil && !allowed(c.Permission) {
			continue
		}
		n, ok := count(c.Table, c.Where)
		if !ok || n <= 0 {
			continue
		}
		tone, label := c.Tone, c.Label
		for _, e := range c.Escalate {
			if e.Tone.rank() <= tone.rank() {
				continue
			}
			if en, ok := count(c.Table, mergeWhere(c.Where, e.Where)); ok && en > 0 {
				tone = e.Tone
				if e.Label != "" {
					label = e.Label
				}
			}
		}
		out = append(out, Value{ID: c.ID, Count: n, Tone: tone, Label: label, Href: href(c), RefreshOn: c.RefreshOn})
	}
	return out
}

func href(c Counter) string {
	if c.Target == "" {
		return ""
	}
	if len(c.Filter) == 0 {
		return c.Target
	}
	keys := make([]string, 0, len(c.Filter))
	for k := range c.Filter {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	q := url.Values{}
	for _, k := range keys {
		q.Set(k, c.Filter[k])
	}
	sep := "?"
	if strings.Contains(c.Target, "?") {
		sep = "&"
	}
	return c.Target + sep + q.Encode()
}
