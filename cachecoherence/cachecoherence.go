package cachecoherence

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Canonical invalidation orders. Lower runs first. Layers may use any int; the
// constants only name the dependency tiers the kernel itself relies on.
const (
	// OrderDatabase: server-side plan state (prepared statements). Must be
	// first: every layer below re-reads the database.
	OrderDatabase = 10
	// OrderSchema: caches of schema introspection (live columns, table flags).
	OrderSchema = 20
	// OrderManifest: manifest / installation registries.
	OrderManifest = 30
	// OrderRuntime: loaded backend modules (wasm), native services.
	OrderRuntime = 40
	// OrderPermission: RBAC and capability grants.
	OrderPermission = 50
	// OrderCatalog: capability catalog, i18n catalogs.
	OrderCatalog = 60
	// OrderRouting: gateway routes, webhook/scheduler tables.
	OrderRouting = 70
	// OrderNavigation: nav / sidebar projections.
	OrderNavigation = 80
	// OrderMetadata: dynamic-model metadata and its versions.
	OrderMetadata = 90
	// OrderPublish: anything that tells a client (HTTP validators, WS, SSE).
	OrderPublish = 100
)

// Epoch is the version vector of one (org, addon) view of the world.
type Epoch struct {
	Global uint64 `json:"global"`
	Org    uint64 `json:"org"`
	Addon  uint64 `json:"addon"`
}

// Token renders the epoch as a short opaque string ("g.o.a" in base 36). Equal
// epochs yield equal tokens; any bump that touches the scope changes it.
func (e Epoch) Token() string {
	return strconv.FormatUint(e.Global, 36) + "." + strconv.FormatUint(e.Org, 36) + "." + strconv.FormatUint(e.Addon, 36)
}

// ParseToken is the inverse of Token. ok is false for anything else.
func ParseToken(s string) (Epoch, bool) {
	parts := strings.Split(strings.TrimSpace(s), ".")
	if len(parts) != 3 {
		return Epoch{}, false
	}
	var v [3]uint64
	for i, p := range parts {
		n, err := strconv.ParseUint(p, 36, 64)
		if err != nil {
			return Epoch{}, false
		}
		v[i] = n
	}
	return Epoch{Global: v[0], Org: v[1], Addon: v[2]}, true
}

// Event describes one bump. Epoch carries the NEW values for the scope it
// touched (Org / Addon are zero when the bump did not name them).
type Event struct {
	Seq      uint64    `json:"seq"`
	Reason   string    `json:"reason"`
	OrgID    string    `json:"org_id,omitempty"`
	AddonKey string    `json:"addon_key,omitempty"`
	At       time.Time `json:"at"`
	Epoch    Epoch     `json:"epoch"`
	// Remote is true when the event was applied from a peer (ApplyRemote).
	Remote bool `json:"remote,omitempty"`
}

// Option narrows a bump's scope.
type Option func(*Event)

// WithOrg scopes the bump to one organization.
func WithOrg(orgID string) Option { return func(e *Event) { e.OrgID = orgID } }

// WithAddon scopes the bump to one addon (across every organization: the
// compiled module and the manifest of an addon are shared).
func WithAddon(key string) Option { return func(e *Event) { e.AddonKey = key } }

// InvalidateFunc drops whatever the layer derived. It must be idempotent and
// should be fast; it receives the event with the new epoch.
type InvalidateFunc func(ctx context.Context, ev Event) error

type invalidator struct {
	name  string
	order int
	fn    InvalidateFunc
	seq   uint64 // monotonic registration id, tie-breaker + replace guard
}

// InvalidatorStatus is the diagnostic view of one registered layer.
type InvalidatorStatus struct {
	Name       string    `json:"name"`
	Order      int       `json:"order"`
	Runs       uint64    `json:"runs"`
	LastSeq    uint64    `json:"last_seq"`
	LastRun    time.Time `json:"last_run,omitempty"`
	LastMillis float64   `json:"last_ms"`
	LastError  string    `json:"last_error,omitempty"`
	// UpToDate is true when the layer processed the latest bump without error.
	UpToDate bool `json:"up_to_date"`
}

// Status is the diagnostic snapshot served by the host's cache-status endpoint.
type Status struct {
	Global      uint64                 `json:"global_epoch"`
	Seq         uint64                 `json:"seq"`
	StartedAt   time.Time              `json:"started_at"`
	Orgs        map[string]uint64      `json:"orgs"`
	Addons      map[string]uint64      `json:"addons"`
	Invalidated []InvalidatorStatus    `json:"invalidators"`
	Recent      []Event                `json:"recent"`
	Providers   map[string]any         `json:"providers,omitempty"`
	AllUpToDate bool                   `json:"all_up_to_date"`
	Extra       map[string]interface{} `json:"-"`
}

