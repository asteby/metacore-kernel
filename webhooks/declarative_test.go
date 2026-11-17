package webhooks

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/events"
	"github.com/asteby/metacore-kernel/security"
	"github.com/asteby/metacore-kernel/webhooks/signing"
)

// --- infraestructura de prueba --------------------------------------------

const (
	testHMACSecret = "SUPERSECRET-hmac-0123456789"
	testBearer     = "chn_TOKEN.abcdef-bearer-secret"
	testPath       = "/api/channels/v1/customers"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock { return &fakeClock{t: time.Now().Truncate(time.Second)} }
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// hit es una peticion recibida por el destino de prueba.
type hit struct {
	Method, Path string
	Body         []byte
	Header       http.Header
	VerifyErr    error
	Status       int
}

// target es un destino HTTPS de prueba que verifica la firma con la MISMA
// libreria de verificacion que usara la recepcion.
type target struct {
	*httptest.Server
	mu     sync.Mutex
	hits   []hit
	script []int // status por peticion; agotado => 201
	spec   *signing.Spec
	clk    *fakeClock
	nonces *security.NonceCache
}

func newTarget(t *testing.T, clk *fakeClock, spec *signing.Spec, script ...int) *target {
	t.Helper()
	tg := &target{script: script, spec: spec, clk: clk, nonces: security.NewNonceCache(10 * time.Minute)}
	tg.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		tg.mu.Lock()
		status := 201
		if len(tg.script) > 0 {
			status, tg.script = tg.script[0], tg.script[1:]
		}
		h := hit{Method: r.Method, Path: r.URL.Path, Body: body, Header: r.Header.Clone(), Status: status}
		if tg.spec != nil {
			h.VerifyErr = signing.Verify(*tg.spec, signing.Secrets{HMACKey: testHMACSecret, BearerToken: testBearer},
				signing.Request{Method: r.Method, Path: signing.PathOf(r.URL), Body: body}, r.Header,
				signing.VerifyOptions{Now: tg.clk.Now(), Nonces: tg.nonces})
		}
		tg.hits = append(tg.hits, h)
		tg.mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	t.Cleanup(tg.Close)
	return tg
}

func (tg *target) Hits() []hit {
	tg.mu.Lock()
	defer tg.mu.Unlock()
	return append([]hit(nil), tg.hits...)
}

type testEnv struct {
	db  *gorm.DB
	svc *Service
	clk *fakeClock
	org uuid.UUID
	tg  *target
	bus *events.Bus
}

func testSchemas() *Schemas {
	return NewSchemas().
		Register("customers.customer.created", "id", "name", "email", "branch_id", "channel", "address.city").
		Register("customers.customer.updated", "id", "name", "email", "branch_id", "channel")
}

func testResolver() SecretResolver {
	return SecretResolverFunc(func(_ context.Context, ref string) (string, error) {
		switch ref {
		case "v2.signing_secret":
			return testHMACSecret, nil
		case "v2.api_key":
			return testBearer, nil
		}
		return "", errors.New("unknown ref")
	})
}

// newEnv arma servicio + destino + bus con el puente conectado.
func newEnv(t *testing.T, spec *signing.Spec, script ...int) *testEnv {
	t.Helper()
	db := setupTestDB(t)
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1) // ":memory:" es por conexion
	clk := newClock()
	tg := newTarget(t, clk, spec, script...)
	svc := New(Config{
		DB: db, Clock: clk.Now, HTTPClient: tg.Client(),
		Schemas: testSchemas(), Secrets: testResolver(), UnsafeAllowPrivateTargets: true,
	})
	bus := events.NewBus(nil)
	if err := NewBridge(svc, BridgeOptions{}).Attach(bus); err != nil {
		t.Fatal(err)
	}
	return &testEnv{db: db, svc: svc, clk: clk, org: uuid.New(), tg: tg, bus: bus}
}

func (e *testEnv) sub(decl *Declarative, evs ...string) *Webhook {
	if len(evs) == 0 {
		evs = []string{"customers.*"}
	}
	return &Webhook{Name: "v2", URL: e.tg.URL + testPath, Events: StringSlice(evs), Active: true,
		OwnerType: "organization", OwnerID: e.org, Declarative: decl, RetryMax: 5}
}

func (e *testEnv) create(t *testing.T, w *Webhook) *Webhook {
	t.Helper()
	if err := e.svc.Create(context.Background(), w); err != nil {
		t.Fatalf("create: %v", err)
	}
	return w
}

func (e *testEnv) publish(t *testing.T, event string, payload map[string]any) {
	t.Helper()
	if err := e.bus.Publish(context.Background(), "kernel", event, e.org, payload); err != nil {
		t.Fatalf("publish: %v", err)
	}
}

func (e *testEnv) drain(n int) {
	for i := 0; i < n; i++ {
		e.svc.drainOnce(context.Background())
	}
}

func (e *testEnv) deliveries(t *testing.T, w *Webhook) []WebhookDelivery {
	t.Helper()
	var out []WebhookDelivery
	if err := e.db.Where("webhook_id = ?", w.ID).Order("seq ASC").Find(&out).Error; err != nil {
		t.Fatal(err)
	}
	return out
}

