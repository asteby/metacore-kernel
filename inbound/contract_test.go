package inbound

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestRegisterValidation(t *testing.T) {
	mut := func(f func(*Contract)) error {
		c := stockContract()
		f(&c)
		return NewRegistry().Register(c)
	}
	bad := map[string]func(*Contract){
		"bad key":           func(c *Contract) { c.Key = "Bad Key" },
		"version":           func(c *Contract) { c.Version = 0 },
		"no auth":           func(c *Contract) { c.Auth = "" },
		"no idempotency":    func(c *Contract) { c.Idempotency = "" },
		"literal-only key":  func(c *Contract) { c.Idempotency = "constant" },
		"unbalanced braces": func(c *Contract) { c.Idempotency = "{event_id}}" },
		"both targets":      func(c *Contract) { c.Route.Command = "x" },
		"no target":         func(c *Contract) { c.Route.Model = "" },
		"bad op":            func(c *Contract) { c.Route.Op = "delete" },
		"empty mapping":     func(c *Contract) { c.Route.Mapping = nil },
		"id target":         func(c *Contract) { c.Route.Mapping["id"] = "sku" },
		"org target":        func(c *Contract) { c.Route.Mapping["organization_id"] = "sku" },
		"org default":       func(c *Contract) { c.Route.Defaults = map[string]any{"organization_id": "x"} },
		"bad path":          func(c *Contract) { c.Route.Mapping["sku"] = "a..b" },
		"path chars":        func(c *Contract) { c.Route.Mapping["sku"] = "a[0]" },
		"upsert no match":   func(c *Contract) { c.Route.Match = nil },
		"match not mapped":  func(c *Contract) { c.Route.Match = []string{"ghost"} },
		"no schema":         func(c *Contract) { c.JSONSchema = nil },
		"broken schema":     func(c *Contract) { c.JSONSchema = json.RawMessage(`{"type":"nonsense"}`) },
		"not json":          func(c *Contract) { c.JSONSchema = json.RawMessage(`{`) },
		"remote ref": func(c *Contract) {
			c.JSONSchema = json.RawMessage(`{"$ref":"http://169.254.169.254/latest/meta-data"}`)
		},
		"file ref": func(c *Contract) { c.JSONSchema = json.RawMessage(`{"$ref":"file:///etc/passwd"}`) },
	}
	for name, f := range bad {
		if err := mut(f); !errors.Is(err, ErrInvalidContract) {
			t.Errorf("%s: want ErrInvalidContract, got %v", name, err)
		}
	}
	if err := mut(func(*Contract) {}); err != nil {
		t.Fatalf("valid contract rejected: %v", err)
	}
}

func TestVersionsLatestWins(t *testing.T) {
	r := NewRegistry()
	c1, c2 := stockContract(), stockContract()
	c2.Version = 2
	c2.Route.Defaults = map[string]any{"source": "v2"}
	_ = r.Register(c1)
	_ = r.Register(c2)
	if c, _ := r.get("stock.changed", 0); c.Version != 2 {
		t.Fatalf("latest = %d", c.Version)
	}
	if c, _ := r.get("stock.changed", 1); c.Version != 1 {
		t.Fatalf("v1 = %d", c.Version)
	}
	if _, ok := r.get("stock.changed", 3); ok {
		t.Fatal("v3 does not exist")
	}
	if k := r.Keys(); len(k) != 1 || k[0] != "stock.changed" {
		t.Fatalf("keys %v", k)
	}
}

