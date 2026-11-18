package inbound

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/modelbase"
	"github.com/asteby/metacore-kernel/query"
)

// ------------------------------------------------------------------ A1 ---

func TestA1_NonceCapIsPerOrganization(t *testing.T) {
	e := newEnv(t, nil)
	e.ver.MaxNonces = 3
	for i := 0; i < 3; i++ {
		if c, _ := e.send(t, e.orgA, "stock.changed", stockBody("a"+strconv.Itoa(i), "S", 1), nil); c != 200 {
			t.Fatalf("A%d: %d", i, c)
		}
	}
	// Org A filled its own window: only A is locked out.
	if c, _ := e.send(t, e.orgA, "stock.changed", stockBody("a-x", "S", 1), nil); c != 401 {
		t.Fatalf("A over its cap must fail closed, got %d", c)
	}
	for i := 0; i < 3; i++ {
		if c, _ := e.send(t, e.orgB, "stock.changed", stockBody("b"+strconv.Itoa(i), "S", 1), nil); c != 200 {
			t.Fatalf("org B affected by org A flooding: %d", c)
		}
	}
	// The window drains with time.
	e.now = e.now.Add(DefaultNonceTTL + time.Second)
	if c, _ := e.send(t, e.orgA, "stock.changed", stockBody("a-y", "S", 1), nil); c != 200 {
		t.Fatalf("A after ttl: %d", c)
	}
}

func TestA1_RejectedRequestsDoNotBurnNonce(t *testing.T) {
	tg := &toggleLimiter{}
	e := newEnv(t, func(c *Config) { c.Limiter = tg })
	fixed := func(nonce, body string, ct string) func(*http.Request) {
		return func(r *http.Request) {
			ts := strconv.FormatInt(e.now.Unix(), 10)
			r.Header.Set("X-Metacore-Nonce", nonce)
			r.Header.Set("Content-Type", ct)
			r.Header.Set("X-Metacore-Signature", SignCanonicalV1(e.secrets[e.orgA], ts, nonce, "POST", r.URL.Path, []byte(body)))
		}
	}
	bad := `{"event_id":"x"}`
	// 422 (schema) then the SAME nonce on a valid event must still work.
	if c, _ := e.send(t, e.orgA, "stock.changed", bad, fixed("N1", bad, "application/json")); c != 422 {
		t.Fatalf("schema: %d", c)
	}
	good := stockBody("g1", "S", 1)
	// 415 does not burn it either.
	if c, _ := e.send(t, e.orgA, "stock.changed", good, fixed("N1", good, "text/plain")); c != 415 {
		t.Fatalf("media: %d", c)
	}
	if c, out := e.send(t, e.orgA, "stock.changed", good, fixed("N1", good, "application/json")); c != 200 {
		t.Fatalf("nonce burned by a rejected request: %d %v", c, out)
	}
	// 429 (limiter) does not burn it.
	tg.deny.Store(true)
	g2 := stockBody("g2", "S", 1)
	if c, _ := e.send(t, e.orgA, "stock.changed", g2, fixed("N2", g2, "application/json")); c != 429 {
		t.Fatalf("limiter: %d", c)
	}
	tg.deny.Store(false)
	if c, out := e.send(t, e.orgA, "stock.changed", g2, fixed("N2", g2, "application/json")); c != 200 {
		t.Fatalf("nonce burned by a 429: %d %v", c, out)
	}
	// The genuinely replayed nonce is still refused.
	if c, _ := e.send(t, e.orgA, "stock.changed", stockBody("g3", "S", 1), fixed("N2", stockBody("g3", "S", 1), "application/json")); c != 401 {
		t.Fatalf("replay: %d", c)
	}
}

type toggleLimiter struct{ deny atomic.Bool }

func (l *toggleLimiter) Allow(string) bool { return !l.deny.Load() }