func customerPayload(evID, aggID string, extra map[string]any) map[string]any {
	p := map[string]any{
		"id": aggID, "name": "  Ana Perez ", "email": "ANA@Example.com", "branch_id": "br-1", "channel": "web",
		"envelope": map[string]any{
			"id": evID, "source": "kernel", "branch_id": "br-1",
			"aggregate": map[string]any{"type": "customers.customer", "id": aggID},
			"sequence":  1,
		},
	}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

func v2Decl() *Declarative {
	return &Declarative{
		Key:    "v2-clientes",
		Filter: &Filter{All: []Condition{{Path: "payload.branch_id", In: []any{"br-1", "br-2"}}}},
		Transform: &Transform{
			Body: map[string]any{
				"external_reference": "{{event.aggregate_id}}",
				"name":               "{{payload.name | trim}}",
				"email":              "{{payload.email | lower}}",
				"channel":            "{{payload.channel | map:channels}}",
				"origin":             "ops",
			},
			Tables: map[string]map[string]string{"channels": {"web": "b2c", "pos": "retail"}},
		},
		Signing:     &signing.Spec{Profile: signing.BearerHMACCanonicalV1, SecretRef: "v2.signing_secret", KeyRef: "v2.api_key"},
		Headers:     map[string]string{"X-7L-Contract": "1.0"},
		Idempotency: "{{event.id}}",
		Ordering:    OrderingPerAggregate,
		Enabled:     true,
	}
}

func v2Spec() *signing.Spec { return v2Decl().Signing }

// --- punta a punta ---------------------------------------------------------

// Evento del bus -> filtro -> cuerpo transformado -> firmado -> servidor que
// verifica la firma -> 500 -> reintento con la MISMA llave de idempotencia.
func TestEndToEndBusToSignedDeliveryWithRetry(t *testing.T) {
	e := newEnv(t, v2Spec(), 500, 201)
	w := e.create(t, e.sub(v2Decl()))

	e.publish(t, "customers.customer.created", customerPayload("evt-0001", "cus-1", nil))

	rows := e.deliveries(t, w)
	if len(rows) != 1 || rows[0].Held || rows[0].NextAttemptAt == nil {
		t.Fatalf("expected one queued delivery, got %+v", rows)
	}
	if rows[0].IdempotencyKey != "evt-0001" || rows[0].OrderingKey != "customers.customer:cus-1" {
		t.Fatalf("idem/ordering = %q / %q", rows[0].IdempotencyKey, rows[0].OrderingKey)
	}

	// intento 1: el destino responde 500
	e.drain(1)
	hits := e.tg.Hits()
	if len(hits) != 1 || hits[0].Status != 500 {
		t.Fatalf("attempt 1: %+v", hits)
	}
	rows = e.deliveries(t, w)
	if rows[0].Succeeded || rows[0].AttemptCount != 1 || rows[0].NextAttemptAt == nil {
		t.Fatalf("after 500 the delivery must be scheduled for retry: %+v", rows[0])
	}
	// aun no toca (backoff)
	e.drain(1)
	if len(e.tg.Hits()) != 1 {
		t.Fatal("retry must wait for the backoff")
	}

	// intento 2, tras el backoff
	e.clk.Advance(10 * time.Second)
	e.drain(1)
	hits = e.tg.Hits()
	if len(hits) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(hits))
	}
	for i, h := range hits {
		if h.VerifyErr != nil {
			t.Errorf("attempt %d: signature rejected by the receiver-side Verify: %v", i+1, h.VerifyErr)
		}
		if h.Method != "POST" || h.Path != testPath {
			t.Errorf("attempt %d: %s %s", i+1, h.Method, h.Path)
		}
		if h.Header.Get("Authorization") != "Bearer "+testBearer {
			t.Errorf("attempt %d: bearer missing", i+1)
		}
		if h.Header.Get("X-7L-Contract") != "1.0" {
			t.Errorf("attempt %d: constant header missing", i+1)
		}
	}
	// misma llave de idempotencia y mismos bytes; nonce y timestamp frescos
	if hits[0].Header.Get("X-Idempotency-Key") != "evt-0001" || hits[1].Header.Get("X-Idempotency-Key") != "evt-0001" {
		t.Fatalf("idempotency keys: %q %q", hits[0].Header.Get("X-Idempotency-Key"), hits[1].Header.Get("X-Idempotency-Key"))
	}
	if string(hits[0].Body) != string(hits[1].Body) {
		t.Fatal("body must be byte-identical across retries")
	}
	if hits[0].Header.Get("X-7L-Nonce") == hits[1].Header.Get("X-7L-Nonce") {
		t.Fatal("each attempt needs a fresh nonce (receiver rejects repeats)")
	}
	if hits[0].Header.Get("X-7L-Timestamp") == hits[1].Header.Get("X-7L-Timestamp") {
		t.Fatal("each attempt is signed with its own timestamp")
	}
	var body map[string]any
	_ = json.Unmarshal(hits[1].Body, &body)
	want := map[string]any{"external_reference": "cus-1", "name": "Ana Perez", "email": "ana@example.com", "channel": "b2c", "origin": "ops"}
	if !reflect.DeepEqual(body, want) {
		t.Fatalf("body = %v, want %v", body, want)
	}
	// estado final + PII borrada
	rows = e.deliveries(t, w)
	if !rows[0].Succeeded || rows[0].AttemptCount != 2 || rows[0].DeliveredAt == nil {
		t.Fatalf("final: %+v", rows[0])
	}
	if len(rows[0].Payload) != 0 || !rows[0].PayloadPurged {
		t.Fatalf("personal payload must be erased once delivered: %+v", rows[0].Payload)
	}
}

func TestEndToEndFilterExcludesEvent(t *testing.T) {
	e := newEnv(t, v2Spec())
	w := e.create(t, e.sub(v2Decl()))
	e.publish(t, "customers.customer.created", customerPayload("evt-x", "cus-9", map[string]any{"branch_id": "br-OTHER"}))
	if n := len(e.deliveries(t, w)); n != 0 {
		t.Fatalf("filtered event must not create deliveries, got %d", n)
	}
}