func TestNewContractIsOnlyData(t *testing.T) {
	// A host that already runs gets a new event type by loading JSON; no new
	// route and no code.
	e := newEnv(t, nil)
	data := `[{"key":"price.changed","version":1,
	  "json_schema":{"type":"object","required":["id","price"],"properties":{"id":{"type":"string"},"price":{"type":"number"}}},
	  "route":{"model":"inventory","op":"upsert","mapping":{"sku":"id","price":"price"},"match":["sku"]},
	  "idempotency":"{id}:{price}","auth":"hmac_canonical_v1"}]`
	if err := e.h.cfg.Contracts.LoadJSON([]byte(data)); err != nil {
		t.Fatal(err)
	}
	if err := e.h.Check(); err != nil {
		t.Fatal(err)
	}
	code, out := e.send(t, e.orgA, "price.changed", `{"id":"SKU-1","price":19.9}`, nil)
	if code != 200 || out["status"] != "applied" {
		t.Fatalf("%d %v", code, out)
	}
	if e.exec.last.Values["price"] != 19.9 {
		t.Fatalf("%v", e.exec.last.Values)
	}
	if err := e.h.cfg.Contracts.LoadJSON([]byte(`{"key":"x"}`)); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("%v", err)
	}
	if err := e.h.cfg.Contracts.LoadJSON([]byte(`not json`)); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("%v", err)
	}
}

func TestLookupAndMapping(t *testing.T) {
	var p any
	_ = json.Unmarshal([]byte(`{"a":{"b":[{"c":5},{"c":null}]},"s":"x","n":null}`), &p)
	if v, ok := lookup(p, "a.b.0.c"); !ok || v != float64(5) {
		t.Fatalf("%v %v", v, ok)
	}
	for _, miss := range []string{"a.b.1.c", "a.b.9.c", "a.z", "s.x", "n", "a.b.x"} {
		if _, ok := lookup(p, miss); ok {
			t.Errorf("%s should be absent", miss)
		}
	}
	r := Route{Mapping: map[string]string{"x": "s", "y": "a.b.0.c", "z": "missing"}, Defaults: map[string]any{"x": "default", "k": 1}}
	got := applyMapping(r, p)
	if got["x"] != "x" || got["y"] != float64(5) || got["k"] != 1 {
		t.Fatalf("%v", got)
	}
	if _, ok := got["z"]; ok {
		t.Fatal("absent source must be omitted, not nil")
	}
}

func TestEventKeyTemplate(t *testing.T) {
	parts, err := parseTemplate("{a.b}:{header:X-Id}-v{n}")
	if err != nil {
		t.Fatal(err)
	}
	dec, _ := decodeJSON([]byte(`{"a":{"b":"k1"},"n":2}`))
	h := http.Header{"X-Id": {"H"}}
	if k, err := eventKey(parts, dec, h); err != nil || k != "k1:H-v2" {
		t.Fatalf("%q %v", k, err)
	}
	if _, err := eventKey(parts, dec, http.Header{}); err == nil {
		t.Fatal("missing header must fail")
	}
	dec2, _ := decodeJSON([]byte(`{"a":{"b":{"obj":1}},"n":2}`))
	if _, err := eventKey(parts, dec2, h); err == nil {
		t.Fatal("non-scalar must fail")
	}
	dec3, _ := decodeJSON([]byte(`{"a":{"b":"k"}}`))
	if _, err := eventKey(parts, dec3, h); err == nil {
		t.Fatal("missing field must fail")
	}
	long, _ := decodeJSON([]byte(`{"a":{"b":"` + strings.Repeat("x", 300) + `"},"n":1}`))
	if _, err := eventKey(parts, long, h); err == nil {
		t.Fatal("overlong key must fail")
	}
}

func TestEventKeyFromHeaderContract(t *testing.T) {
	reg := NewRegistry()
	c := stockContract()
	c.Idempotency = "{header:X-Event-Id}"
	_ = reg.Register(c)
	e := newEnv(t, func(cfg *Config) { cfg.Contracts = reg })
	withHdr := func(id string) func(*http.Request) {
		return func(r *http.Request) {
			if id != "" {
				r.Header.Set("X-Event-Id", id)
			}
		}
	}
	if code, _ := e.send(t, e.orgA, "stock.changed", stockBody("a", "S", 1), withHdr("")); code != 422 {
		t.Fatalf("missing header: %d", code)
	}
	if code, _ := e.send(t, e.orgA, "stock.changed", stockBody("a", "S", 1), withHdr("h1")); code != 200 {
		t.Fatalf("%d", code)
	}
	if _, out := e.send(t, e.orgA, "stock.changed", stockBody("a", "S", 1), withHdr("h1")); out["status"] != "replayed" {
		t.Fatalf("%v", out)
	}
}