func TestA1_UnauthenticatedNeverTouchesNonceCache(t *testing.T) {
	e := newEnv(t, nil)
	for i := 0; i < 50; i++ {
		e.send(t, e.orgA, "stock.changed", stockBody("u"+strconv.Itoa(i), "S", 1), func(r *http.Request) {
			r.Header.Set("X-Metacore-Signature", strings.Repeat("0", 64))
		})
	}
	if len(e.ver.orgs) != 0 {
		t.Fatalf("forged requests populated the nonce cache: %d orgs", len(e.ver.orgs))
	}
}

func TestA1_NonceExpiryIsAmortizedNotAScan(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	v := &HMACCanonicalV1{Now: func() time.Time { return now }, MaxNonces: 1 << 20}
	org := uuid.New()
	for i := 0; i < 5000; i++ {
		if err := v.record(org, fmt.Sprint("n", i), now); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(DefaultNonceTTL + time.Second)
	if err := v.record(org, "fresh", now); err != nil {
		t.Fatal(err)
	}
	if got := len(v.orgs[org].set); got != 1 {
		t.Fatalf("expired nonces not evicted from the front of the queue: %d left", got)
	}
	// Idle organizations are dropped when the org table is full.
	v2 := &HMACCanonicalV1{Now: func() time.Time { return now }, MaxOrgs: 2}
	_ = v2.record(uuid.New(), "a", now)
	_ = v2.record(uuid.New(), "b", now)
	if err := v2.record(uuid.New(), "c", now); err == nil {
		t.Fatal("org table full of live orgs must fail closed")
	}
	now = now.Add(DefaultNonceTTL + time.Second)
	if err := v2.record(uuid.New(), "c", now); err != nil {
		t.Fatalf("idle orgs must be reclaimed: %v", err)
	}
}

// ------------------------------------------------------------------ A2 ---

func TestA2_ClientIPTrustedProxies(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	mk := func(remote, xff string) *http.Request {
		r := httptest.NewRequest("POST", "/", nil)
		r.RemoteAddr = remote
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		return r
	}
	cases := []struct {
		name, remote, xff string
		trusted           []netip.Prefix
		want              string
	}{
		{"no proxies: XFF ignored (forgeable)", "203.0.113.9:1234", "1.2.3.4", nil, "203.0.113.9"},
		{"untrusted peer cannot set XFF", "203.0.113.9:1234", "1.2.3.4", trusted, "203.0.113.9"},
		{"trusted proxy: client from XFF", "10.0.0.5:80", "198.51.100.7", trusted, "198.51.100.7"},
		{"spoofed left entry ignored", "10.0.0.5:80", "6.6.6.6, 198.51.100.7", trusted, "198.51.100.7"},
		{"chain of proxies", "10.0.0.5:80", "198.51.100.7, 10.0.0.9", trusted, "198.51.100.7"},
		{"malformed chain falls back to peer", "10.0.0.5:80", "garbage", trusted, "10.0.0.5"},
		{"no XFF from proxy", "10.0.0.5:80", "", trusted, "10.0.0.5"},
	}
	for _, c := range cases {
		if got := ClientIPFrom(mk(c.remote, c.xff), c.trusted); got != c.want {
			t.Errorf("%s: got %s want %s", c.name, got, c.want)
		}
	}
}

func TestA2_IPv6GroupedBy64(t *testing.T) {
	a := LimiterKeyForIP("2001:db8:1:2:aaaa:bbbb:cccc:dddd")
	b := LimiterKeyForIP("2001:db8:1:2:1111:2222:3333:4444")
	c := LimiterKeyForIP("2001:db8:1:3::1")
	if a != b || a == c {
		t.Fatalf("a=%s b=%s c=%s", a, b, c)
	}
	if LimiterKeyForIP("192.0.2.1") != "192.0.2.1" || LimiterKeyForIP("::ffff:192.0.2.1") != "192.0.2.1" {
		t.Fatal("ipv4 / mapped")
	}
}

func TestA2_PreAuthLimiterHonorsTrustedProxyAndSurvivesFullTable(t *testing.T) {
	e := newEnv(t, func(c *Config) {
		c.PreAuthLimiter = &TokenBucket{Rate: 0.0001, Burst: 1, MaxKeys: 2}
		c.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}
	})
	post := func(xff string) int {
		code, _ := e.send(t, e.orgA, "stock.changed", stockBody("p"+xff, "S", 1), func(r *http.Request) {
			r.Header.Set("X-Forwarded-For", xff)
		})
		return code
	}
	// Distinct real clients behind the proxy each get their own bucket, even
	// with the table (2) already full: a new client is never refused for that.
	for _, ip := range []string{"198.51.100.1", "198.51.100.2", "198.51.100.3", "198.51.100.4"} {
		if c := post(ip); c != 200 {
			t.Fatalf("%s: %d", ip, c)
		}
	}
	// 198.51.100.4 (most recent) is now empty.
	if c := post("198.51.100.4"); c != 429 {
		t.Fatalf("same client must be limited: %d", c)
	}
}