type runState struct {
	runs    uint64
	lastSeq uint64
	lastRun time.Time
	lastDur time.Duration
	lastErr string
}

// Registry holds epochs and invalidators. Use [Default] unless a test needs
// isolation.
type Registry struct {
	// bumpMu serialises bumps so invalidators of two bumps never interleave
	// and epochs are published in sequence order.
	bumpMu sync.Mutex

	mu        sync.RWMutex
	global    uint64
	seq       uint64
	orgs      map[string]uint64
	addons    map[string]uint64
	started   time.Time
	invs      map[string]*invalidator
	states    map[string]*runState
	regSeq    uint64
	subs      map[int]func(Event)
	subSeq    int
	recent    []Event
	providers map[string]func() any
}

const recentMax = 32

// NewRegistry returns an empty registry. The global epoch starts at the
// current time in milliseconds so tokens never repeat across restarts.
func NewRegistry() *Registry {
	now := time.Now()
	return &Registry{
		global:    uint64(now.UnixMilli()),
		orgs:      map[string]uint64{},
		addons:    map[string]uint64{},
		started:   now,
		invs:      map[string]*invalidator{},
		states:    map[string]*runState{},
		subs:      map[int]func(Event){},
		providers: map[string]func() any{},
	}
}

// Default is the process-wide registry the kernel packages register with.
var Default = NewRegistry()

// Register adds (or replaces, by name) an invalidator. The returned func
// removes it, but only if it has not been replaced since. Replacement keeps
// constructor-registered layers correct when a host builds a service twice
// (hot reload, tests): the newest instance is the one that gets flushed.
func (r *Registry) Register(name string, order int, fn InvalidateFunc) (unregister func()) {
	if name == "" || fn == nil {
		return func() {}
	}
	r.mu.Lock()
	r.regSeq++
	mine := r.regSeq
	r.invs[name] = &invalidator{name: name, order: order, fn: fn, seq: mine}
	if _, ok := r.states[name]; !ok {
		// A freshly registered layer starts consistent with the epoch that
		// exists now: it has nothing cached yet.
		r.states[name] = &runState{lastSeq: r.seq}
	}
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		if cur, ok := r.invs[name]; ok && cur.seq == mine {
			delete(r.invs, name)
			delete(r.states, name)
		}
		r.mu.Unlock()
	}
}

// RegisterProvider adds a named diagnostic block to Status (e.g. the stale-plan
// retry counters). The function must be cheap and concurrency-safe.
func (r *Registry) RegisterProvider(name string, fn func() any) {
	if name == "" || fn == nil {
		return
	}
	r.mu.Lock()
	r.providers[name] = fn
	r.mu.Unlock()
}

// Subscribe registers a callback invoked after every bump, once the new epoch
// is published. Callbacks run synchronously on the bumping goroutine and must
// not block (hand off to a channel). The returned func unsubscribes.
func (r *Registry) Subscribe(fn func(Event)) (cancel func()) {
	if fn == nil {
		return func() {}
	}
	r.mu.Lock()
	r.subSeq++
	id := r.subSeq
	r.subs[id] = fn
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		delete(r.subs, id)
		r.mu.Unlock()
	}
}

// Epoch returns the current version vector for (orgID, addonKey). Either may
// be empty: the matching component is then zero.
func (r *Registry) Epoch(orgID, addonKey string) Epoch {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e := Epoch{Global: r.global}
	if orgID != "" {
		e.Org = r.orgs[orgID]
	}
	if addonKey != "" {
		e.Addon = r.addons[addonKey]
	}
	return e
}

// ViewEpoch is the epoch of "everything an organization's UI is built from":
// the global component, the organization's, and the SUM of every addon's. Any
// bump that can change what that organization sees (global, its own, or any
// addon's) moves it, which is what a single X-Cache-Epoch response header for
// the whole SPA needs; the sum is monotonic because every counter is.
func (r *Registry) ViewEpoch(orgID string) Epoch {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e := Epoch{Global: r.global}
	if orgID != "" {
		e.Org = r.orgs[orgID]
	}
	for _, v := range r.addons {
		e.Addon += v
	}
	return e
}

