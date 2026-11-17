package inbound

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// --------------------------------------------------------------- fixtures ---

const stockSchema = `{
  "type":"object",
  "required":["event_id","sku","warehouse","quantity"],
  "additionalProperties":false,
  "properties":{
    "event_id":{"type":"string","minLength":1},
    "sku":{"type":"string","minLength":1},
    "warehouse":{"type":"string"},
    "quantity":{"type":"integer","minimum":0},
    "branch":{"type":"string"},
    "note":{"type":"string"}
  }}`

func stockContract() Contract {
	return Contract{
		Key: "stock.changed", Version: 1, JSONSchema: json.RawMessage(stockSchema),
		Route: Route{
			Model: "inventory", Op: OpUpsert,
			Mapping:  map[string]string{"sku": "sku", "warehouse": "warehouse", "quantity": "quantity"},
			Defaults: map[string]any{"source": "inbound"},
			Match:    []string{"sku", "warehouse"},
			Scope:    &ScopeMapping{Branch: "branch"},
		},
		Idempotency: "{event_id}",
		Auth:        ProfileHMACCanonicalV1,
	}
}

// fakeExec is an in-memory executor that, like dynamic, is partitioned by org.
type fakeExec struct {
	mu    sync.Mutex
	rows  map[uuid.UUID][]map[string]any
	calls int32
	fail  error // returned once when set
	delay time.Duration
	last  Operation
}

func newFakeExec() *fakeExec { return &fakeExec{rows: map[uuid.UUID][]map[string]any{}} }

func (f *fakeExec) Execute(ctx context.Context, op Operation) (Outcome, error) {
	atomic.AddInt32(&f.calls, 1)
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.last = op
	if f.fail != nil {
		e := f.fail
		f.fail = nil
		return Outcome{}, e
	}
	for _, r := range f.rows[op.Org] {
		match := true
		for _, m := range op.Route.Match {
			if fmt.Sprint(r[m]) != fmt.Sprint(op.Values[m]) {
				match = false
			}
		}
		if match {
			for k, v := range op.Values {
				r[k] = v
			}
			return Outcome{RecordID: r["id"].(string)}, nil
		}
	}
	row := map[string]any{"id": uuid.NewString()}
	for k, v := range op.Values {
		row[k] = v
	}
	f.rows[op.Org] = append(f.rows[op.Org], row)
	return Outcome{RecordID: row["id"].(string), Created: true}, nil
}

type env struct {
	h       *Handler
	srv     *httptest.Server
	exec    *fakeExec
	store   *MemoryReceiptStore
	secrets map[uuid.UUID][]byte
	now     time.Time
	orgA    uuid.UUID
	orgB    uuid.UUID
}

func newEnv(t *testing.T, mut func(*Config)) *env {
	t.Helper()
	e := &env{exec: newFakeExec(), store: NewMemoryReceiptStore(), now: time.Unix(1_800_000_000, 0),
		orgA: uuid.New(), orgB: uuid.New()}
	e.secrets = map[uuid.UUID][]byte{e.orgA: []byte("secret-A"), e.orgB: []byte("secret-B")}
	reg := NewRegistry()
	if err := reg.Register(stockContract()); err != nil {
		t.Fatal(err)
	}
	v := &HMACCanonicalV1{
		Secrets: func(_ context.Context, org uuid.UUID) ([][]byte, error) {
			s, ok := e.secrets[org]
			if !ok {
				return nil, errors.New("unknown org")
			}
			return [][]byte{s}, nil
		},
		Now: func() time.Time { return e.now },
	}
	cfg := Config{Contracts: reg, Verifiers: map[string]Verifier{ProfileHMACCanonicalV1: v},
		Receipts: e.store, Executor: e.exec, Logger: quietLogger()}
	if mut != nil {
		mut(&cfg)
	}
	h, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.h = h
	e.srv = httptest.NewServer(h)
	t.Cleanup(e.srv.Close)
	return e
}

var nonceSeq int64