// ------------------------------------------------------------------ A3 ---

type fakeDyn struct {
	mu      sync.Mutex
	cols    []string
	rows    []map[string]any
	failNew error // Create returns this once (and, when raceRow != nil, inserts raceRow first)
	raceRow map[string]any
	creates int32
	updates int32
	deletes int32
	// noSoftDelete makes the model report no deleted_at column.
	noSoftDelete bool
}

func (f *fakeDyn) TableMetadata(context.Context, string) (*modelbase.TableMetadata, error) {
	m := &modelbase.TableMetadata{}
	for _, c := range f.cols {
		m.Columns = append(m.Columns, modelbase.ColumnDef{Key: c})
	}
	if !f.noSoftDelete {
		m.Audit = &modelbase.AuditMeta{DeletedAt: "deleted_at"}
	}
	return m, nil
}

func (f *fakeDyn) List(_ context.Context, _ string, u modelbase.AuthUser, p query.Params) ([]map[string]any, query.PageMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	known := map[string]bool{}
	for _, c := range f.cols {
		known[c] = true
	}
	var out []map[string]any
	for _, r := range f.rows {
		if r["organization_id"] != u.GetOrganizationID().String() || r["deleted_at"] != nil {
			continue
		}
		ok := true
		for k, flt := range p.Filters {
			if !known[k] { // like the real builder: unknown columns are dropped
				continue
			}
			if fmt.Sprint(r[k]) != fmt.Sprint(flt.Value) {
				ok = false
			}
		}
		if ok {
			cp := map[string]any{}
			for k, v := range r {
				cp[k] = v
			}
			out = append(out, cp)
		}
	}
	return out, query.PageMeta{}, nil
}

func (f *fakeDyn) Create(_ context.Context, _ string, u modelbase.AuthUser, in map[string]any) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	atomic.AddInt32(&f.creates, 1)
	if f.failNew != nil {
		e := f.failNew
		f.failNew = nil
		if f.raceRow != nil {
			f.rows = append(f.rows, f.raceRow)
		}
		return nil, e
	}
	row := map[string]any{"id": uuid.NewString(), "organization_id": u.GetOrganizationID().String()}
	for k, v := range in {
		row[k] = v
	}
	f.rows = append(f.rows, row)
	return row, nil
}

func (f *fakeDyn) Update(_ context.Context, _ string, u modelbase.AuthUser, id uuid.UUID, in map[string]any) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	atomic.AddInt32(&f.updates, 1)
	for _, r := range f.rows {
		if r["id"] == id.String() && r["organization_id"] == u.GetOrganizationID().String() {
			for k, v := range in {
				r[k] = v
			}
			return r, nil
		}
	}
	return nil, dynamic.ErrRecordNotFound
}

// Delete soft-deletes like dynamic.Service: tenant scoped, a tombstone hides
// the row from List.
func (f *fakeDyn) Delete(_ context.Context, _ string, u modelbase.AuthUser, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	atomic.AddInt32(&f.deletes, 1)
	for _, r := range f.rows {
		if r["id"] == id.String() && r["organization_id"] == u.GetOrganizationID().String() && r["deleted_at"] == nil {
			r["deleted_at"] = "now"
			return nil
		}
	}
	return dynamic.ErrRecordNotFound
}