// Bump invalidates every derived cache for the given scope, in order, and then
// publishes the new epoch. It never returns an error to the mutation path that
// called it (a cache problem must not fail an install); use [Registry.BumpContext]
// to observe failures. Safe for concurrent use.
func (r *Registry) Bump(reason string, opts ...Option) Event {
	ev, _ := r.BumpContext(context.Background(), reason, opts...)
	return ev
}

// BumpContext is Bump with a context and the joined invalidator errors.
func (r *Registry) BumpContext(ctx context.Context, reason string, opts ...Option) (Event, error) {
	ev := Event{Reason: reason}
	for _, o := range opts {
		o(&ev)
	}
	return r.run(ctx, ev)
}

// ApplyRemote applies an event produced by a peer process: it runs the
// invalidators and advances the local epochs to at least the event's values,
// without re-publishing as a local bump. Unknown scopes are ignored.
func (r *Registry) ApplyRemote(ctx context.Context, ev Event) (Event, error) {
	ev.Remote = true
	return r.run(ctx, ev)
}

func (r *Registry) run(ctx context.Context, ev Event) (Event, error) {
	r.bumpMu.Lock()
	defer r.bumpMu.Unlock()

	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	r.mu.RLock()
	next := Epoch{Global: r.global, Org: r.orgs[ev.OrgID], Addon: r.addons[ev.AddonKey]}
	seq := r.seq + 1
	list := make([]*invalidator, 0, len(r.invs))
	for _, v := range r.invs {
		list = append(list, v)
	}
	r.mu.RUnlock()

	// The epoch the invalidators (and later the consumers) will observe.
	//   - nothing named: a global flush, the global component moves (and with
	//     it every token);
	//   - org named: that organization's view moves; addon named: that addon's
	//     view moves for every organization that watches it.
	if ev.OrgID == "" && ev.AddonKey == "" {
		next.Global++
	}
	if ev.OrgID != "" {
		next.Org++
	}
	if ev.AddonKey != "" {
		next.Addon++
	}
	if ev.Remote {
		// Never move backwards: take the max of ours and the peer's.
		next = maxEpoch(next, ev.Epoch)
	}
	ev.Seq = seq
	ev.Epoch = next

	sort.Slice(list, func(i, j int) bool {
		if list[i].order != list[j].order {
			return list[i].order < list[j].order
		}
		if list[i].name != list[j].name {
			return list[i].name < list[j].name
		}
		return list[i].seq < list[j].seq
	})

	var errs []error
	for _, inv := range list {
		start := time.Now()
		err := safeInvoke(ctx, inv, ev)
		dur := time.Since(start)
		r.mu.Lock()
		st := r.states[inv.name]
		if st == nil {
			st = &runState{}
			r.states[inv.name] = st
		}
		st.runs++
		st.lastRun = start
		st.lastDur = dur
		if err != nil {
			st.lastErr = err.Error()
			// lastSeq stays behind: the layer is NOT up to date.
		} else {
			st.lastErr = ""
			st.lastSeq = seq
		}
		r.mu.Unlock()
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", inv.name, err))
			slog.Warn("cachecoherence.invalidate_failed", "layer", inv.name, "reason", ev.Reason,
				"org", ev.OrgID, "addon", ev.AddonKey, "err", err.Error())
		}
	}

	// Publish AFTER the invalidators ran.
	r.mu.Lock()
	r.seq = seq
	if next.Global > r.global {
		r.global = next.Global
	}
	if ev.OrgID != "" {
		r.orgs[ev.OrgID] = next.Org
	}
	if ev.AddonKey != "" {
		r.addons[ev.AddonKey] = next.Addon
	}
	r.recent = append(r.recent, ev)
	if len(r.recent) > recentMax {
		r.recent = r.recent[len(r.recent)-recentMax:]
	}
	subs := make([]func(Event), 0, len(r.subs))
	for _, s := range r.subs {
		subs = append(subs, s)
	}
	r.mu.Unlock()

	slog.Info("cachecoherence.bump", "seq", seq, "reason", ev.Reason, "org", ev.OrgID,
		"addon", ev.AddonKey, "token", ev.Epoch.Token(), "layers", len(list), "errors", len(errs))
	for _, s := range subs {
		func() {
			defer func() { _ = recover() }()
			s(ev)
		}()
	}
	return ev, errors.Join(errs...)
}

func maxEpoch(a, b Epoch) Epoch {
	if b.Global > a.Global {
		a.Global = b.Global
	}
	if b.Org > a.Org {
		a.Org = b.Org
	}
	if b.Addon > a.Addon {
		a.Addon = b.Addon
	}
	return a
}