func TestEventPatternMatching(t *testing.T) {
	e := newEnv(t, nil)
	d := &Declarative{Enabled: true}
	w := e.create(t, e.sub(d, "customers.customer.created"))
	e.publish(t, "customers.customer.updated", customerPayload("evt-u", "cus-1", nil))
	e.publish(t, "orders.order.created", map[string]any{"id": "o1"})
	if n := len(e.deliveries(t, w)); n != 0 {
		t.Fatalf("non matching events delivered: %d", n)
	}
	e.publish(t, "customers.customer.created", customerPayload("evt-c", "cus-1", nil))
	if n := len(e.deliveries(t, w)); n != 1 {
		t.Fatalf("matching event not queued: %d", n)
	}
}

func TestDefaultBodyHasEnvelopeAndPayload(t *testing.T) {
	e := newEnv(t, nil)
	w := e.create(t, e.sub(&Declarative{Enabled: true}))
	e.publish(t, "customers.customer.created", customerPayload("evt-d", "cus-1", nil))
	e.drain(1)
	hits := e.tg.Hits()
	if len(hits) != 1 {
		t.Fatalf("hits: %d", len(hits))
	}
	var body struct {
		Event   map[string]any `json:"event"`
		Payload map[string]any `json:"payload"`
	}
	_ = json.Unmarshal(hits[0].Body, &body)
	if body.Event["id"] != "evt-d" || body.Event["aggregate_id"] != "cus-1" || body.Payload["name"] == nil {
		t.Fatalf("default body: %s", hits[0].Body)
	}
	if _, has := body.Payload["envelope"]; has {
		t.Fatal("envelope must not leak into payload")
	}
	// sin Signing: firma historica X-Metacore-* con el secreto del webhook
	if hits[0].Header.Get("X-Metacore-Signature") == "" {
		t.Fatal("legacy signature headers expected without a signing profile")
	}
	_ = w
}

// --- retencion (enabled apagado) -----------------------------------------

func TestDisabledRetainsAndEnableReleases(t *testing.T) {
	e := newEnv(t, v2Spec())
	d := v2Decl()
	d.Enabled = false
	w := e.create(t, e.sub(d))

	e.publish(t, "customers.customer.created", customerPayload("evt-1", "cus-1", nil))
	e.publish(t, "customers.customer.updated", customerPayload("evt-2", "cus-1", nil))

	rows := e.deliveries(t, w)
	if len(rows) != 2 || !rows[0].Held || !rows[1].Held || rows[0].NextAttemptAt != nil {
		t.Fatalf("events must be retained, not lost: %+v", rows)
	}
	e.clk.Advance(time.Minute)
	e.drain(3)
	if len(e.tg.Hits()) != 0 {
		t.Fatal("a disabled subscription must not send")
	}

	released, err := e.svc.SetEnabled(context.Background(), w.ID, true)
	if err != nil || released != 2 {
		t.Fatalf("SetEnabled released %d, err %v", released, err)
	}
	e.drain(3)
	hits := e.tg.Hits()
	if len(hits) != 2 {
		t.Fatalf("retained events must go out after enabling, got %d", len(hits))
	}
	for _, h := range hits {
		if h.VerifyErr != nil {
			t.Fatalf("verify: %v", h.VerifyErr)
		}
	}
	// salen en el orden en que llegaron (mismo agregado)
	if hits[0].Header.Get("X-Idempotency-Key") != "evt-1" || hits[1].Header.Get("X-Idempotency-Key") != "evt-2" {
		t.Fatalf("order: %q then %q", hits[0].Header.Get("X-Idempotency-Key"), hits[1].Header.Get("X-Idempotency-Key"))
	}
}