func newFakeDyn() *fakeDyn {
	return &fakeDyn{cols: []string{"sku", "warehouse", "quantity", "branch_id"}}
}

func scopedOp(org uuid.UUID, entity, branch string) Operation {
	op := Operation{Org: org, Entity: entity, Branch: branch, RequireEntity: true, RequireBranch: true,
		Route:  Route{Model: "inv", Op: OpUpsert, Match: []string{"sku"}},
		Values: map[string]any{"sku": "S1", "quantity": int64(1)}}
	if branch != "" {
		op.Values["branch_id"] = branch
	}
	return op
}

func TestA3_ScopeFailsClosed(t *testing.T) {
	org := uuid.New()
	restricted := dynamic.EntityScope{EntityID: "ent-1", Restricted: true, BranchIDs: []string{"br-1"}}
	resolver := func(ok bool) func(context.Context, uuid.UUID, string) (dynamic.EntityScope, bool) {
		return func(context.Context, uuid.UUID, string) (dynamic.EntityScope, bool) { return restricted, ok }
	}
	kind := func(err error) (Kind, string) {
		var ee *ExecError
		if !errors.As(err, &ee) {
			t.Fatalf("not ExecError: %v", err)
		}
		return ee.Kind, ee.Code
	}
	// 1. payload omitted the entity or branch -> refused (was: fail open).
	d := &DynamicExecutor{store: newFakeDyn(), EntityScope: resolver(true)}
	for _, op := range []Operation{scopedOp(org, "", "br-1"), scopedOp(org, "ent-1", "")} {
		_, err := d.Execute(context.Background(), op)
		if k, c := kind(err); k != KindForbidden || c != "scope_required" {
			t.Errorf("omitted scope: %v %s", k, c)
		}
	}
	// 2. entity named but resolver says "unknown" -> 403, not open.
	d = &DynamicExecutor{store: newFakeDyn(), EntityScope: resolver(false)}
	if _, err := d.Execute(context.Background(), scopedOp(org, "ent-1", "br-1")); err == nil {
		t.Error("ok=false must refuse")
	} else if k, c := kind(err); k != KindForbidden || c != "scope_unresolved" {
		t.Errorf("unresolved: %v %s", k, c)
	}
	// 3. entity named but no resolver configured -> internal (misconfigured), never open.
	d = &DynamicExecutor{store: newFakeDyn()}
	if _, err := d.Execute(context.Background(), scopedOp(org, "ent-1", "br-1")); err == nil {
		t.Error("missing resolver must refuse")
	} else if k, _ := kind(err); k != KindInternal {
		t.Errorf("no resolver: %v", k)
	}
	// 4. branch outside the entity's branches -> 403.
	d = &DynamicExecutor{store: newFakeDyn(), EntityScope: resolver(true)}
	if _, err := d.Execute(context.Background(), scopedOp(org, "ent-1", "br-9")); err == nil {
		t.Error("out-of-scope branch accepted")
	} else if k, c := kind(err); k != KindForbidden || c != "scope_forbidden" {
		t.Errorf("branch: %v %s", k, c)
	}
	// 5. an explicit branch_id mapped into the values is checked too.
	op := scopedOp(org, "ent-1", "br-1")
	op.Values["branch_id"] = "br-9"
	if _, err := d.Execute(context.Background(), op); err == nil {
		t.Error("values.branch_id out of scope accepted")
	}
}