func safeInvoke(ctx context.Context, inv *invalidator, ev Event) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("panic: %v\n%s", rec, debug.Stack())
		}
	}()
	return inv.fn(ctx, ev)
}

// Status returns a diagnostic snapshot.
func (r *Registry) Status() Status {
	r.mu.RLock()
	defer r.mu.RUnlock()
	st := Status{
		Global:    r.global,
		Seq:       r.seq,
		StartedAt: r.started,
		Orgs:      make(map[string]uint64, len(r.orgs)),
		Addons:    make(map[string]uint64, len(r.addons)),
		Recent:    append([]Event(nil), r.recent...),
	}
	for k, v := range r.orgs {
		st.Orgs[k] = v
	}
	for k, v := range r.addons {
		st.Addons[k] = v
	}
	st.AllUpToDate = true
	for name, inv := range r.invs {
		rs := r.states[name]
		is := InvalidatorStatus{Name: name, Order: inv.order}
		if rs != nil {
			is.Runs = rs.runs
			is.LastSeq = rs.lastSeq
			is.LastRun = rs.lastRun
			is.LastMillis = float64(rs.lastDur.Microseconds()) / 1000
			is.LastError = rs.lastErr
			is.UpToDate = rs.lastErr == "" && rs.lastSeq >= r.seq
		}
		if !is.UpToDate {
			st.AllUpToDate = false
		}
		st.Invalidated = append(st.Invalidated, is)
	}
	sort.Slice(st.Invalidated, func(i, j int) bool {
		if st.Invalidated[i].Order != st.Invalidated[j].Order {
			return st.Invalidated[i].Order < st.Invalidated[j].Order
		}
		return st.Invalidated[i].Name < st.Invalidated[j].Name
	})
	if len(r.providers) > 0 {
		st.Providers = make(map[string]any, len(r.providers))
		for k, fn := range r.providers {
			st.Providers[k] = fn()
		}
	}
	return st
}

// Watcher lets a cache holder ask "has the world changed since I filled
// myself?" without subscribing to anything.
type Watcher struct {
	r          *Registry
	org, addon string
	mu         sync.Mutex
	seen       Epoch
}

// NewWatcher snapshots the current epoch of (org, addon) on the registry.
func (r *Registry) NewWatcher(orgID, addonKey string) *Watcher {
	return &Watcher{r: r, org: orgID, addon: addonKey, seen: r.Epoch(orgID, addonKey)}
}

// Stale reports whether a bump touched the watched scope since the last
// Refresh (or construction).
func (w *Watcher) Stale() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.r.Epoch(w.org, w.addon) != w.seen
}

// Refresh marks the watcher current and reports whether it had been stale.
// Call it BEFORE re-reading the source so a bump during the re-read is not lost.
func (w *Watcher) Refresh() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	cur := w.r.Epoch(w.org, w.addon)
	was := cur != w.seen
	w.seen = cur
	return was
}

// Token is the epoch token the watcher last saw.
func (w *Watcher) Token() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.seen.Token()
}

// --- package-level conveniences over Default --------------------------------

// Bump invalidates every registered cache on [Default] and publishes a new epoch.
func Bump(reason string, opts ...Option) Event { return Default.Bump(reason, opts...) }

// BumpContext is Bump returning the joined invalidator errors.
func BumpContext(ctx context.Context, reason string, opts ...Option) (Event, error) {
	return Default.BumpContext(ctx, reason, opts...)
}

// Register adds an invalidator to [Default].
func Register(name string, order int, fn InvalidateFunc) (unregister func()) {
	return Default.Register(name, order, fn)
}

// Subscribe attaches to [Default].
func Subscribe(fn func(Event)) (cancel func()) { return Default.Subscribe(fn) }

// ViewEpoch is [Registry.ViewEpoch] on [Default].
func ViewEpoch(orgID string) Epoch { return Default.ViewEpoch(orgID) }

// CurrentEpoch is [Registry.Epoch] on [Default].
func CurrentEpoch(orgID, addonKey string) Epoch { return Default.Epoch(orgID, addonKey) }

// NewWatcher is [Registry.NewWatcher] on [Default].
func NewWatcher(orgID, addonKey string) *Watcher { return Default.NewWatcher(orgID, addonKey) }

// SnapshotStatus is [Registry.Status] on [Default].
func SnapshotStatus() Status { return Default.Status() }