// send signs (unless sig overrides) and posts a body.
func (e *env) send(t *testing.T, org uuid.UUID, contract string, body string, tweak func(*http.Request)) (int, map[string]any) {
	t.Helper()
	path := "/api/inbound/" + org.String() + "/" + contract
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+path, strings.NewReader(body))
	ts := strconv.FormatInt(e.now.Unix(), 10)
	nonce := "n-" + strconv.FormatInt(atomic.AddInt64(&nonceSeq, 1), 10)
	sec := e.secrets[org]
	if sec == nil {
		sec = []byte("x")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Metacore-Timestamp", ts)
	req.Header.Set("X-Metacore-Nonce", nonce)
	req.Header.Set("X-Metacore-Signature", "sha256="+SignCanonicalV1(sec, ts, nonce, "POST", path, []byte(body)))
	if tweak != nil {
		tweak(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func stockBody(id, sku string, qty int) string {
	return fmt.Sprintf(`{"event_id":%q,"sku":%q,"warehouse":"MX1","quantity":%d}`, id, sku, qty)
}

func errCode(m map[string]any) string {
	e, _ := m["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

// ------------------------------------------------------------------ tests ---

func TestStockChangedUpsertEndToEnd(t *testing.T) {
	e := newEnv(t, nil)

	code, out := e.send(t, e.orgA, "stock.changed", stockBody("ev-1", "SKU-1", 10), nil)
	if code != 200 || out["status"] != "applied" || out["created"] != true {
		t.Fatalf("first: %d %v", code, out)
	}
	if got := e.exec.last.Values["source"]; got != "inbound" {
		t.Fatalf("defaults not applied: %v", e.exec.last.Values)
	}
	if v := e.exec.last.Values["quantity"]; v != int64(10) {
		t.Fatalf("quantity normalized to %T %v", v, v)
	}

	// New event, same SKU: updates in place (upsert), no second row.
	code, out = e.send(t, e.orgA, "stock.changed", stockBody("ev-2", "SKU-1", 7), nil)
	if code != 200 || out["status"] != "applied" || out["created"] == true {
		t.Fatalf("second: %d %v", code, out)
	}
	if n := len(e.exec.rows[e.orgA]); n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
	if q := e.exec.rows[e.orgA][0]["quantity"]; q != int64(7) {
		t.Fatalf("quantity = %v", q)
	}
}

func TestDuplicateEventIsReplayedWithoutEffect(t *testing.T) {
	e := newEnv(t, nil)
	body := stockBody("ev-dup", "SKU-9", 3)
	_, first := e.send(t, e.orgA, "stock.changed", body, nil)
	calls := atomic.LoadInt32(&e.exec.calls)

	code, out := e.send(t, e.orgA, "stock.changed", body, nil)
	if code != 200 || out["status"] != "replayed" {
		t.Fatalf("dup: %d %v", code, out)
	}
	if out["record_id"] != first["record_id"] || out["receipt_id"] != first["receipt_id"] {
		t.Fatalf("replay must point at the original: %v vs %v", out, first)
	}
	if atomic.LoadInt32(&e.exec.calls) != calls {
		t.Fatal("duplicate repeated the effect")
	}
}

func TestSameKeyDifferentBodyConflicts(t *testing.T) {
	e := newEnv(t, nil)
	e.send(t, e.orgA, "stock.changed", stockBody("ev-c", "SKU-1", 1), nil)
	code, out := e.send(t, e.orgA, "stock.changed", stockBody("ev-c", "SKU-1", 2), nil)
	if code != 409 || errCode(out) != CodeIdempotencyClash {
		t.Fatalf("got %d %v", code, out)
	}
	if e.exec.rows[e.orgA][0]["quantity"] != int64(1) {
		t.Fatal("conflicting event was applied")
	}
}

func TestBadSignatureIs401AndNoEffect(t *testing.T) {
	e := newEnv(t, nil)
	cases := map[string]func(*http.Request){
		"tampered signature": func(r *http.Request) { r.Header.Set("X-Metacore-Signature", "sha256="+strings.Repeat("0", 64)) },
		"missing signature":  func(r *http.Request) { r.Header.Del("X-Metacore-Signature") },
		"missing nonce":      func(r *http.Request) { r.Header.Del("X-Metacore-Nonce") },
		"stale timestamp": func(r *http.Request) {
			r.Header.Set("X-Metacore-Timestamp", strconv.FormatInt(e.now.Unix()-301, 10))
		},
		"future timestamp": func(r *http.Request) {
			r.Header.Set("X-Metacore-Timestamp", strconv.FormatInt(e.now.Unix()+301, 10))
		},
		"non-hex signature": func(r *http.Request) { r.Header.Set("X-Metacore-Signature", "zz") },
	}
	for name, tw := range cases {
		code, out := e.send(t, e.orgA, "stock.changed", stockBody("ev-"+name, "S", 1), tw)
		if code != 401 || errCode(out) != CodeUnauthorized {
			t.Errorf("%s: got %d %v", name, code, out)
		}
	}
	// Body tampered after signing.
	path := "/api/inbound/" + e.orgA.String() + "/stock.changed"
	ts := strconv.FormatInt(e.now.Unix(), 10)
	sig := SignCanonicalV1(e.secrets[e.orgA], ts, "nz", "POST", path, []byte(stockBody("a", "S", 1)))
	req, _ := http.NewRequest("POST", e.srv.URL+path, strings.NewReader(stockBody("a", "S", 999)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Metacore-Timestamp", ts)
	req.Header.Set("X-Metacore-Nonce", "nz")
	req.Header.Set("X-Metacore-Signature", sig)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 401 {
		t.Errorf("tampered body: %d", resp.StatusCode)
	}
	resp.Body.Close()

	if atomic.LoadInt32(&e.exec.calls) != 0 {
		t.Fatal("unauthenticated request reached the executor")
	}
	if rs, _ := e.store.List(context.Background(), e.orgA, ReceiptFilter{}); len(rs) != 0 {
		t.Fatalf("unauthenticated requests must not write the log, got %d", len(rs))
	}
}

func TestSkewWindowBoundaryAccepted(t *testing.T) {
	e := newEnv(t, nil)
	code, _ := e.send(t, e.orgA, "stock.changed", stockBody("ev-edge", "S", 1), func(r *http.Request) {
		ts := strconv.FormatInt(e.now.Unix()-300, 10)
		path := r.URL.Path
		r.Header.Set("X-Metacore-Timestamp", ts)
		r.Header.Set("X-Metacore-Signature", SignCanonicalV1(e.secrets[e.orgA], ts, r.Header.Get("X-Metacore-Nonce"), "POST", path, []byte(stockBody("ev-edge", "S", 1))))
	})
	if code != 200 {
		t.Fatalf("-300s must be accepted, got %d", code)
	}
}

func TestNonceReplayRejectedThenExpires(t *testing.T) {
	e := newEnv(t, nil)
	send := func(id string) int {
		code, _ := e.send(t, e.orgA, "stock.changed", stockBody(id, "S", 1), func(r *http.Request) {
			path := r.URL.Path
			ts := strconv.FormatInt(e.now.Unix(), 10)
			r.Header.Set("X-Metacore-Timestamp", ts)
			r.Header.Set("X-Metacore-Nonce", "fixed")
			r.Header.Set("X-Metacore-Signature", SignCanonicalV1(e.secrets[e.orgA], ts, "fixed", "POST", path, []byte(stockBody(id, "S", 1))))
		})
		return code
	}
	if c := send("n1"); c != 200 {
		t.Fatalf("first = %d", c)
	}
	if c := send("n2"); c != 401 {
		t.Fatalf("same nonce must be rejected, got %d", c)
	}
	e.now = e.now.Add(DefaultNonceTTL + time.Second)
	if c := send("n3"); c != 200 {
		t.Fatalf("after ttl nonce is free again, got %d", c)
	}
}

func TestInvalidSchemaIs422WithoutEchoingValues(t *testing.T) {
	e := newEnv(t, nil)
	bad := `{"event_id":"ev-x","sku":"secret-pii@example.com","warehouse":"MX1","quantity":-5,"extra":1}`
	code, out := e.send(t, e.orgA, "stock.changed", bad, nil)
	if code != 422 || errCode(out) != CodeInvalidSchema {
		t.Fatalf("got %d %v", code, out)
	}
	raw, _ := json.Marshal(out)
	if bytes.Contains(raw, []byte("pii@example.com")) {
		t.Fatalf("payload value echoed in error: %s", raw)
	}
	details := out["error"].(map[string]any)["details"].([]any)
	if len(details) == 0 {
		t.Fatal("expected details")
	}
	if atomic.LoadInt32(&e.exec.calls) != 0 {
		t.Fatal("invalid event reached executor")
	}
	rs, _ := e.store.List(context.Background(), e.orgA, ReceiptFilter{Status: StatusRejected})
	if len(rs) != 1 || rs[0].ErrorCode != CodeInvalidSchema {
		t.Fatalf("rejected receipt missing: %+v", rs)
	}
	// A rejected event does not reserve the key: the corrected one applies.
	code, _ = e.send(t, e.orgA, "stock.changed", stockBody("ev-x", "S", 1), nil)
	if code != 200 {
		t.Fatalf("corrected event: %d", code)
	}
}

func TestMissingRequiredAndWrongType(t *testing.T) {
	e := newEnv(t, nil)
	for name, b := range map[string]string{
		"missing": `{"event_id":"a","sku":"s"}`,
		"type":    `{"event_id":"a","sku":"s","warehouse":"w","quantity":"3"}`,
		"array":   `[1,2]`,
	} {
		if code, out := e.send(t, e.orgA, "stock.changed", b, nil); code != 422 {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
}

func TestOrganizationIsolation(t *testing.T) {
	e := newEnv(t, nil)

	// Same event id + SKU in two orgs: both apply, separate rows, no "replayed".
	body := stockBody("ev-shared", "SKU-1", 5)
	ca, oa := e.send(t, e.orgA, "stock.changed", body, nil)
	cb, ob := e.send(t, e.orgB, "stock.changed", stockBody("ev-shared", "SKU-1", 50), nil)
	if ca != 200 || cb != 200 || oa["status"] != "applied" || ob["status"] != "applied" {
		t.Fatalf("A=%d %v B=%d %v", ca, oa, cb, ob)
	}
	if len(e.exec.rows[e.orgA]) != 1 || len(e.exec.rows[e.orgB]) != 1 {
		t.Fatal("rows must be per org")
	}
	if e.exec.rows[e.orgA][0]["quantity"] != int64(5) || e.exec.rows[e.orgB][0]["quantity"] != int64(50) {
		t.Fatal("quantities crossed between orgs")
	}
	if e.exec.rows[e.orgA][0]["id"] == e.exec.rows[e.orgB][0]["id"] {
		t.Fatal("same record in both orgs")
	}

	// Org A's credentials cannot write into org B's URL.
	path := "/api/inbound/" + e.orgB.String() + "/stock.changed"
	ts := strconv.FormatInt(e.now.Unix(), 10)
	b := stockBody("ev-evil", "SKU-1", 0)
	req, _ := http.NewRequest("POST", e.srv.URL+path, strings.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Metacore-Timestamp", ts)
	req.Header.Set("X-Metacore-Nonce", "cross")
	req.Header.Set("X-Metacore-Signature", SignCanonicalV1(e.secrets[e.orgA], ts, "cross", "POST", path, []byte(b)))
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("cross-org write: %d", resp.StatusCode)
	}
	if e.exec.rows[e.orgB][0]["quantity"] != int64(50) {
		t.Fatal("org B data modified by org A")
	}

	// The log is per org.
	ra, _ := e.h.Receipts(context.Background(), e.orgA, ReceiptFilter{})
	rb, _ := e.h.Receipts(context.Background(), e.orgB, ReceiptFilter{})
	if len(ra) != 1 || len(rb) != 1 || ra[0].OrgID != e.orgA || rb[0].OrgID != e.orgB {
		t.Fatalf("receipt isolation: %d/%d", len(ra), len(rb))
	}
	// Nonces are scoped per org: the same nonce in B is not a replay of A's.
	if code, _ := e.send(t, e.orgB, "stock.changed", stockBody("ev-n", "S", 1), func(r *http.Request) {
		r.Header.Set("X-Metacore-Nonce", "shared-nonce")
		ts := strconv.FormatInt(e.now.Unix(), 10)
		r.Header.Set("X-Metacore-Signature", SignCanonicalV1(e.secrets[e.orgB], ts, "shared-nonce", "POST", r.URL.Path, []byte(stockBody("ev-n", "S", 1))))
	}); code != 200 {
		t.Fatalf("nonce scoped per org: %d", code)
	}
}

func TestUnknownOrgIs401NotLeaking(t *testing.T) {
	e := newEnv(t, nil)
	code, out := e.send(t, uuid.New(), "stock.changed", stockBody("a", "S", 1), nil)
	if code != 401 || errCode(out) != CodeUnauthorized {
		t.Fatalf("%d %v", code, out)
	}
}

func TestUniformErrors(t *testing.T) {
	e := newEnv(t, nil)
	if code, out := e.send(t, e.orgA, "nope.unknown", `{}`, nil); code != 404 || errCode(out) != CodeUnknownContract {
		t.Errorf("unknown contract: %d %v", code, out)
	}
	if code, out := e.send(t, e.orgA, "stock.changed", `{`, nil); code != 400 || errCode(out) != CodeMalformedJSON {
		t.Errorf("malformed: %d %v", code, out)
	}
	if code, out := e.send(t, e.orgA, "stock.changed", stockBody("a", "S", 1)+` {}`, nil); code != 400 {
		t.Errorf("trailing data: %d %v", code, out)
	}
	if code, out := e.send(t, e.orgA, "stock.changed", stockBody("a", "S", 1), func(r *http.Request) {
		r.Header.Set("Content-Type", "text/plain")
	}); code != 415 || errCode(out) != CodeUnsupportedMedia {
		t.Errorf("media type: %d %v", code, out)
	}
	if code, _ := e.send(t, e.orgA, "stock.changed", `{}`, func(r *http.Request) { r.Header.Set("X-Contract-Version", "9") }); code != 404 {
		t.Errorf("unknown version: %d", code)
	}
	resp, _ := http.Get(e.srv.URL + "/api/inbound/" + e.orgA.String() + "/stock.changed")
	if resp.StatusCode != 405 {
		t.Errorf("GET: %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp, _ = http.Post(e.srv.URL+"/api/inbound/not-a-uuid/stock.changed", "application/json", strings.NewReader("{}"))
	if resp.StatusCode != 404 {
		t.Errorf("bad org: %d", resp.StatusCode)
	}
	resp.Body.Close()
	// Every error is the same envelope.
	_, out := e.send(t, e.orgA, "nope.unknown", `{}`, nil)
	if _, ok := out["error"].(map[string]any)["message"]; !ok {
		t.Errorf("envelope: %v", out)
	}
}

func TestBodySizeLimit(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.MaxBodyBytes = 256 })
	big := `{"event_id":"a","sku":"` + strings.Repeat("x", 1000) + `","warehouse":"w","quantity":1}`
	code, out := e.send(t, e.orgA, "stock.changed", big, nil)
	if code != 413 || errCode(out) != CodeTooLarge {
		t.Fatalf("%d %v", code, out)
	}
	// Chunked (unknown length) bodies are cut off too.
	path := "/api/inbound/" + e.orgA.String() + "/stock.changed"
	req, _ := http.NewRequest("POST", e.srv.URL+path, strings.NewReader(big))
	req.ContentLength = -1
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 413 {
		t.Fatalf("chunked: %d", resp.StatusCode)
	}
}

func TestInternalErrorsAreNotLeaked(t *testing.T) {
	e := newEnv(t, nil)
	e.exec.fail = errors.New(`pq: duplicate key value violates unique constraint "x" DETAIL: Key (email)=(juan@example.com)`)
	code, out := e.send(t, e.orgA, "stock.changed", stockBody("ev-f", "S", 1), nil)
	if code != 500 || errCode(out) != CodeInternal {
		t.Fatalf("%d %v", code, out)
	}
	raw, _ := json.Marshal(out)
	if bytes.Contains(raw, []byte("juan@example.com")) || bytes.Contains(raw, []byte("pq:")) {
		t.Fatalf("internal error leaked: %s", raw)
	}
	rs, _ := e.store.List(context.Background(), e.orgA, ReceiptFilter{Status: StatusFailed})
	if len(rs) != 1 || rs[0].ErrorCode != "internal" {
		t.Fatalf("failed receipt: %+v", rs)
	}
	rawRec, _ := json.Marshal(rs)
	if bytes.Contains(rawRec, []byte("juan@example.com")) {
		t.Fatal("error text stored in the log")
	}
	// A failed event can be retried with the same key and then applies.
	code, out = e.send(t, e.orgA, "stock.changed", stockBody("ev-f", "S", 1), nil)
	if code != 200 || out["status"] != "applied" {
		t.Fatalf("retry after failure: %d %v", code, out)
	}
	rs, _ = e.store.List(context.Background(), e.orgA, ReceiptFilter{})
	if len(rs) != 1 || rs[0].Status != StatusApplied || rs[0].Attempts != 2 {
		t.Fatalf("receipt after retry: %+v", rs)
	}
}

func TestExecErrorsMapToStatuses(t *testing.T) {
	e := newEnv(t, nil)
	for i, tc := range []struct {
		err  error
		code int
		c    string
	}{
		{&ExecError{Kind: KindInvalid, Code: "rejected_by_model", Err: errors.New("secret detail")}, 422, "rejected_by_model"},
		{&ExecError{Kind: KindConflict, Code: "state_conflict"}, 409, "state_conflict"},
		{&ExecError{Kind: KindNotFound, Code: "record_not_found"}, 404, "record_not_found"},
		{&ExecError{Kind: KindForbidden, Code: "scope_forbidden"}, 403, "scope_forbidden"},
	} {
		e.exec.fail = tc.err
		code, out := e.send(t, e.orgA, "stock.changed", stockBody("ev-m"+strconv.Itoa(i), "S", 1), nil)
		if code != tc.code || errCode(out) != tc.c {
			t.Errorf("%v: %d %v", tc.err, code, out)
		}
		if raw, _ := json.Marshal(out); bytes.Contains(raw, []byte("secret detail")) {
			t.Errorf("leak: %s", raw)
		}
	}
}

func TestConcurrentDuplicatesApplyOnce(t *testing.T) {
	e := newEnv(t, nil)
	e.exec.delay = 30 * time.Millisecond
	body := stockBody("ev-par", "SKU-P", 4)
	var wg sync.WaitGroup
	var applied, other int32
	statuses := make(chan string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, out := e.send(t, e.orgA, "stock.changed", body, nil)
			s, _ := out["status"].(string)
			if code == 200 && s == "applied" {
				atomic.AddInt32(&applied, 1)
			} else {
				atomic.AddInt32(&other, 1)
				statuses <- fmt.Sprint(code, s, errCode(out))
			}
		}()
	}
	wg.Wait()
	close(statuses)
	if applied != 1 || atomic.LoadInt32(&e.exec.calls) != 1 {
		t.Fatalf("applied=%d calls=%d", applied, e.exec.calls)
	}
	for s := range statuses {
		if s != "200 replayed " && s != "409 "+CodeInProgress && !strings.HasPrefix(s, "409") {
			t.Errorf("unexpected loser answer %q", s)
		}
	}
}

func TestReceiptLogDoesNotKeepPersonalData(t *testing.T) {
	reg := NewRegistry()
	c := stockContract()
	c.Key = "customer.updated"
	c.Idempotency = "{event_id}:{note}"
	_ = reg.Register(c)
	e := newEnv(t, func(cfg *Config) { cfg.Contracts = reg })
	body := `{"event_id":"e1","sku":"S","warehouse":"w","quantity":1,"note":"maria@example.com"}`
	if code, out := e.send(t, e.orgA, "customer.updated", body, nil); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	rs, _ := e.h.Receipts(context.Background(), e.orgA, ReceiptFilter{})
	raw, _ := json.Marshal(rs)
	if bytes.Contains(raw, []byte("maria@example.com")) || rs[0].EventKey != "" || rs[0].KeyHash == "" {
		t.Fatalf("PII in the log: %s", raw)
	}
	// Still queryable by the clear key.
	got, _ := e.h.Receipts(context.Background(), e.orgA, ReceiptFilter{Contract: "customer.updated", EventKey: "e1:maria@example.com"})
	if len(got) != 1 {
		t.Fatalf("query by key: %d", len(got))
	}
	none, _ := e.h.Receipts(context.Background(), e.orgB, ReceiptFilter{Contract: "customer.updated", EventKey: "e1:maria@example.com"})
	if len(none) != 0 {
		t.Fatal("other org sees the receipt")
	}
}

func TestKeyInClearOptIn(t *testing.T) {
	reg := NewRegistry()
	c := stockContract()
	c.Log.KeyInClear = true
	_ = reg.Register(c)
	e := newEnv(t, func(cfg *Config) { cfg.Contracts = reg })
	e.send(t, e.orgA, "stock.changed", stockBody("opaque-1", "S", 1), nil)
	rs, _ := e.h.Receipts(context.Background(), e.orgA, ReceiptFilter{})
	if rs[0].EventKey != "opaque-1" {
		t.Fatalf("%+v", rs[0])
	}
}

func TestScopeIsPassedToExecutor(t *testing.T) {
	e := newEnv(t, nil)
	b := `{"event_id":"s1","sku":"S","warehouse":"w","quantity":1,"branch":"br-7"}`
	e.send(t, e.orgA, "stock.changed", b, nil)
	if e.exec.last.Branch != "br-7" || e.exec.last.Org != e.orgA {
		t.Fatalf("%+v", e.exec.last)
	}
}

func TestOrgComesFromRouteNotPayload(t *testing.T) {
	// organization_id in the payload is rejected by the schema (additionalProperties)
	// and can never be a mapping target.
	e := newEnv(t, nil)
	b := fmt.Sprintf(`{"event_id":"o1","sku":"S","warehouse":"w","quantity":1,"organization_id":%q}`, e.orgB)
	if code, _ := e.send(t, e.orgA, "stock.changed", b, nil); code != 422 {
		t.Fatalf("%d", code)
	}
	c := stockContract()
	c.Route.Mapping["organization_id"] = "org"
	if err := NewRegistry().Register(c); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("organization_id target must be refused: %v", err)
	}
}

func TestCommandRoute(t *testing.T) {
	reg := NewRegistry()
	c := stockContract()
	c.Key, c.Route = "order.status_changed", Route{Command: "orders.set_status", Op: OpUpdate,
		Mapping: map[string]string{"order": "order_ref", "status": "status"}}
	c.Idempotency = "{event_id}"
	c.JSONSchema = json.RawMessage(`{"type":"object","required":["event_id","order_ref","status"]}`)
	if err := reg.Register(c); err != nil {
		t.Fatal(err)
	}
	cmds := NewCommandRegistry()
	var got Operation
	cmds.Register("orders.set_status", func(_ context.Context, op Operation) (Outcome, error) {
		got = op
		return Outcome{RecordID: "ord-1"}, nil
	})
	e := newEnv(t, func(cfg *Config) { cfg.Contracts, cfg.Commands, cfg.Executor = reg, cmds, nil })
	code, out := e.send(t, e.orgA, "order.status_changed", `{"event_id":"c1","order_ref":"A-1","status":"shipped"}`, nil)
	if code != 200 || out["record_id"] != "ord-1" || got.Values["status"] != "shipped" || got.Org != e.orgA {
		t.Fatalf("%d %v %+v", code, out, got)
	}
}

func TestCommandPanicIs500(t *testing.T) {
	reg := NewRegistry()
	c := stockContract()
	c.Route = Route{Command: "boom", Op: OpCreate, Mapping: map[string]string{"sku": "sku"}}
	_ = reg.Register(c)
	cmds := NewCommandRegistry()
	cmds.Register("boom", func(context.Context, Operation) (Outcome, error) { panic("kaboom juan@example.com") })
	e := newEnv(t, func(cfg *Config) { cfg.Contracts, cfg.Commands = reg, cmds })
	code, out := e.send(t, e.orgA, "stock.changed", stockBody("p1", "S", 1), nil)
	if code != 500 || errCode(out) != CodeInternal {
		t.Fatalf("%d %v", code, out)
	}
	if raw, _ := json.Marshal(out); bytes.Contains(raw, []byte("juan")) {
		t.Fatal("panic text leaked")
	}
}

func TestNewRejectsBadWiring(t *testing.T) {
	reg := NewRegistry()
	_ = reg.Register(stockContract())
	st := NewMemoryReceiptStore()
	if _, err := New(Config{Contracts: reg, Receipts: st, Executor: newFakeExec()}); err == nil || !strings.Contains(err.Error(), "auth profile") {
		t.Errorf("missing verifier: %v", err)
	}
	v := map[string]Verifier{ProfileHMACCanonicalV1: VerifierFunc(func(context.Context, VerifyRequest) error { return nil })}
	if _, err := New(Config{Contracts: reg, Receipts: st, Verifiers: v}); err == nil || !strings.Contains(err.Error(), "Executor") {
		t.Errorf("missing executor: %v", err)
	}
	c := stockContract()
	c.Key, c.Route = "x.y", Route{Command: "nope", Op: OpCreate, Mapping: map[string]string{"a": "a"}}
	_ = reg.Register(c)
	if _, err := New(Config{Contracts: reg, Receipts: st, Verifiers: v, Executor: newFakeExec()}); err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Errorf("missing command: %v", err)
	}
	if _, err := New(Config{}); err == nil {
		t.Error("empty config accepted")
	}
}

func TestRateLimit(t *testing.T) {
	tb := &TokenBucket{Rate: 0.0001, Burst: 2}
	e := newEnv(t, func(c *Config) { c.Limiter = tb })
	var codes []int
	for i := 0; i < 4; i++ {
		code, _ := e.send(t, e.orgA, "stock.changed", stockBody("r"+strconv.Itoa(i), "S", 1), nil)
		codes = append(codes, code)
	}
	if codes[0] != 200 || codes[1] != 200 || codes[2] != 429 || codes[3] != 429 {
		t.Fatalf("%v", codes)
	}
	// Another org has its own budget.
	if code, _ := e.send(t, e.orgB, "stock.changed", stockBody("r", "S", 1), nil); code != 200 {
		t.Fatalf("org B throttled by org A: %d", code)
	}
}

func TestPreAuthRateLimitProtectsBeforeAuth(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.PreAuthLimiter = &TokenBucket{Rate: 0.0001, Burst: 1} })
	c1, _ := e.send(t, e.orgA, "stock.changed", stockBody("a", "S", 1), func(r *http.Request) { r.Header.Del("X-Metacore-Signature") })
	c2, out := e.send(t, e.orgA, "stock.changed", stockBody("b", "S", 1), nil)
	if c1 != 401 || c2 != 429 || errCode(out) != CodeRateLimited {
		t.Fatalf("%d %d %v", c1, c2, out)
	}
}

func TestTokenBucketRefillsAndBoundsKeys(t *testing.T) {
	now := time.Unix(0, 0)
	tb := &TokenBucket{Rate: 1, Burst: 1, MaxKeys: 2, Now: func() time.Time { return now }}
	if !tb.Allow("a") || tb.Allow("a") {
		t.Fatal("burst")
	}
	now = now.Add(time.Second)
	if !tb.Allow("a") {
		t.Fatal("refill")
	}
	tb.Allow("b")
	if tb.Allow("c") {
		t.Fatal("must fail closed when the key table is full of live keys")
	}
	now = now.Add(2 * time.Minute)
	if !tb.Allow("c") {
		t.Fatal("idle keys must be evicted")
	}
}

func TestInProgressReservation(t *testing.T) {
	e := newEnv(t, nil)
	org := e.orgA
	// Simulate another replica mid-flight: a fresh processing reservation.
	_, claimed, _ := e.store.Begin(context.Background(), Receipt{OrgID: org, Contract: "stock.changed", Version: 1,
		KeyHash: HashKey(org, "stock.changed", "ev-ip"), BodyHash: "h"}, time.Minute)
	if !claimed {
		t.Fatal("setup")
	}
	code, out := e.send(t, org, "stock.changed", stockBody("ev-ip", "S", 1), nil)
	if code != 409 || errCode(out) != CodeInProgress {
		t.Fatalf("%d %v", code, out)
	}
	// After the lease expires the event is re-claimed (crashed worker).
	e.store.Now = func() time.Time { return time.Now().Add(2 * DefaultLease) }
	code, out = e.send(t, org, "stock.changed", stockBody("ev-ip", "S", 1), nil)
	if code != 200 || out["status"] != "applied" {
		t.Fatalf("stale reservation not recovered: %d %v", code, out)
	}
}