func TestA3_UpsertNeverTouchesAnotherEntitysRow(t *testing.T) {
	org := uuid.New()
	fd := newFakeDyn()
	fd.rows = []map[string]any{
		{"id": uuid.NewString(), "organization_id": org.String(), "sku": "S1", "branch_id": "br-OTHER-ENTITY", "quantity": int64(99)},
	}
	other := fd.rows[0]
	d := &DynamicExecutor{store: fd, EntityScope: func(context.Context, uuid.UUID, string) (dynamic.EntityScope, bool) {
		return dynamic.EntityScope{EntityID: "ent-1", Restricted: true, BranchIDs: []string{"br-1"}}, true
	}}
	out, err := d.Execute(context.Background(), scopedOp(org, "ent-1", "br-1"))
	if err != nil || !out.Created {
		t.Fatalf("must create its own row, got %+v %v", out, err)
	}
	if other["quantity"] != int64(99) || fd.updates != 0 {
		t.Fatalf("another entity's row was modified: %v updates=%d", other, fd.updates)
	}
	// Its own row (in scope) is updated in place.
	out2, err := d.Execute(context.Background(), scopedOp(org, "ent-1", "br-1"))
	if err != nil || out2.Created || out2.RecordID != out.RecordID {
		t.Fatalf("own row must be updated: %+v %v", out2, err)
	}
}