func TestUpdateEnablingAlsoReleases(t *testing.T) {
	e := newEnv(t, nil)
	w := e.create(t, e.sub(&Declarative{Enabled: false}))
	e.publish(t, "customers.customer.created", customerPayload("evt-1", "cus-1", nil))
	w.Declarative.Enabled = true
	if err := e.svc.Update(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	e.drain(1)
	if len(e.tg.Hits()) != 1 {
		t.Fatal("Update(enabled=true) must release the retained event")
	}
}

func TestDisablingWhileQueuedRetainsInsteadOfSending(t *testing.T) {
	e := newEnv(t, nil)
	w := e.create(t, e.sub(&Declarative{Enabled: true}))
	e.publish(t, "customers.customer.created", customerPayload("evt-1", "cus-1", nil))
	if _, err := e.svc.SetEnabled(context.Background(), w.ID, false); err != nil {
		t.Fatal(err)
	}
	e.drain(2)
	if len(e.tg.Hits()) != 0 {
		t.Fatal("queued delivery must be retained once the subscription is turned off")
	}
	if rows := e.deliveries(t, w); !rows[0].Held {
		t.Fatalf("not held: %+v", rows[0])
	}
	if n, _ := e.svc.SetEnabled(context.Background(), w.ID, true); n != 1 {
		t.Fatalf("released %d", n)
	}
	e.drain(1)
	if len(e.tg.Hits()) != 1 {
		t.Fatal("not sent after re-enabling")
	}
}

func TestSetEnabledOnClassicWebhook(t *testing.T) {
	e := newEnv(t, nil)
	w := e.create(t, e.sub(nil))
	if _, err := e.svc.SetEnabled(context.Background(), w.ID, true); !errors.Is(err, ErrNotDeclarative) {
		t.Fatalf("got %v", err)
	}
}

// --- idempotencia y deduplicacion ----------------------------------------

func TestBusRepublicationIsDeduplicated(t *testing.T) {
	e := newEnv(t, nil)
	w := e.create(t, e.sub(&Declarative{Enabled: true}))
	p := customerPayload("evt-same", "cus-1", nil)
	e.publish(t, "customers.customer.created", p)
	e.publish(t, "customers.customer.created", p) // relay del outbox: at-least-once
	if n := len(e.deliveries(t, w)); n != 1 {
		t.Fatalf("republished event created %d deliveries", n)
	}
	// sin id de sobre: id derivado de los bytes => tambien deduplica
	q := map[string]any{"id": "cus-2", "name": "x", "branch_id": "br-1"}
	e.publish(t, "customers.customer.created", q)
	e.publish(t, "customers.customer.created", q)
	if n := len(e.deliveries(t, w)); n != 2 {
		t.Fatalf("derived-id dedupe: %d deliveries", n)
	}
}

func TestIdempotencyTemplateAndFallback(t *testing.T) {
	e := newEnv(t, nil)
	d := &Declarative{Enabled: true, Idempotency: "cust-{{event.aggregate_id}}-v{{event.sequence}}"}
	w := e.create(t, e.sub(d))
	e.publish(t, "customers.customer.created", customerPayload("evt-1", "cus-7", nil))
	if got := e.deliveries(t, w)[0].IdempotencyKey; got != "cust-cus-7-v1" {
		t.Fatalf("key = %q", got)
	}
	// plantilla que renderiza vacio => cae al id del evento
	d2 := &Declarative{Enabled: true, Idempotency: "{{event.actor_id}}"}
	w2 := e.create(t, e.sub(d2))
	e.publish(t, "customers.customer.updated", customerPayload("evt-2", "cus-7", nil))
	if got := e.deliveries(t, w2)[0].IdempotencyKey; got != "evt-2" {
		t.Fatalf("fallback key = %q", got)
	}
	e.drain(2)
	h := e.tg.Hits()
	if len(h) == 0 || h[0].Header.Get("X-Idempotency-Key") == "" {
		t.Fatal("key not sent")
	}
}

func TestCustomIdempotencyHeader(t *testing.T) {
	e := newEnv(t, nil)
	e.create(t, e.sub(&Declarative{Enabled: true, IdempotencyHeader: "Idempotency-Key"}))
	e.publish(t, "customers.customer.created", customerPayload("evt-1", "cus-1", nil))
	e.drain(1)
	if e.tg.Hits()[0].Header.Get("Idempotency-Key") != "evt-1" {
		t.Fatal("custom idempotency header not used")
	}
}

func TestReplayKeepsIdempotencyKey(t *testing.T) {
	e := newEnv(t, nil, 422) // primera entrega: 422 => muerta (no se reintenta)
	w := e.create(t, e.sub(&Declarative{Enabled: true}))
	e.publish(t, "customers.customer.created", customerPayload("evt-r", "cus-1", nil))
	e.drain(1)
	rows := e.deliveries(t, w)
	if rows[0].Succeeded || rows[0].NextAttemptAt != nil {
		t.Fatalf("422 is permanent: %+v", rows[0])
	}
	if len(rows[0].Payload) == 0 {
		t.Fatal("undelivered payload must be kept so it can be replayed")
	}
	d, err := e.svc.Replay(context.Background(), rows[0].ID)
	if err != nil || !d.Succeeded {
		t.Fatalf("replay: %v %+v", err, d)
	}
	h := e.tg.Hits()
	if len(h) != 2 || h[1].Header.Get("X-Idempotency-Key") != "evt-r" {
		t.Fatalf("replay must reuse the key: %+v", h)
	}
	// ya entregada: el payload se borro, no se puede repetir
	if _, err := e.svc.Replay(context.Background(), d.ID); !errors.Is(err, ErrPayloadPurged) {
		t.Fatalf("want ErrPayloadPurged, got %v", err)
	}
}

// --- politica de reintentos ----------------------------------------------

func TestRetryPolicyByStatus(t *testing.T) {
	cases := map[int]bool{ // status => se reintenta
		500: true, 502: true, 503: true, 429: true, 423: true, 409: true, 408: true,
		400: false, 401: false, 403: false, 404: false, 422: false,
	}
	for status, retry := range cases {
		e := newEnv(t, nil, status)
		w := e.create(t, e.sub(&Declarative{Enabled: true}))
		e.publish(t, "customers.customer.created", customerPayload("evt-p", "cus-1", nil))
		e.drain(1)
		rows := e.deliveries(t, w)
		if got := rows[0].NextAttemptAt != nil; got != retry {
			t.Errorf("status %d: retry = %v, want %v", status, got, retry)
		}
	}
}

func TestRetriesStopAtRetryMax(t *testing.T) {
	e := newEnv(t, nil, 500, 500, 500)
	w := e.sub(&Declarative{Enabled: true})
	w.RetryMax = 2
	e.create(t, w)
	e.publish(t, "customers.customer.created", customerPayload("evt-m", "cus-1", nil))
	for i := 0; i < 5; i++ {
		e.drain(1)
		e.clk.Advance(2 * time.Hour)
	}
	if n := len(e.tg.Hits()); n != 2 {
		t.Fatalf("attempts = %d, want 2", n)
	}
}

// --- orden por agregado ---------------------------------------------------

func TestPerAggregateOrdering(t *testing.T) {
	e := newEnv(t, nil, 500) // el 1er evento de cus-1 falla una vez
	d := &Declarative{Enabled: true, Ordering: OrderingPerAggregate}
	e.create(t, e.sub(d))
	e.publish(t, "customers.customer.created", customerPayload("a1", "cus-1", nil))
	e.publish(t, "customers.customer.updated", customerPayload("a2", "cus-1", nil))
	e.publish(t, "customers.customer.created", customerPayload("b1", "cus-2", nil))

	e.drain(1) // a1 -> 500
	e.drain(3) // a2 esta bloqueado por a1; b1 (otro agregado) pasa; a1 espera backoff
	keys := func() []string {
		var out []string
		for _, h := range e.tg.Hits() {
			out = append(out, h.Header.Get("X-Idempotency-Key")+":"+http.StatusText(h.Status)[:2])
		}
		return out
	}
	if got := strings.Join(keys(), ","); got != "a1:In,b1:Cr" {
		t.Fatalf("while a1 is failing only the other aggregate may flow, got %s", got)
	}
	e.clk.Advance(time.Minute)
	e.drain(3)
	var order []string
	for _, h := range e.tg.Hits() {
		order = append(order, h.Header.Get("X-Idempotency-Key"))
	}
	if got := strings.Join(order, ","); got != "a1,b1,a1,a2" {
		t.Fatalf("order = %s, want a1,b1,a1,a2 (a2 only after a1 succeeded)", got)
	}
}

func TestOrderingNoneDoesNotBlock(t *testing.T) {
	e := newEnv(t, nil, 500)
	e.create(t, e.sub(&Declarative{Enabled: true}))
	e.publish(t, "customers.customer.created", customerPayload("a1", "cus-1", nil))
	e.publish(t, "customers.customer.updated", customerPayload("a2", "cus-1", nil))
	e.drain(2)
	if n := len(e.tg.Hits()); n != 2 {
		t.Fatalf("without ordering both go out immediately, got %d", n)
	}
}

func TestExhaustedPredecessorNoLongerBlocks(t *testing.T) {
	e := newEnv(t, nil, 500, 500)
	w := e.sub(&Declarative{Enabled: true, Ordering: OrderingPerAggregate})
	w.RetryMax = 1
	e.create(t, w)
	e.publish(t, "customers.customer.created", customerPayload("a1", "cus-1", nil))
	e.publish(t, "customers.customer.updated", customerPayload("a2", "cus-1", nil))
	e.drain(1) // a1 -> 500, sin reintentos (RetryMax=1): queda muerta
	e.drain(1) // a2 ya no esta bloqueado
	if got := len(e.tg.Hits()); got != 2 {
		t.Fatalf("a dead-lettered predecessor must not block forever, hits=%d", got)
	}
}

// --- fallos de render ----------------------------------------------------

func TestRenderFailureIsRecordedNotLost(t *testing.T) {
	e := newEnv(t, nil)
	d := &Declarative{Enabled: true, Transform: &Transform{
		Body:   map[string]any{"c": "{{payload.channel | map:ch}}"},
		Tables: map[string]map[string]string{"ch": {"pos": "retail"}}, // 'web' no esta
	}}
	w := e.create(t, e.sub(d))
	e.publish(t, "customers.customer.created", customerPayload("evt-f", "cus-1", nil))
	rows := e.deliveries(t, w)
	if len(rows) != 1 || rows[0].NextAttemptAt != nil || !strings.Contains(rows[0].ErrorMessage, "no entry") {
		t.Fatalf("render failure must leave a visible dead delivery: %+v", rows)
	}
	e.drain(2)
	if len(e.tg.Hits()) != 0 {
		t.Fatal("nothing must be sent")
	}
}

// --- validacion al guardar ------------------------------------------------

func TestSaveRejectsUnknownPathsAndBadConfig(t *testing.T) {
	e := newEnv(t, nil)
	mk := func(mut func(d *Declarative, w *Webhook)) error {
		d := &Declarative{Enabled: true}
		w := e.sub(d)
		mut(d, w)
		return e.svc.Create(context.Background(), w)
	}
	cases := []struct {
		name string
		mut  func(d *Declarative, w *Webhook)
		is   error
	}{
		{"payload path not in schema (transform)", func(d *Declarative, w *Webhook) {
			d.Transform = &Transform{Body: map[string]any{"x": "{{payload.nombre}}"}}
		}, ErrUnknownPath},
		{"payload path not in schema (filter)", func(d *Declarative, w *Webhook) {
			d.Filter = &Filter{All: []Condition{{Path: "payload.nope", Eq: json.RawMessage(`1`)}}}
		}, ErrUnknownPath},
		{"payload path not in schema (idempotency)", func(d *Declarative, w *Webhook) {
			d.Idempotency = "{{payload.nope}}"
		}, ErrUnknownPath},
		{"path valid in created but not in updated", func(d *Declarative, w *Webhook) {
			d.Transform = &Transform{Body: map[string]any{"x": "{{payload.address.city}}"}}
		}, ErrUnknownPath},
		{"event field", func(d *Declarative, w *Webhook) {
			d.Transform = &Transform{Body: map[string]any{"x": "{{event.nope}}"}}
		}, ErrUnknownPath},
		{"event without schema", func(d *Declarative, w *Webhook) {
			w.Events = StringSlice{"orders.order.created"}
			d.Transform = &Transform{Body: map[string]any{"x": "{{payload.id}}"}}
		}, ErrNoSchema},
		{"bad ordering", func(d *Declarative, w *Webhook) { d.Ordering = "global" }, ErrInvalidDeclarative},
		{"bad method", func(d *Declarative, w *Webhook) { d.Method = "DELETE" }, ErrInvalidDeclarative},
		{"bad event pattern", func(d *Declarative, w *Webhook) { w.Events = StringSlice{"a b"} }, ErrInvalidDeclarative},
		{"bad signing profile", func(d *Declarative, w *Webhook) {
			d.Signing = &signing.Spec{Profile: "rot13", SecretRef: "v2.signing_secret"}
		}, ErrInvalidDeclarative},
		{"secret value instead of reference", func(d *Declarative, w *Webhook) {
			d.Signing = &signing.Spec{Profile: signing.HMACSimple, SecretRef: "my secret value!"}
		}, ErrInvalidDeclarative},
		{"secret smuggled in a header", func(d *Declarative, w *Webhook) {
			d.Headers = map[string]string{"X-Api-Key": "abc"}
		}, ErrInvalidDeclarative},
		{"authorization header", func(d *Declarative, w *Webhook) {
			d.Headers = map[string]string{"Authorization": "Bearer abc"}
		}, ErrInvalidDeclarative},
		{"header injection", func(d *Declarative, w *Webhook) {
			d.Headers = map[string]string{"X-Contract": "1\r\nX-Evil: 1"}
		}, ErrInvalidDeclarative},
		{"timeout too large", func(d *Declarative, w *Webhook) { w.TimeoutSec = 9999 }, ErrInvalidDeclarative},
	}
	for _, c := range cases {
		if err := mk(c.mut); !errors.Is(err, c.is) {
			t.Errorf("%s: got %v, want %v", c.name, err, c.is)
		}
	}
	// y una configuracion correcta pasa
	if err := mk(func(d *Declarative, w *Webhook) {
		d.Transform = &Transform{Body: map[string]any{"x": "{{payload.name}}", "y": "{{event.id}}"}}
		w.Events = StringSlice{"customers.*"}
	}); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestSaveRejectsOnUpdateToo(t *testing.T) {
	e := newEnv(t, nil)
	w := e.create(t, e.sub(&Declarative{Enabled: true}))
	w.Declarative.Transform = &Transform{Body: map[string]any{"x": "{{payload.nope}}"}}
	if err := e.svc.Update(context.Background(), w); !errors.Is(err, ErrUnknownPath) {
		t.Fatalf("Update must validate: %v", err)
	}
}

func TestPayloadPathsNeedASchemaSource(t *testing.T) {
	db := setupTestDB(t)
	svc := New(Config{DB: db, UnsafeAllowPrivateTargets: true}) // sin Schemas
	mk := func(body string) error {
		return svc.Create(context.Background(), &Webhook{URL: "https://x.example.com/h", Events: StringSlice{"a.b"},
			Active: true, OwnerType: "o", OwnerID: uuid.New(),
			Declarative: &Declarative{Transform: &Transform{Body: map[string]any{"x": body}}}})
	}
	if err := mk("{{payload.id}}"); !errors.Is(err, ErrNoSchema) {
		t.Fatalf("payload path without schema must fail closed: %v", err)
	}
	if err := mk("{{event.id}}"); err != nil {
		t.Fatalf("event paths need no schema: %v", err)
	}
}

func TestSigningRequiresSecretResolver(t *testing.T) {
	db := setupTestDB(t)
	svc := New(Config{DB: db, UnsafeAllowPrivateTargets: true})
	err := svc.Create(context.Background(), &Webhook{URL: "https://x.example.com/h", Events: StringSlice{"a.b"},
		Active: true, OwnerType: "o", OwnerID: uuid.New(),
		Declarative: &Declarative{Signing: &signing.Spec{Profile: signing.Bearer, KeyRef: "v2.api_key"}}})
	if !errors.Is(err, ErrNoSecretResolver) {
		t.Fatalf("got %v", err)
	}
}

func TestSaveBlocksInternalTargets(t *testing.T) {
	db := setupTestDB(t)
	svc := New(Config{DB: db}) // politica real
	for _, u := range []string{"http://x.example.com/h", "https://127.0.0.1/h", "https://10.1.2.3/h", "https://metadata.internal/h", "https://localhost/h"} {
		err := svc.Create(context.Background(), &Webhook{URL: u, Events: StringSlice{"a.b"}, Active: true,
			OwnerType: "o", OwnerID: uuid.New(), Declarative: &Declarative{}})
		if !errors.Is(err, ErrBlockedTarget) {
			t.Errorf("%s: got %v", u, err)
		}
	}
	// un webhook clasico conserva su validacion historica (http permitido)
	if err := svc.Create(context.Background(), &Webhook{URL: "http://legacy.example.com/h", Events: StringSlice{"a.b"},
		Active: true, OwnerType: "o", OwnerID: uuid.New()}); err != nil {
		t.Fatalf("classic webhook must keep working: %v", err)
	}
	if err := svc.Create(context.Background(), &Webhook{URL: "https://api.example.com/h", Events: StringSlice{"a.b"},
		Active: true, OwnerType: "o", OwnerID: uuid.New(), Declarative: &Declarative{}}); err != nil {
		t.Fatalf("public https target rejected: %v", err)
	}
}

// El cliente de salida por omision (netguard) tambien bloquea AL CONECTAR,
// aunque el destino se hubiera guardado (p. ej. DNS que luego apunta a 127.0.0.1).
func TestDefaultEgressClientBlocksLoopbackAtDialTime(t *testing.T) {
	db := setupTestDB(t)
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	clk := newClock()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the loopback server must never be reached")
	}))
	defer srv.Close()
	svc := New(Config{DB: db, Clock: clk.Now, UnsafeAllowPrivateTargets: true}) // egress = netguard por omision
	org := uuid.New()
	w := &Webhook{URL: srv.URL, Events: StringSlice{"a.b"}, Active: true, OwnerType: "organization", OwnerID: org,
		Declarative: &Declarative{Enabled: true}, RetryMax: 1}
	if err := svc.Create(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.TriggerEvent(context.Background(), "organization", org, Event{Name: "a.b", ID: "e1", Payload: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	svc.drainOnce(context.Background())
	var d WebhookDelivery
	db.First(&d, "webhook_id = ?", w.ID)
	if d.Succeeded || !strings.Contains(d.ErrorMessage, "not allowed") {
		t.Fatalf("expected a netguard block, got %+v", d)
	}
}

// --- secretos -------------------------------------------------------------

func TestSecretsNeverPersistedNorLogged(t *testing.T) {
	e := newEnv(t, v2Spec(), 500, 201)
	w := e.create(t, e.sub(v2Decl()))
	if w.Secret != "" {
		t.Fatal("a subscription that signs by profile must not generate/store a secret")
	}
	e.publish(t, "customers.customer.created", customerPayload("evt-s", "cus-1", nil))
	e.drain(1)
	e.clk.Advance(time.Minute)
	e.drain(1)

	// volcado de TODAS las filas de ambas tablas
	var dump strings.Builder
	for _, tbl := range []string{"webhooks", "webhook_deliveries"} {
		rows, err := e.db.Raw("SELECT * FROM " + tbl).Rows()
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			_ = rows.Scan(ptrs...)
			for _, v := range vals {
				switch x := v.(type) {
				case []byte:
					dump.Write(x)
				case string:
					dump.WriteString(x)
				}
				dump.WriteByte('|')
			}
		}
		rows.Close()
	}
	for _, secret := range []string{testHMACSecret, testBearer, "abcdef-bearer-secret"} {
		if strings.Contains(dump.String(), secret) {
			t.Fatalf("secret %q found persisted", secret)
		}
	}
	// ... pero la referencia si
	if !strings.Contains(dump.String(), "v2.signing_secret") {
		t.Fatal("the reference should be stored")
	}
	// API JSON del webhook: ni secreto ni valores
	b, _ := json.Marshal(w)
	if strings.Contains(string(b), testHMACSecret) || strings.Contains(string(b), testBearer) {
		t.Fatal("secret leaked in JSON")
	}
}

func TestUnresolvableSecretFailsDeliveryWithoutLeakingAndRetries(t *testing.T) {
	e := newEnv(t, nil)
	d := v2Decl()
	d.Signing = &signing.Spec{Profile: signing.Bearer, KeyRef: "missing.ref"}
	w := e.create(t, e.sub(d))
	e.publish(t, "customers.customer.created", customerPayload("evt-q", "cus-1", nil))
	e.drain(1)
	rows := e.deliveries(t, w)
	if rows[0].Succeeded || rows[0].NextAttemptAt == nil || !strings.Contains(rows[0].ErrorMessage, "missing.ref") {
		t.Fatalf("%+v", rows[0])
	}
	if len(e.tg.Hits()) != 0 {
		t.Fatal("must not send unsigned")
	}
}

// --- datos personales -----------------------------------------------------

func TestPurgeDeadPayloads(t *testing.T) {
	e := newEnv(t, nil, 422)
	w := e.create(t, e.sub(&Declarative{Enabled: true}))
	e.publish(t, "customers.customer.created", customerPayload("evt-z", "cus-1", nil))
	e.drain(1)
	if n, _ := e.svc.PurgeDeadPayloads(context.Background(), w.ID, e.clk.Now().Add(-time.Hour)); n != 0 {
		t.Fatalf("too recent: purged %d", n)
	}
	if n, err := e.svc.PurgeDeadPayloads(context.Background(), w.ID, time.Now().Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("purged %d err %v", n, err)
	}
	rows := e.deliveries(t, w)
	if len(rows[0].Payload) != 0 || !rows[0].PayloadPurged {
		t.Fatalf("%+v", rows[0])
	}
}

// --- compatibilidad hacia atras ------------------------------------------

func TestClassicWebhookUnchangedAndNotFedByBridge(t *testing.T) {
	e := newEnv(t, nil)
	classic := e.create(t, e.sub(nil, "customers.*"))
	if classic.Secret == "" {
		t.Fatal("classic webhook keeps its generated secret")
	}
	// el bus NO alimenta webhooks clasicos
	e.publish(t, "customers.customer.created", customerPayload("evt-1", "cus-1", nil))
	if n := len(e.deliveries(t, classic)); n != 0 {
		t.Fatalf("bridge fed a classic webhook (%d)", n)
	}
	// Trigger explicito: cuerpo = payload tal cual, firma X-Metacore-*
	payload := JSONMap{"id": "c1", "name": "Ana"}
	if err := e.svc.Trigger(context.Background(), "customers.customer.created", "organization", e.org, payload); err != nil {
		t.Fatal(err)
	}
	e.drain(1)
	hits := e.tg.Hits()
	if len(hits) != 1 {
		t.Fatalf("hits %d", len(hits))
	}
	h := hits[0]
	var got map[string]any
	_ = json.Unmarshal(h.Body, &got)
	if !reflect.DeepEqual(got, map[string]any{"id": "c1", "name": "Ana"}) {
		t.Fatalf("classic body changed: %s", h.Body)
	}
	ts := h.Header.Get("X-Metacore-Timestamp")
	if h.Header.Get("X-Metacore-Signature") != computeSignature([]byte(classic.Secret), ts, h.Body) {
		t.Fatal("classic signature changed")
	}
	if h.Header.Get("X-Metacore-Event") != "customers.customer.created" || h.Header.Get("X-Metacore-Delivery") == "" {
		t.Fatal("classic headers changed")
	}
	if h.Header.Get("X-Idempotency-Key") != "" {
		t.Fatal("classic webhooks do not get the declarative idempotency header")
	}
	rows := e.deliveries(t, classic)
	if !rows[0].Succeeded || len(rows[0].Payload) == 0 || rows[0].PayloadPurged {
		t.Fatalf("classic delivery keeps its payload as before: %+v", rows[0])
	}
}

func TestTriggerAlsoServesDeclarativeSubscriptions(t *testing.T) {
	e := newEnv(t, nil)
	w := e.create(t, e.sub(&Declarative{Enabled: true, Transform: &Transform{Body: map[string]any{"n": "{{payload.name}}"}}}))
	if err := e.svc.Trigger(context.Background(), "customers.customer.created", "organization", e.org, JSONMap{"name": "Ana"}); err != nil {
		t.Fatal(err)
	}
	if rows := e.deliveries(t, w); len(rows) != 1 || rows[0].Payload["n"] != "Ana" {
		t.Fatalf("%+v", rows)
	}
}

func TestLegacyRowsFromTheOldSchemaLoadAsClassic(t *testing.T) {
	e := newEnv(t, nil)
	id, did := uuid.New(), uuid.New()
	// fila escrita por una version anterior: sin las columnas nuevas
	if err := e.db.Exec(`INSERT INTO webhooks (id, name, url, events, secret, active, retry_max, timeout_sec, owner_type, owner_id)
		VALUES (?, 'old', 'https://old.example.com/h', '["order.*"]', 's3', 1, 3, 15, 'organization', ?)`, id, e.org).Error; err != nil {
		t.Fatal(err)
	}
	if err := e.db.Exec(`INSERT INTO webhook_deliveries (id, webhook_id, event, payload, next_attempt_at)
		VALUES (?, ?, 'order.created', '{"a":1}', ?)`, did, id, e.clk.Now().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	w, err := e.svc.Get(context.Background(), id)
	if err != nil || w.Declarative != nil || w.Secret != "s3" {
		t.Fatalf("legacy row: %+v err %v", w, err)
	}
	var d WebhookDelivery
	if err := e.db.First(&d, "id = ?", did).Error; err != nil || d.Held || d.InFlight || d.PayloadPurged || d.OrderingKey != "" {
		t.Fatalf("legacy delivery: %+v err %v", d, err)
	}
}

func TestClassicRetryAndBackoffUnchanged(t *testing.T) {
	e := newEnv(t, nil, 500, 201)
	w := e.create(t, e.sub(nil, "order.created"))
	_ = e.svc.Trigger(context.Background(), "order.created", "organization", e.org, JSONMap{"id": "o1"})
	e.drain(1)
	rows := e.deliveries(t, w)
	if rows[0].AttemptCount != 1 || rows[0].NextAttemptAt == nil {
		t.Fatalf("%+v", rows[0])
	}
	if d := rows[0].NextAttemptAt.Sub(e.clk.Now()); d != 4*time.Second {
		t.Fatalf("classic backoff changed: %v", d)
	}
	e.clk.Advance(5 * time.Second)
	e.drain(1)
	if rows = e.deliveries(t, w); !rows[0].Succeeded || len(rows[0].Payload) == 0 {
		t.Fatalf("%+v", rows[0])
	}
}

// --- puente y bus ---------------------------------------------------------

func TestBridgeSurfacesDurableFailure(t *testing.T) {
	e := newEnv(t, nil)
	e.create(t, e.sub(&Declarative{Enabled: true}))
	if err := e.db.Exec("DROP TABLE webhooks").Error; err != nil {
		t.Fatal(err)
	}
	err := e.bus.Publish(context.Background(), "kernel", "customers.customer.created", e.org, customerPayload("evt-1", "cus-1", nil))
	if !errors.Is(err, events.ErrDurableDelivery) {
		t.Fatalf("a failure to record must reach the durable publisher, got %v", err)
	}
}

func TestBridgeIsolatesOrganizations(t *testing.T) {
	e := newEnv(t, nil)
	w := e.create(t, e.sub(&Declarative{Enabled: true}))
	other := uuid.New()
	if err := e.bus.Publish(context.Background(), "kernel", "customers.customer.created", other, customerPayload("evt-1", "cus-1", nil)); err != nil {
		t.Fatal(err)
	}
	if n := len(e.deliveries(t, w)); n != 0 {
		t.Fatalf("events of another organization leaked: %d", n)
	}
	if got := e.bus.EstimateSubscribers(context.Background(), e.org, "customers.customer.created"); got != 1 {
		t.Fatalf("estimate = %d", got)
	}
	if got := e.bus.EstimateSubscribers(context.Background(), other, "customers.customer.created"); got != 0 {
		t.Fatalf("estimate for other org = %d", got)
	}
}

func TestMultipleSubscriptionsGetIndependentDeliveries(t *testing.T) {
	e := newEnv(t, nil)
	a := e.create(t, e.sub(&Declarative{Enabled: true, Transform: &Transform{Body: map[string]any{"n": "{{payload.name | trim}}"}}}))
	b := e.create(t, e.sub(&Declarative{Enabled: true, Filter: &Filter{All: []Condition{{Path: "payload.channel", Eq: json.RawMessage(`"pos"`)}}}}))
	e.publish(t, "customers.customer.created", customerPayload("evt-m", "cus-1", nil))
	if len(e.deliveries(t, a)) != 1 || len(e.deliveries(t, b)) != 0 {
		t.Fatal("each subscription filters on its own")
	}
}

// --- concurrencia ---------------------------------------------------------

func TestWorkersDoNotDoubleSend(t *testing.T) {
	e := newEnv(t, nil)
	e.create(t, e.sub(&Declarative{Enabled: true}))
	for i := 0; i < 5; i++ {
		e.publish(t, "customers.customer.created", customerPayload("evt-"+string(rune('a'+i)), "cus-"+string(rune('a'+i)), nil))
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				e.svc.drainOnce(ctx)
			}
		}()
	}
	wg.Wait()
	cancel()
	if n := len(e.tg.Hits()); n != 5 {
		t.Fatalf("hits = %d, want 5 (each exactly once)", n)
	}
}

func TestOrphanedInFlightIsRequeued(t *testing.T) {
	e := newEnv(t, nil)
	w := e.create(t, e.sub(&Declarative{Enabled: true}))
	e.publish(t, "customers.customer.created", customerPayload("evt-o", "cus-1", nil))
	// un worker reclamo la entrega y murio: next_attempt_at NULL + in_flight
	e.db.Exec("UPDATE webhook_deliveries SET next_attempt_at = NULL, in_flight = 1")
	e.drain(1)
	if len(e.tg.Hits()) != 0 {
		t.Fatal("still within the lease")
	}
	e.clk.Advance(inFlightLease + time.Minute)
	// updated_at (reloj real) queda "reciente" respecto al reloj de prueba hacia adelante: forzar antiguedad
	e.db.Exec("UPDATE webhook_deliveries SET updated_at = ?", e.clk.Now().Add(-time.Hour))
	e.drain(2)
	if len(e.tg.Hits()) != 1 {
		t.Fatalf("orphan not recovered: %d hits, rows %+v", len(e.tg.Hits()), e.deliveries(t, w))
	}
}

// compilacion: el cliente TLS de prueba se usa tambien con transporte propio
var _ = tls.VersionTLS12