// ---------------------------------------------------------------- verifier ---

func TestHMACCanonicalV1Verifier(t *testing.T) {
	org := uuid.New()
	now := time.Unix(1_800_000_000, 0)
	sec := [][]byte{[]byte("new"), []byte("old")}
	v := &HMACCanonicalV1{Secrets: func(context.Context, uuid.UUID) ([][]byte, error) { return sec, nil },
		Now: func() time.Time { return now }, MaxNonces: 2}
	body := []byte(`{"a":1}`)
	mk := func(secret, ts, nonce, method, path string) VerifyRequest {
		h := http.Header{}
		h.Set("X-Metacore-Timestamp", ts)
		h.Set("X-Metacore-Nonce", nonce)
		h.Set("X-Metacore-Signature", SignCanonicalV1([]byte(secret), ts, nonce, method, path, body))
		return VerifyRequest{Org: org, Method: "POST", Path: "/p", Header: h, Body: body}
	}
	ts := "1800000000"
	if err := v.Verify(context.Background(), mk("old", ts, "n1", "POST", "/p")); err != nil {
		t.Fatalf("rotated secret must verify: %v", err)
	}
	if err := v.Verify(context.Background(), mk("old", ts, "n1", "POST", "/p")); err == nil {
		t.Fatal("replay accepted")
	}
	if err := v.Verify(context.Background(), mk("other", ts, "n2", "POST", "/p")); err == nil {
		t.Fatal("wrong secret accepted")
	}
	// Failed signatures must not burn the nonce (no nonce-exhaustion by forgery).
	if err := v.Verify(context.Background(), mk("new", ts, "n2", "POST", "/p")); err != nil {
		t.Fatalf("nonce burned by a forged request: %v", err)
	}
	if err := v.Verify(context.Background(), mk("new", ts, "n3", "GET", "/p")); err == nil {
		t.Fatal("method is part of the signature")
	}
	if err := v.Verify(context.Background(), mk("new", ts, "n3", "POST", "/other")); err == nil {
		t.Fatal("path is part of the signature")
	}
	// Cache cap (2) is full of live nonces: fail closed.
	if err := v.Verify(context.Background(), mk("new", ts, "n4", "POST", "/p")); err == nil {
		t.Fatal("full nonce cache must fail closed")
	}
	// Expired entries free the space.
	now = now.Add(DefaultNonceTTL + time.Second)
	ts2 := "1800000601"
	if err := v.Verify(context.Background(), mk("new", ts2, "n4", "POST", "/p")); err != nil {
		t.Fatalf("after ttl: %v", err)
	}
	// Oversized nonce and missing resolver.
	if err := v.Verify(context.Background(), mk("new", ts2, strings.Repeat("n", 200), "POST", "/p")); err == nil {
		t.Fatal("oversized nonce accepted")
	}
	if err := (&HMACCanonicalV1{}).Verify(context.Background(), mk("new", ts2, "z", "POST", "/p")); err == nil {
		t.Fatal("no resolver must fail")
	}
}

func TestSignatureFormatMatchesRFC(t *testing.T) {
	// "{ts}.{nonce}.{METHOD}.{path}.{sha256hex(body)}" with an empty body.
	got := SignCanonicalV1([]byte("k"), "1", "n", "post", "/p", nil)
	again := SignCanonicalV1([]byte("k"), "1", "n", "POST", "/p", []byte{})
	if got != again || len(got) != 64 {
		t.Fatalf("%s %s", got, again)
	}
}