func TestA3_HandlerRequiresDeclaredScope(t *testing.T) {
	reg := NewRegistry()
	c := stockContract()
	c.Route.Scope = &ScopeMapping{Branch: "branch", Entity: "entity"}
	c.JSONSchema = json.RawMessage(`{"type":"object","required":["event_id"]}`)
	_ = reg.Register(c)
	e := newEnv(t, func(cfg *Config) { cfg.Contracts = reg })
	for name, b := range map[string]string{
		"no branch": `{"event_id":"a","sku":"S","warehouse":"w","quantity":1,"entity":"e1"}`,
		"no entity": `{"event_id":"a","sku":"S","warehouse":"w","quantity":1,"branch":"b1"}`,
	} {
		code, out := e.send(t, e.orgA, "stock.changed", b, nil)
		if code != 422 || errCode(out) != CodeScopeRequired {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
	if e.exec.calls != 0 {
		t.Fatal("event without scope reached the executor")
	}
}

// ------------------------------------------------------------------ M1 ---

func TestM1_ConcurrentUpsertsSerialize(t *testing.T) {
	org := uuid.New()
	fd := newFakeDyn()
	d := &DynamicExecutor{store: fd}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := d.Execute(context.Background(), Operation{Org: org,
				Route:  Route{Model: "inv", Op: OpUpsert, Match: []string{"sku"}},
				Values: map[string]any{"sku": "SAME", "quantity": int64(1)}})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := len(fd.rows); n != 1 || fd.creates != 1 {
		t.Fatalf("rows=%d creates=%d, want a single row", n, fd.creates)
	}
}

func TestM1_LostCrossReplicaRaceFallsBackToUpdate(t *testing.T) {
	org := uuid.New()
	fd := newFakeDyn()
	winner := map[string]any{"id": uuid.NewString(), "organization_id": org.String(), "sku": "S1", "quantity": int64(1)}
	fd.failNew = errors.New("UNIQUE constraint failed: inv.sku")
	fd.raceRow = winner
	d := &DynamicExecutor{store: fd}
	out, err := d.Execute(context.Background(), Operation{Org: org,
		Route:  Route{Model: "inv", Op: OpUpsert, Match: []string{"sku"}},
		Values: map[string]any{"sku": "S1", "quantity": int64(5)}})
	if err != nil || out.Created || out.RecordID != winner["id"] || winner["quantity"] != int64(5) {
		t.Fatalf("%+v %v %v", out, err, winner)
	}
	// A create failure with no winner is reported, not swallowed.
	fd2 := newFakeDyn()
	fd2.failNew = errors.New("boom")
	if _, err := (&DynamicExecutor{store: fd2}).Execute(context.Background(), Operation{Org: org,
		Route:  Route{Model: "inv", Op: OpUpsert, Match: []string{"sku"}},
		Values: map[string]any{"sku": "S2"}}); err == nil {
		t.Fatal("create error swallowed")
	}
}

// ------------------------------------------------------------------ M2 ---

func TestM2_MatchColumnValidatedAtRegistrationWithEmptyTable(t *testing.T) {
	fd := newFakeDyn() // EMPTY table: the runtime guard cannot fire here
	reg := NewRegistry()
	c := stockContract()
	c.Route.Match = []string{"sku", "bin"}
	c.Route.Mapping["bin"] = "bin"
	_ = reg.Register(c)
	_, err := New(Config{Contracts: reg, Receipts: NewMemoryReceiptStore(), Executor: &DynamicExecutor{store: fd},
		Verifiers: map[string]Verifier{ProfileHMACCanonicalV1: VerifierFunc(func(context.Context, VerifyRequest) error { return nil })}})
	if !errors.Is(err, ErrInvalidContract) || !strings.Contains(err.Error(), `"bin"`) {
		t.Fatalf("want stable ErrInvalidContract naming the field, got %v", err)
	}
	// RegisterContract refuses it and does NOT register it.
	h, err := New(Config{Contracts: NewRegistry(), Receipts: NewMemoryReceiptStore(), Executor: &DynamicExecutor{store: fd},
		Verifiers: map[string]Verifier{ProfileHMACCanonicalV1: VerifierFunc(func(context.Context, VerifyRequest) error { return nil })}})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.RegisterContract(context.Background(), c); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("%v", err)
	}
	if len(h.cfg.Contracts.Keys()) != 0 {
		t.Fatal("rejected contract was registered")
	}
	good := stockContract()
	if err := h.RegisterContract(context.Background(), good); err != nil {
		t.Fatalf("valid contract: %v", err)
	}
	if len(h.cfg.Contracts.Keys()) != 1 {
		t.Fatal("valid contract not registered")
	}
}

func TestM2_TypedComparison(t *testing.T) {
	for _, c := range []struct {
		a, b any
		want bool
	}{
		{int64(5), 5.0, true},
		{float64(5), int64(5), true},
		{"5", int64(5), false}, // "5" the string is not 5 the number
		{"a", "a", true},
		{"a", "A", false},
		{true, true, true},
		{true, "true", false},
		{nil, nil, false},
	} {
		if got := valuesEqual(c.a, c.b); got != c.want {
			t.Errorf("valuesEqual(%#v,%#v)=%v want %v", c.a, c.b, got, c.want)
		}
	}
	// Runtime guard: rows come back but a non-column filter was dropped.
	fd := newFakeDyn()
	org := uuid.New()
	fd.rows = []map[string]any{{"id": uuid.NewString(), "organization_id": org.String(), "sku": "A", "bin": "B1"}}
	_, err := (&DynamicExecutor{store: fd}).Execute(context.Background(), Operation{Org: org,
		Route:  Route{Model: "inv", Op: OpUpsert, Match: []string{"bin"}},
		Values: map[string]any{"bin": "B2"}})
	var ee *ExecError
	if !errors.As(err, &ee) || ee.Kind != KindInternal {
		t.Fatalf("%v", err)
	}
}

// ------------------------------------------------------------------ M4 ---

func TestM4_FinishIsFencedByAttempt(t *testing.T) {
	st := NewMemoryReceiptStore()
	now := time.Unix(1_800_000_000, 0)
	st.Now = func() time.Time { return now }
	org := uuid.New()
	r := Receipt{OrgID: org, Contract: "c", Version: 1, KeyHash: HashKey(org, "c", "k"), BodyHash: "h"}
	slow, claimed, _ := st.Begin(context.Background(), r, time.Minute)
	if !claimed || slow.Attempts != 1 {
		t.Fatal("setup")
	}
	now = now.Add(2 * time.Minute) // the slow worker's lease expires
	fast, claimed, _ := st.Begin(context.Background(), r, time.Minute)
	if !claimed || fast.Attempts != 2 || fast.ID != slow.ID {
		t.Fatalf("reclaim: %+v %v", fast, claimed)
	}
	if ok, _ := st.FinishAttempt(context.Background(), fast.ID, 2, StatusApplied, "rec-fast", ""); !ok {
		t.Fatal("owner must finish")
	}
	// The slow worker wakes up and tries to mark the event failed/other record.
	if ok, _ := st.FinishAttempt(context.Background(), slow.ID, 1, StatusFailed, "", "internal"); ok {
		t.Fatal("stale attempt overwrote the owner")
	}
	got, _ := st.List(context.Background(), org, ReceiptFilter{})
	if got[0].Status != StatusApplied || got[0].RecordID != "rec-fast" {
		t.Fatalf("%+v", got[0])
	}
}

type fenceSpy struct {
	*MemoryReceiptStore
	fenced, plain int32
}

func (f *fenceSpy) Finish(ctx context.Context, id uuid.UUID, s, rec, code string) error {
	atomic.AddInt32(&f.plain, 1)
	return f.MemoryReceiptStore.Finish(ctx, id, s, rec, code)
}
func (f *fenceSpy) FinishAttempt(ctx context.Context, id uuid.UUID, a int, s, rec, code string) (bool, error) {
	atomic.AddInt32(&f.fenced, 1)
	return f.MemoryReceiptStore.FinishAttempt(ctx, id, a, s, rec, code)
}

func TestM4_HandlerUsesFencedFinish(t *testing.T) {
	spy := &fenceSpy{MemoryReceiptStore: NewMemoryReceiptStore()}
	e := newEnv(t, func(c *Config) { c.Receipts = spy })
	e.send(t, e.orgA, "stock.changed", stockBody("f1", "S", 1), nil)
	e.exec.fail = errors.New("x")
	e.send(t, e.orgA, "stock.changed", stockBody("f2", "S", 1), nil)
	if spy.fenced != 2 || spy.plain != 0 {
		t.Fatalf("fenced=%d plain=%d", spy.fenced, spy.plain)
	}
}

// ------------------------------------------------------------------ M7 ---

func TestM7_RejectedRowsAreCappedPerOrganization(t *testing.T) {
	st := NewMemoryReceiptStore()
	st.MaxRejected = 3
	e := newEnv(t, func(c *Config) { c.Receipts = st })
	for i := 0; i < 10; i++ {
		if c, _ := e.send(t, e.orgA, "stock.changed", fmt.Sprintf(`{"event_id":"x%d"}`, i), nil); c != 422 {
			t.Fatal(c)
		}
	}
	e.send(t, e.orgB, "stock.changed", `{"event_id":"b"}`, nil)
	ra, _ := st.List(context.Background(), e.orgA, ReceiptFilter{Status: StatusRejected})
	rb, _ := st.List(context.Background(), e.orgB, ReceiptFilter{Status: StatusRejected})
	if len(ra) != 3 || len(rb) != 1 {
		t.Fatalf("A=%d B=%d", len(ra), len(rb))
	}
	// Applied/failed receipts are never pruned by rejects.
	e.send(t, e.orgA, "stock.changed", stockBody("ok", "S", 1), nil)
	for i := 0; i < 5; i++ {
		e.send(t, e.orgA, "stock.changed", fmt.Sprintf(`{"event_id":"y%d"}`, i), nil)
	}
	if ap, _ := st.List(context.Background(), e.orgA, ReceiptFilter{Status: StatusApplied}); len(ap) != 1 {
		t.Fatalf("applied rows pruned: %d", len(ap))
	}
}

// ------------------------------------------------------------- L1 L2 L4 L3 ---

func TestL1_ClaimColumnFitsMaxKey(t *testing.T) {
	org := uuid.New()
	contract := strings.Repeat("a", 128) // keyRe maximum
	r := Receipt{OrgID: org, Contract: contract, KeyHash: HashKey(org, contract, "k")}
	f, _ := reflect.TypeOf(Receipt{}).FieldByName("Claim")
	tag := f.Tag.Get("gorm")
	var size int
	for _, p := range strings.Split(tag, ";") {
		if s, ok := strings.CutPrefix(p, "size:"); ok {
			size, _ = strconv.Atoi(s)
		}
	}
	if n := len(claimOf(r)); n > size {
		t.Fatalf("claim %d bytes does not fit size:%d", n, size)
	}
	// The registry accepts a 128-char key and refuses 129.
	c := stockContract()
	c.Key = contract
	if err := NewRegistry().Register(c); err != nil {
		t.Fatal(err)
	}
	c.Key += "a"
	if err := NewRegistry().Register(c); !errors.Is(err, ErrInvalidContract) {
		t.Fatal("129-char key accepted")
	}
}

func TestL2_EventKeyFilterNeedsContractEverywhere(t *testing.T) {
	st := NewMemoryReceiptStore()
	if _, err := st.List(context.Background(), uuid.New(), ReceiptFilter{EventKey: "k"}); !errors.Is(err, ErrEventKeyNeedsContract) {
		t.Fatalf("%v", err)
	}
}

func TestL4_NumbersOutOfRangeAre422(t *testing.T) {
	e := newEnv(t, nil)
	for _, q := range []string{"1e999", "-1e999", "123456789012345678901234567890e999"} {
		b := `{"event_id":"n1","sku":"S","warehouse":"w","quantity":` + q + `}`
		code, out := e.send(t, e.orgA, "stock.changed", b, nil)
		if code != 422 || errCode(out) != CodeInvalidSchema {
			t.Errorf("%s: %d %v", q, code, out)
		}
	}
	if e.exec.calls != 0 {
		t.Fatal("reached the executor")
	}
	// Nested too.
	if _, err := decodeJSON([]byte(`{"a":[1,{"b":1e999}]}`)); !errors.Is(err, errNumberRange) {
		t.Fatalf("%v", err)
	}
	// Big but representable numbers are fine.
	if _, err := decodeJSON([]byte(`{"a":12345678901234567890,"b":1.5e300}`)); err != nil {
		t.Fatal(err)
	}
}

func TestL3_UnknownContractDoesNotLeakToUnauthenticated(t *testing.T) {
	e := newEnv(t, nil)
	unsigned := func(r *http.Request) { r.Header.Del("X-Metacore-Signature") }
	for _, ct := range []string{"nope.unknown", "stock.changed"} {
		code, out := e.send(t, e.orgA, ct, `{}`, unsigned)
		if code != 401 || errCode(out) != CodeUnauthorized {
			t.Errorf("%s unsigned: %d %v", ct, code, out)
		}
	}
	// Known and unknown contracts are indistinguishable without a signature,
	// including an unknown version.
	c1, o1 := e.send(t, e.orgA, "stock.changed", `{}`, func(r *http.Request) { r.Header.Del("X-Metacore-Signature"); r.Header.Set("X-Contract-Version", "7") })
	c2, o2 := e.send(t, e.orgA, "nope.unknown", `{}`, unsigned)
	if c1 != c2 || !reflect.DeepEqual(o1, o2) {
		t.Fatalf("distinguishable: %d %v vs %d %v", c1, o1, c2, o2)
	}
	// A signed caller does learn the contract is unknown.
	if code, out := e.send(t, e.orgA, "nope.unknown", `{}`, nil); code != 404 || errCode(out) != CodeUnknownContract {
		t.Fatalf("%d %v", code, out)
	}
	// Without DefaultAuth, unknown contracts are always 401.
	e2 := newEnv(t, func(c *Config) { c.DefaultAuth = "" })
	if code, _ := e2.send(t, e2.orgA, "nope.unknown", `{}`, nil); code != 401 {
		t.Fatalf("no DefaultAuth: %d", code)
	}
}

func TestHardeningJSONRoundTrip(t *testing.T) {
	// Contracts keep loading from JSON with the new scope semantics.
	reg := NewRegistry()
	data := `{"key":"a.b","version":1,"json_schema":{"type":"object"},
	 "route":{"model":"m","op":"create","mapping":{"x":"x"},"scope":{"branch":"b","entity":"e"}},
	 "idempotency":"{id}","auth":"p"}`
	if err := reg.LoadJSON([]byte(data)); err != nil {
		t.Fatal(err)
	}
	c, _ := reg.get("a.b", 0)
	b, _ := json.Marshal(c.Route.Scope)
	if string(b) != `{"branch":"b","entity":"e"}` {
		t.Fatal(string(b))
	}
}
