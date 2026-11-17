package webhooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/webhooks/signing"
)

// --- A1: aislamiento por propietario, por HTTP ---------------------------

type httpEnv struct {
	*testEnv
	app *fiber.App
}

func newHTTPEnv(t *testing.T, script ...int) *httpEnv {
	t.Helper()
	e := newEnv(t, nil, script...)
	h := NewHandler(e.svc, func(c fiber.Ctx) (string, uuid.UUID, error) {
		id, err := uuid.Parse(c.Get("X-Owner"))
		if err != nil {
			return "", uuid.Nil, errors.New("no owner")
		}
		return "organization", id, nil
	})
	app := fiber.New()
	h.Mount(app.Group("/webhooks"))
	return &httpEnv{testEnv: e, app: app}
}

func (e *httpEnv) do(t *testing.T, owner uuid.UUID, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, "/webhooks"+path, rd)
	req.Header.Set("X-Owner", owner.String())
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := e.app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (e *httpEnv) createBody() map[string]any {
	return map[string]any{
		"name": "mine", "url": e.tg.URL + testPath, "events": []string{"customers.*"},
		"declarative": map[string]any{"payload_fields": []string{"id", "name"}, "enabled": true},
	}
}

func (e *httpEnv) createAs(t *testing.T, owner uuid.UUID) uuid.UUID {
	t.Helper()
	st, out := e.do(t, owner, "POST", "/", e.createBody())
	if st != 200 {
		t.Fatalf("create: %d %v", st, out)
	}
	return uuid.MustParse(out["data"].(map[string]any)["id"].(string))
}

func TestHandlerOwnerIsolationOnEveryRoute(t *testing.T) {
	e := newHTTPEnv(t, 500)
	a, b := e.org, e.other
	wid := e.createAs(t, a)

	// a (failed, queued for retry) delivery of A
	e.publish(t, "customers.customer.created", customerPayload("evt-1", "cus-1", nil))
	e.drain(1)
	var d WebhookDelivery
	if err := e.db.First(&d, "webhook_id = ?", wid).Error; err != nil {
		t.Fatal(err)
	}
	hitsBefore := len(e.tg.Hits())
	did := d.ID.String()

	// B tries every route against A's ids
	attacks := []struct {
		method, path string
		body         any
	}{
		{"GET", "/" + wid.String(), nil},
		{"PUT", "/" + wid.String(), map[string]any{"name": "pwned", "url": "https://evil.example.com/x"}},
		{"DELETE", "/" + wid.String(), nil},
		{"POST", "/" + wid.String() + "/test", nil},
		{"GET", "/" + wid.String() + "/logs", nil},
		{"POST", "/" + wid.String() + "/enable", nil},
		{"POST", "/" + wid.String() + "/disable", nil},
		{"POST", "/logs/" + did + "/replay", nil},
		{"POST", "/logs/" + did + "/requeue", nil},
	}
	for _, at := range attacks {
		st, _ := e.do(t, b, at.method, at.path, at.body)
		if st != 404 {
			t.Errorf("B %s %s = %d, want 404", at.method, at.path, st)
		}
	}
	// nothing changed, nothing was sent on B's behalf
	got, err := e.svc.Get(context.Background(), wid)
	if err != nil || got.Name != "mine" || !got.IsEnabled() || got.URL != e.tg.URL+testPath {
		t.Fatalf("A's webhook was touched: %+v %v", got, err)
	}
	if n := len(e.tg.Hits()); n != hitsBefore {
		t.Fatalf("B triggered a delivery of A's webhook (%d -> %d hits)", hitsBefore, n)
	}
	var d2 WebhookDelivery
	e.db.First(&d2, "id = ?", d.ID)
	if d2.AttemptCount != d.AttemptCount || d2.NextAttemptAt == nil {
		t.Fatalf("A's delivery was touched: %+v", d2)
	}
	if _, out := e.do(t, b, "GET", "/", nil); len(out["data"].([]any)) != 0 {
		t.Fatalf("B lists A's webhooks: %v", out)
	}
	if _, out := e.do(t, a, "GET", "/", nil); len(out["data"].([]any)) != 1 {
		t.Fatalf("A does not list its own: %v", out)
	}

	// the owner still can
	for _, ok := range []struct {
		method, path string
		code         int
	}{
		{"GET", "/" + wid.String(), 200}, {"GET", "/" + wid.String() + "/logs", 200},
		{"POST", "/" + wid.String() + "/disable", 200}, {"POST", "/" + wid.String() + "/enable", 200},
	} {
		if st, _ := e.do(t, a, ok.method, ok.path, nil); st != ok.code {
			t.Errorf("A %s %s = %d, want %d", ok.method, ok.path, st, ok.code)
		}
	}
	if st, _ := e.do(t, a, "DELETE", "/"+wid.String(), nil); st != 200 {
		t.Errorf("A delete = %d", st)
	}
	if st, _ := e.do(t, a, "GET", "/"+wid.String(), nil); st != 404 {
		t.Errorf("deleted webhook should be 404, got %d", st)
	}
}

func TestHandlerMassAssignmentIsIgnored(t *testing.T) {
	e := newHTTPEnv(t)
	a, b := e.org, e.other

	// create: id / owner / organization from the body are ignored
	body := e.createBody()
	forged := uuid.New()
	body["id"], body["owner_id"], body["owner_type"], body["organization_id"] = forged.String(), b.String(), "device", b.String()
	body["secret"], body["consecutive_failures"], body["success_count"] = "chosen-secret", 99, 99
	st, out := e.do(t, a, "POST", "/", body)
	if st != 200 {
		t.Fatalf("create: %d %v", st, out)
	}
	wid := uuid.MustParse(out["data"].(map[string]any)["id"].(string))
	if wid == forged {
		t.Fatal("client-chosen id accepted")
	}
	w, _ := e.svc.Get(context.Background(), wid)
	if w.OwnerID != a || w.OwnerType != "organization" || w.OrganizationID != a || w.SuccessCount != 0 || w.ConsecutiveFailures != 0 || w.Secret == "chosen-secret" {
		t.Fatalf("create took forbidden fields: %+v", w)
	}

	// update: only the allow-list changes
	e.db.Exec("UPDATE webhooks SET success_count = 7, consecutive_failures = 2 WHERE id = ?", wid)
	st, _ = e.do(t, a, "PUT", "/"+wid.String(), map[string]any{
		"name": "renamed", "owner_id": b.String(), "owner_type": "device", "organization_id": b.String(), "id": uuid.NewString(),
		"success_count": 99, "failure_count": 99, "consecutive_failures": 0, "auto_disabled_at": time.Now(), "last_error": "x",
		"secret": "pwned", "declarative": map[string]any{"headers": map[string]any{"X-Contract": "2"}},
	})
	if st != 200 {
		t.Fatalf("update: %d", st)
	}
	w, _ = e.svc.Get(context.Background(), wid)
	if w.Name != "renamed" || w.OwnerID != a || w.OwnerType != "organization" || w.SuccessCount != 7 || w.ConsecutiveFailures != 2 ||
		w.AutoDisabledAt != nil || w.LastError != "" || w.Secret == "pwned" || w.FailureCount != 0 {
		t.Fatalf("update took forbidden fields: %+v", w)
	}
	// partial declarative update keeps what it does not mention (and still valid)
	if !w.Declarative.Enabled || len(w.Declarative.PayloadFields) != 2 || w.Declarative.Headers["X-Contract"] != "2" {
		t.Fatalf("declarative merge: %+v", w.Declarative)
	}
	// B still sees nothing
	if _, out := e.do(t, b, "GET", "/", nil); len(out["data"].([]any)) != 0 {
		t.Fatal("webhook leaked to B")
	}
	// a declarative subscription cannot be downgraded to a classic one
	if st, _ := e.do(t, a, "PUT", "/"+wid.String(), map[string]any{"declarative": nil}); st != 400 {
		t.Fatalf("declarative:null = %d, want 400", st)
	}
	// PUT enabled:true clears an auto-disable
	e.db.Exec("UPDATE webhooks SET auto_disabled_at = ?, consecutive_failures = 10 WHERE id = ?", time.Now(), wid)
	if st, _ := e.do(t, a, "PUT", "/"+wid.String(), map[string]any{"declarative": map[string]any{"enabled": true}}); st != 200 {
		t.Fatal("put enabled")
	}
	w, _ = e.svc.Get(context.Background(), wid)
	if !w.IsEnabled() || w.ConsecutiveFailures != 0 || w.AutoDisabledAt != nil {
		t.Fatalf("enabling via PUT must clear the health trip: %+v", w)
	}
}

// --- A2: secretos entre organizaciones -----------------------------------

func TestSecretRefsAreScopedToTheOwner(t *testing.T) {
	e := newEnv(t, nil)
	mk := func(owner uuid.UUID, ref string) error {
		w := e.sub(evDeclSigned(&signing.Spec{Profile: signing.Bearer, KeyRef: ref}))
		w.OwnerID = owner
		return e.svc.Create(context.Background(), w)
	}
	if err := mk(e.org, "v2.api_key"); err != nil {
		t.Fatalf("own ref: %v", err)
	}
	if err := mk(e.other, "other.api_key"); err != nil {
		t.Fatalf("own ref (other org): %v", err)
	}
	// the other org names A's reference: refused at save, fail closed
	if err := mk(e.other, "v2.api_key"); !errors.Is(err, ErrSecretRef) {
		t.Fatalf("cross-org ref accepted: %v", err)
	}
	if err := mk(e.org, "other.api_key"); !errors.Is(err, ErrSecretRef) {
		t.Fatalf("cross-org ref accepted: %v", err)
	}
	if err := mk(e.org, "does.not.exist"); !errors.Is(err, ErrSecretRef) {
		t.Fatalf("unknown ref accepted: %v", err)
	}
	// Update cannot smuggle it in either
	w := e.create(t, e.sub(evDeclSigned(&signing.Spec{Profile: signing.Bearer, KeyRef: "v2.api_key"})))
	w.Declarative.Signing.KeyRef = "other.api_key"
	if err := e.svc.Update(context.Background(), w); !errors.Is(err, ErrSecretRef) {
		t.Fatalf("update with foreign ref: %v", err)
	}
}

func TestForeignSecretRefNeverReachesTheWire(t *testing.T) {
	e := newEnv(t, nil)
	d := evDeclSigned(&signing.Spec{Profile: signing.Bearer, KeyRef: "v2.api_key"})
	d.Enabled = true
	w := e.create(t, e.sub(d))
	// tamper with the stored row (bypassing validation): now it names the other org's key
	if err := e.db.Exec("UPDATE webhooks SET declarative = REPLACE(declarative, 'v2.api_key', 'other.api_key') WHERE id = ?", w.ID).Error; err != nil {
		t.Fatal(err)
	}
	e.publish(t, "customers.customer.created", customerPayload("evt-x", "cus-1", nil))
	e.drain(1)
	for _, h := range e.tg.Hits() {
		if strings.Contains(h.Header.Get("Authorization"), otherOrgToken) {
			t.Fatal("another organization's token was sent to this owner's server")
		}
	}
	if n := len(e.tg.Hits()); n != 0 {
		t.Fatalf("a delivery whose secret cannot be scoped must not be sent (%d hits)", n)
	}
	row := e.deliveries(t, w)[0]
	if row.Succeeded || !strings.Contains(row.ErrorMessage, "could not be resolved") || strings.Contains(row.ErrorMessage, otherOrgToken) {
		t.Fatalf("%+v", row)
	}
}

// --- A3: inanicion --------------------------------------------------------

func TestBlockedDeliveriesDoNotStarveOthers(t *testing.T) {
	e := newEnv(t, nil)
	a := e.create(t, e.sub(&Declarative{PayloadFields: []string{"id"}, Enabled: true, Ordering: OrderingPerAggregate}))
	sb := e.sub(&Declarative{PayloadFields: []string{"id"}, Enabled: true})
	sb.OwnerID = e.other
	b := e.create(t, sb)
	now := e.clk.Now()
	future := now.Add(time.Hour)
	// A: one head far in the future (a live predecessor) and 80 successors that are
	// due earlier than B's delivery but blocked by it.
	mk := func(w *Webhook, key string, seq int64, due time.Time, order string) {
		d := &WebhookDelivery{WebhookID: w.ID, Event: "customers.customer.created", Payload: JSONMap{"x": 1},
			IdempotencyKey: key, OrderingKey: order, Seq: seq, NextAttemptAt: &due}
		if err := e.db.Create(d).Error; err != nil {
			t.Fatal(err)
		}
	}
	mk(a, "head", 1, future, "agg:1")
	for i := 0; i < 80; i++ {
		mk(a, "blocked", int64(2+i), now.Add(-2*time.Minute), "agg:1")
	}
	mk(b, "victim", 1000, now.Add(-time.Minute), "")
	e.drain(1)
	hits := e.tg.Hits()
	if len(hits) != 1 || hits[0].Header.Get("X-Idempotency-Key") != "victim" {
		t.Fatalf("blocked rows starved another tenant: %d hits", len(hits))
	}
}

func TestBatchAndConcurrencyCaps(t *testing.T) {
	e := newEnv(t, nil)
	e.svc.batch = 10 // default env uses 1 for determinism
	w1 := e.create(t, e.sub(&Declarative{PayloadFields: []string{"id"}, Enabled: true}))
	for i := 0; i < 5; i++ {
		e.publish(t, "customers.customer.created", customerPayload("evt-"+string(rune('a'+i)), "cus-"+string(rune('a'+i)), nil))
	}
	e.drain(1)
	if n := len(e.tg.Hits()); n != 2 {
		t.Fatalf("PerWebhookConcurrency=2: got %d in one tick", n)
	}
	e.drain(1)
	if n := len(e.tg.Hits()); n != 4 {
		t.Fatalf("second tick: %d", n)
	}
	e.drain(1)
	if n := len(e.tg.Hits()); n != 5 {
		t.Fatalf("third tick: %d", n)
	}
	_ = w1

	// per owner: 3 subscriptions x 2 per tick would be 6; owner cap is 4
	e2 := newEnv(t, nil)
	e2.svc.batch = 10
	for i := 0; i < 3; i++ {
		e2.create(t, e2.sub(&Declarative{PayloadFields: []string{"id"}, Enabled: true}))
	}
	for i := 0; i < 4; i++ {
		e2.publish(t, "customers.customer.created", customerPayload("evt-"+string(rune('a'+i)), "cus-"+string(rune('a'+i)), nil))
	}
	e2.drain(1)
	if n := len(e2.tg.Hits()); n != 4 {
		t.Fatalf("PerOwnerConcurrency=4: got %d in one tick", n)
	}
}

func TestSubscriptionCapPerOwner(t *testing.T) {
	e := newEnv(t, nil)
	e.svc.maxSubs = 2
	e.create(t, e.sub(evDecl()))
	e.create(t, e.sub(evDecl()))
	if err := e.svc.Create(context.Background(), e.sub(evDecl())); !errors.Is(err, ErrTooManySubs) {
		t.Fatalf("got %v", err)
	}
	// another owner is unaffected, and classic webhooks are not counted
	o := e.sub(evDecl())
	o.OwnerID = e.other
	if err := e.svc.Create(context.Background(), o); err != nil {
		t.Fatalf("other owner: %v", err)
	}
	if err := e.svc.Create(context.Background(), e.sub(nil)); err != nil {
		t.Fatalf("classic: %v", err)
	}
}

// --- A4: zombis y borrado ------------------------------------------------

func TestDeleteCancelsPendingDeliveriesAndErasesPayload(t *testing.T) {
	e := newEnv(t, nil)
	held := e.create(t, e.sub(&Declarative{PayloadFields: []string{"id", "name"}, Enabled: false}))
	live := e.create(t, e.sub(&Declarative{PayloadFields: []string{"id", "name"}, Enabled: true}))
	e.publish(t, "customers.customer.created", customerPayload("evt-1", "cus-1", nil))
	e.publish(t, "customers.customer.updated", customerPayload("evt-2", "cus-1", nil))
	for _, w := range []*Webhook{held, live} {
		if err := e.svc.Delete(context.Background(), w.ID); err != nil {
			t.Fatal(err)
		}
	}
	var rows []WebhookDelivery
	e.db.Find(&rows)
	if len(rows) != 4 {
		t.Fatalf("rows %d", len(rows))
	}
	for _, r := range rows {
		if len(r.Payload) != 0 || !r.PayloadPurged || r.Held || r.NextAttemptAt != nil || r.InFlight || r.Succeeded {
			t.Fatalf("pending delivery of a deleted webhook must be cancelled and its PII erased: %+v", r)
		}
	}
	e.clk.Advance(time.Hour)
	e.drain(5)
	if len(e.tg.Hits()) != 0 {
		t.Fatal("sent for a deleted webhook")
	}
}

func TestOrphanDeliveryOfAGoneWebhookIsPurgedNotRequeuedForever(t *testing.T) {
	e := newEnv(t, nil)
	w := e.create(t, e.sub(&Declarative{PayloadFields: []string{"id", "name"}, Enabled: true}))
	e.publish(t, "customers.customer.created", customerPayload("evt-1", "cus-1", nil))
	// the webhook disappears without going through Service.Delete (soft delete in the DB)
	if err := e.db.Delete(&Webhook{}, "id = ?", w.ID).Error; err != nil {
		t.Fatal(err)
	}
	e.drain(2)
	var r WebhookDelivery
	e.db.First(&r, "webhook_id = ?", w.ID)
	if len(r.Payload) != 0 || !r.PayloadPurged || r.NextAttemptAt != nil || r.InFlight {
		t.Fatalf("zombie delivery: %+v", r)
	}
	// and the reaper has nothing to resurrect
	e.clk.Advance(inFlightLease + time.Hour)
	e.drain(2)
	e.db.First(&r, "webhook_id = ?", w.ID)
	if r.NextAttemptAt != nil || len(e.tg.Hits()) != 0 {
		t.Fatalf("zombie resurrected: %+v", r)
	}
}

// --- M1: SSRF -------------------------------------------------------------

func TestTargetIsRevalidatedBeforeEverySend(t *testing.T) {
	e := newEnv(t, nil)
	w := e.create(t, e.sub(&Declarative{PayloadFields: []string{"id", "name"}, Enabled: true}))
	e.publish(t, "customers.customer.created", customerPayload("evt-1", "cus-1", nil))
	// the host tightens its policy after the row was saved: the internal target is no longer authorized
	strict := New(Config{DB: e.db, Clock: e.clk.Now, BatchSize: 1, Schemas: testSchemas(), Secrets: e.sec,
		AllowInternalHosts: nil})
	strict.drainOnce(context.Background())
	if len(e.tg.Hits()) != 0 {
		t.Fatal("sent to a target the current policy forbids")
	}
	row := e.deliveries(t, w)[0]
	if row.NextAttemptAt != nil || !strings.Contains(row.ErrorMessage, "no longer allowed") {
		t.Fatalf("%+v", row)
	}
	if got := reload(t, e, w); got.ConsecutiveFailures != 0 {
		t.Fatal("a blocked target is not the destination's fault")
	}
}

func TestClassicWebhooksUseTheGuardedClientByDefault(t *testing.T) {
	db := setupTestDB(t)
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	var reached bool
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	defer srv.Close()
	svc := New(Config{DB: db}) // default client: no host-supplied one
	org := uuid.New()
	w := &Webhook{URL: srv.URL, Events: StringSlice{"a.b"}, Active: true, OwnerType: "organization", OwnerID: org, RetryMax: 1}
	if err := svc.Create(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	if err := svc.Trigger(context.Background(), "a.b", "organization", org, JSONMap{"x": 1}); err != nil {
		t.Fatal(err)
	}
	svc.drainOnce(context.Background())
	var d WebhookDelivery
	db.First(&d, "webhook_id = ?", w.ID)
	if reached || d.Succeeded || !strings.Contains(d.ErrorMessage, "not allowed") {
		t.Fatalf("classic default client must refuse loopback at connect time (reached=%v): %+v", reached, d)
	}
}

func TestHostSuppliedClientStillWorksForClassic(t *testing.T) {
	e := newEnv(t, nil) // HTTPClient = tg.Client()
	w := e.create(t, e.sub(nil, "order.created"))
	_ = e.svc.Trigger(context.Background(), "order.created", "organization", e.org, JSONMap{"id": "1"})
	e.drain(1)
	if rows := e.deliveries(t, w); !rows[0].Succeeded {
		t.Fatalf("%+v", rows[0])
	}
}

func TestDeclarativeDeliveryIgnoresTheHostClient(t *testing.T) {
	// A host client that would accept anything must not be used for declarative sends:
	// the service has no AllowInternalHosts here, so the loopback server stays unreachable.
	db := setupTestDB(t)
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	clk := newClock()
	tg := newTarget(t, clk, nil)
	svc := New(Config{DB: db, Clock: clk.Now, HTTPClient: tg.Client(), BatchSize: 1, UnsafeAllowPrivateTargets: true})
	org := uuid.New()
	w := &Webhook{URL: tg.URL, Events: StringSlice{"a.b"}, Active: true, OwnerType: "organization", OwnerID: org, RetryMax: 1,
		Declarative: &Declarative{Enabled: true, Transform: &Transform{Body: map[string]any{"x": "{{event.id}}"}}}}
	if err := svc.Create(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.TriggerEvent(context.Background(), "organization", org, Event{Name: "a.b", ID: "e1", Payload: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	svc.drainOnce(context.Background())
	if len(tg.Hits()) != 0 {
		t.Fatal("declarative delivery used the host-supplied client")
	}
}

// --- M2: deduplicacion ----------------------------------------------------

func TestConcurrentTriggerOfTheSameEventCreatesOneDelivery(t *testing.T) {
	e := newEnv(t, nil)
	w := e.create(t, e.sub(&Declarative{PayloadFields: []string{"id", "name"}, Enabled: true}))
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = e.svc.TriggerEvent(context.Background(), "organization", e.org, Event{
				Name: "customers.customer.created", ID: "evt-same", Payload: map[string]any{"id": "c", "name": "n"}})
		}()
	}
	wg.Wait()
	if n := len(e.deliveries(t, w)); n != 1 {
		t.Fatalf("concurrent duplicate created %d deliveries", n)
	}
}

func TestUniqueIndexAbsorbsDuplicateInsert(t *testing.T) {
	e := newEnv(t, nil)
	w := e.create(t, e.sub(&Declarative{PayloadFields: []string{"id", "name"}, Enabled: true}))
	mk := func() bool {
		ok, err := e.svc.createDelivery(context.Background(), &WebhookDelivery{WebhookID: w.ID, Event: "x", EventID: "same", Payload: JSONMap{}})
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !mk() || mk() {
		t.Fatal("second insert with the same (webhook, event_id) must be a no-op")
	}
	// rows without event_id (classic) are never constrained
	for i := 0; i < 2; i++ {
		if ok, _ := e.svc.createDelivery(context.Background(), &WebhookDelivery{WebhookID: w.ID, Event: "x", Payload: JSONMap{}}); !ok {
			t.Fatal("empty event_id must not collide")
		}
	}
}

// --- M3: texto no valido en la respuesta ---------------------------------

func TestInvalidUTF8AndNULInResponsesAreSanitized(t *testing.T) {
	for _, declarative := range []bool{true, false} {
		e := newEnv(t, nil, 500)
		e.tg.resp = "\xff\xfe bad \x00 nul \xc3 {\"unterminated"
		var sub *Webhook
		if declarative {
			sub = e.sub(&Declarative{PayloadFields: []string{"id", "name"}, Enabled: true})
		} else {
			sub = e.sub(nil, "customers.*")
		}
		w := e.create(t, sub)
		if declarative {
			e.publish(t, "customers.customer.created", customerPayload("evt-u", "cus-1", nil))
		} else {
			_ = e.svc.Trigger(context.Background(), "customers.customer.created", "organization", e.org, JSONMap{"id": "1"})
		}
		e.drain(1)
		r := e.deliveries(t, w)[0]
		if r.AttemptCount != 1 || r.InFlight || r.NextAttemptAt == nil || r.ResponseStatus != 500 {
			t.Fatalf("declarative=%v: the attempt must be recorded: %+v", declarative, r)
		}
		for _, s := range []string{r.ResponseBody, r.ErrorMessage} {
			if !utf8.ValidString(s) || strings.ContainsRune(s, 0) {
				t.Fatalf("declarative=%v: unsafe text stored: %q", declarative, s)
			}
		}
	}
}

func TestCleanText(t *testing.T) {
	if got := cleanText("a\x00b\xffc", 100); got != "abc" {
		t.Fatalf("%q", got)
	}
	long := strings.Repeat("é", 400) // 800 bytes
	got := cleanText(long, 500)
	if len(got) > 500 || !utf8.ValidString(got) || len(got) < 498 {
		t.Fatalf("rune-safe cut: %d bytes valid=%v", len(got), utf8.ValidString(got))
	}
	if cleanText("", 5) != "" || cleanText("hello", 5) != "hello" {
		t.Fatal("edge cases")
	}
}

// --- M4: llave de idempotencia y salud ------------------------------------

func TestPoisonedIdempotencyKeysAreDeadNotHealthFailures(t *testing.T) {
	e := newEnv(t, nil)
	sub := e.sub(&Declarative{PayloadFields: []string{"id", "name"}, Enabled: true, AutoDisableThreshold: 3})
	w := e.create(t, sub)
	for i := 0; i < 10; i++ {
		e.publish(t, "customers.customer.created", customerPayload("bad\r\nid-"+string(rune('a'+i)), "cus-"+string(rune('a'+i)), nil))
	}
	e.publish(t, "customers.customer.created", customerPayload(strings.Repeat("x", 300), "cus-long", nil))
	e.drain(5)
	if len(e.tg.Hits()) != 0 {
		t.Fatal("a poisoned key must never be sent")
	}
	rows := e.deliveries(t, w)
	if len(rows) != 11 {
		t.Fatalf("rows %d", len(rows))
	}
	for _, r := range rows {
		if !r.PayloadPurged || r.NextAttemptAt != nil || !strings.HasPrefix(r.ErrorMessage, "render:") || r.IdempotencyKey != "" {
			t.Fatalf("%+v", r)
		}
		if _, err := e.svc.Replay(context.Background(), r.ID); !errors.Is(err, ErrPayloadPurged) {
			t.Fatalf("replay of an unrendered delivery: %v", err)
		}
		if err := e.svc.Requeue(context.Background(), r.ID); !errors.Is(err, ErrPayloadPurged) {
			t.Fatalf("requeue of an unrendered delivery: %v", err)
		}
	}
	if got := reload(t, e, w); !got.IsEnabled() || got.ConsecutiveFailures != 0 {
		t.Fatalf("10 poisoned events must not switch the subscription off: %+v", got)
	}
}

func TestOnlyDestinationFailuresCountForHealth(t *testing.T) {
	cases := map[int]int{ // status -> consecutive_failures after one attempt
		500: 1, 502: 1, 429: 1, 408: 1,
		400: 0, 401: 0, 403: 0, 404: 0, 409: 0, 422: 0, 423: 0,
	}
	for status, want := range cases {
		e := newEnv(t, nil, status)
		w := e.create(t, e.sub(&Declarative{PayloadFields: []string{"id", "name"}, Enabled: true}))
		e.publish(t, "customers.customer.created", customerPayload("evt-h", "cus-1", nil))
		e.drain(1)
		if got := reload(t, e, w).ConsecutiveFailures; got != want {
			t.Errorf("status %d: consecutive_failures = %d, want %d", status, got, want)
		}
	}
	// transport errors count; Test() never does
	e := newEnv(t, nil, 500)
	w := e.create(t, e.sub(&Declarative{PayloadFields: []string{"id", "name"}, Enabled: true}))
	if _, err := e.svc.Test(context.Background(), w.ID); err != nil {
		t.Fatal(err)
	}
	if got := reload(t, e, w); got.ConsecutiveFailures != 0 || got.FailureCount != 1 {
		t.Fatalf("a test delivery must not trip the health check: %+v", got)
	}
	e.tg.Close() // connection refused from now on
	e.publish(t, "customers.customer.created", customerPayload("evt-n", "cus-1", nil))
	e.drain(1)
	if got := reload(t, e, w).ConsecutiveFailures; got != 1 {
		t.Fatalf("transport error must count: %d", got)
	}
}

func TestErrorsDoNotLeakTheTargetURL(t *testing.T) {
	e := newEnv(t, nil)
	w := e.create(t, e.sub(&Declarative{PayloadFields: []string{"id", "name"}, Enabled: true}))
	e.tg.Close()
	e.publish(t, "customers.customer.created", customerPayload("evt-n", "cus-1", nil))
	e.drain(1)
	r := e.deliveries(t, w)[0]
	if strings.Contains(r.ErrorMessage, testPath) || strings.Contains(r.ErrorMessage, "https://") {
		t.Fatalf("error message carries the URL: %q", r.ErrorMessage)
	}
	if r.ErrorMessage == "" {
		t.Fatal("expected an error message")
	}
}

// --- M5: tamano y alcance del cuerpo --------------------------------------

func TestBodyNeverContainsTheWholePayload(t *testing.T) {
	e := newEnv(t, nil)
	w := e.create(t, e.sub(&Declarative{PayloadFields: []string{"id", "name"}, Enabled: true}))
	e.publish(t, "customers.customer.created", customerPayload("evt-1", "cus-1", map[string]any{"ssn": "123-45-6789", "address": map[string]any{"city": "CDMX"}}))
	e.drain(1)
	b := string(e.tg.Hits()[0].Body)
	if strings.Contains(b, "123-45-6789") || strings.Contains(b, "CDMX") || strings.Contains(b, "email") || !strings.Contains(b, "cus-1") {
		t.Fatalf("only the listed fields may leave: %s", b)
	}
	_ = w
}

func TestSaveRequiresTransformOrFieldsAndScalarPaths(t *testing.T) {
	e := newEnv(t, nil)
	for name, d := range map[string]*Declarative{
		"nothing":        {Enabled: true},
		"whole payload":  {Transform: &Transform{Body: map[string]any{"x": "{{payload}}"}}},
		"object subtree": {Transform: &Transform{Body: map[string]any{"x": "{{payload.address}}"}}},
		"field subtree":  {PayloadFields: []string{"address"}},
		"bad field":      {PayloadFields: []string{"a b"}},
	} {
		if err := e.svc.Create(context.Background(), e.sub(d, "customers.customer.created")); err == nil {
			t.Errorf("%s: should be rejected", name)
		}
	}
	ok := &Declarative{Transform: &Transform{Body: map[string]any{"c": "{{payload.address.city}}"}}}
	if err := e.svc.Create(context.Background(), e.sub(ok, "customers.customer.created")); err != nil {
		t.Fatalf("scalar leaf rejected: %v", err)
	}
}

func TestOversizedPayloadAndAmplificationAreRecordedNotSent(t *testing.T) {
	e := newEnv(t, nil)
	amp := &Declarative{Enabled: true, Transform: &Transform{Body: map[string]any{
		"a": "{{payload.name}}", "b": "{{payload.name}}", "c": "{{payload.name}}", "d": "{{payload.name}}"}}}
	w1 := e.create(t, e.sub(amp))
	w2 := e.create(t, e.sub(&Declarative{PayloadFields: []string{"id"}, Enabled: true}))
	// 100 KB name: fits as a payload, but 4 copies exceed the rendered limit
	e.publish(t, "customers.customer.created", customerPayload("evt-amp", "cus-1", map[string]any{"name": strings.Repeat("x", 100<<10)}))
	// 300 KB payload: over the incoming limit for everybody
	e.publish(t, "customers.customer.created", customerPayload("evt-big", "cus-2", map[string]any{"name": strings.Repeat("y", 300<<10)}))
	e.drain(5)
	r1 := e.deliveries(t, w1)
	if len(r1) != 2 || !r1[0].PayloadPurged || !strings.Contains(r1[0].ErrorMessage, "size limit") {
		t.Fatalf("amplified body: %+v", r1)
	}
	r2 := e.deliveries(t, w2)
	if len(r2) != 2 || r2[0].PayloadPurged == false && r2[0].IdempotencyKey != "evt-amp" {
		t.Fatalf("%+v", r2)
	}
	if !r2[1].PayloadPurged || !strings.Contains(r2[1].ErrorMessage, "size limit") {
		t.Fatalf("oversized incoming payload: %+v", r2[1])
	}
}

// --- M6: retencion --------------------------------------------------------

func TestRetentionErasesHeldAndDeadPayloads(t *testing.T) {
	e := newEnv(t, nil, 422)
	e.svc.heldTTL, e.svc.deadTTL = time.Hour, 2*time.Hour
	held := e.create(t, e.sub(&Declarative{PayloadFields: []string{"id", "name"}, Enabled: false}))
	e.publish(t, "customers.customer.created", customerPayload("evt-held", "cus-1", nil)) // held (fresh)
	e.publish(t, "customers.customer.created", customerPayload("evt-held-2", "cus-2", nil))
	old := time.Now().Add(-3 * time.Hour)
	e.db.Exec("UPDATE webhook_deliveries SET created_at = ? WHERE event_id = 'evt-held' AND webhook_id = ?", old, held.ID)

	live := e.create(t, e.sub(&Declarative{PayloadFields: []string{"id", "name"}, Enabled: true}))
	e.publish(t, "customers.customer.updated", customerPayload("evt-dead", "cus-3", nil))
	e.drain(2) // 422 -> dead
	e.db.Exec("UPDATE webhook_deliveries SET created_at = ? WHERE event_id = 'evt-dead' AND webhook_id = ?", old, live.ID)

	n, err := e.svc.PurgeExpired(context.Background())
	if err != nil || n != 2 {
		t.Fatalf("purged %d err %v", n, err)
	}
	byEvent := func(w *Webhook, id string) WebhookDelivery {
		var d WebhookDelivery
		e.db.First(&d, "webhook_id = ? AND event_id = ?", w.ID, id)
		return d
	}
	if d := byEvent(held, "evt-held"); !d.PayloadPurged || d.Held || len(d.Payload) != 0 {
		t.Fatalf("expired held: %+v", d)
	}
	if d := byEvent(held, "evt-held-2"); d.PayloadPurged || !d.Held {
		t.Fatalf("fresh held must stay: %+v", d)
	}
	if d := byEvent(live, "evt-dead"); !d.PayloadPurged {
		t.Fatalf("expired dead: %+v", d)
	}
	// the idempotency key never carries payload data (only event.* paths are allowed)
	err = e.svc.Create(context.Background(), e.sub(&Declarative{PayloadFields: []string{"id"}, Idempotency: "{{payload.email}}"}))
	if !errors.Is(err, ErrInvalidDeclarative) {
		t.Fatalf("PII in the idempotency key: %v", err)
	}
}

// --- M7: actualizaciones parciales y atomicas ------------------------------

func TestUpdateDoesNotOverwriteCountersOrOwner(t *testing.T) {
	e := newEnv(t, nil)
	w := e.create(t, e.sub(&Declarative{PayloadFields: []string{"id", "name"}, Enabled: true}))
	stale := *w // a copy loaded before the counters moved
	e.db.Exec("UPDATE webhooks SET success_count = 7, failure_count = 3, consecutive_failures = 3, last_error = 'boom' WHERE id = ?", w.ID)
	stale.Name = "renamed"
	stale.OwnerID = e.other // owner is immutable
	if err := e.svc.Update(context.Background(), &stale); err != nil {
		t.Fatal(err)
	}
	got := reload(t, e, w)
	if got.Name != "renamed" || got.OwnerID != e.org || got.SuccessCount != 7 || got.FailureCount != 3 || got.ConsecutiveFailures != 3 || got.LastError != "boom" {
		t.Fatalf("%+v", got)
	}
	// a declarative subscription cannot be downgraded
	stale.Declarative = nil
	if err := e.svc.Update(context.Background(), &stale); !errors.Is(err, ErrInvalidDeclarative) {
		t.Fatalf("downgrade: %v", err)
	}
	if err := e.svc.Update(context.Background(), &Webhook{URL: "https://x.example.com", Events: StringSlice{"a"}}); !errors.Is(err, ErrWebhookNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
}

func TestFailureHealthDoesNotRewriteTheConfiguration(t *testing.T) {
	e := newEnv(t, nil)
	w := e.create(t, e.sub(&Declarative{PayloadFields: []string{"id", "name"}, Enabled: true}))
	stale := *w
	// an operator edits the configuration while a delivery is failing
	edit := reload(t, e, w)
	edit.Declarative.Headers = map[string]string{"X-Contract": "9"}
	if err := e.svc.Update(context.Background(), edit); err != nil {
		t.Fatal(err)
	}
	d := &WebhookDelivery{ErrorMessage: "x", Event: "customers.customer.created"}
	e.svc.recordFailureHealth(context.Background(), &stale, d, 500, failOpts{})
	got := reload(t, e, w)
	if got.Declarative.Headers["X-Contract"] != "9" || got.ConsecutiveFailures != 1 {
		t.Fatalf("a health update reverted a concurrent edit: %+v", got.Declarative)
	}
}

func TestConsecutiveFailuresAreCountedAtomically(t *testing.T) {
	e := newEnv(t, nil)
	w := e.create(t, e.sub(&Declarative{PayloadFields: []string{"id", "name"}, Enabled: true, AutoDisableThreshold: -1}))
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stale := *w
			e.svc.recordFailureHealth(context.Background(), &stale, &WebhookDelivery{Event: "x"}, 500, failOpts{})
		}()
	}
	wg.Wait()
	if got := reload(t, e, w).ConsecutiveFailures; got != 20 {
		t.Fatalf("lost updates: %d", got)
	}
}

func TestAutoDisableAlertFiresOnce(t *testing.T) {
	e := newEnv(t, nil)
	var mu sync.Mutex
	calls := 0
	e.svc.onAutoOff = func(context.Context, Webhook) { mu.Lock(); calls++; mu.Unlock() }
	w := e.create(t, e.sub(&Declarative{PayloadFields: []string{"id", "name"}, Enabled: true, AutoDisableThreshold: 2}))
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stale := *w
			e.svc.recordFailureHealth(context.Background(), &stale, &WebhookDelivery{Event: "x"}, 500, failOpts{})
		}()
	}
	wg.Wait()
	if calls != 1 {
		t.Fatalf("alert fired %d times", calls)
	}
}

// --- M8: filas sin renderizar ---------------------------------------------

func TestRenderFailedRowsCannotBeReplayedOrRequeued(t *testing.T) {
	e := newEnv(t, nil)
	d := &Declarative{Enabled: true, Transform: &Transform{
		Body:   map[string]any{"c": "{{payload.channel | map:ch}}"},
		Tables: map[string]map[string]string{"ch": {"pos": "retail"}},
	}}
	w := e.create(t, e.sub(d))
	e.publish(t, "customers.customer.created", customerPayload("evt-f", "cus-1", nil))
	r := e.deliveries(t, w)[0]
	if !r.PayloadPurged || r.NextAttemptAt != nil {
		t.Fatalf("%+v", r)
	}
	if _, err := e.svc.Replay(context.Background(), r.ID); !errors.Is(err, ErrPayloadPurged) {
		t.Fatalf("replay: %v", err)
	}
	if err := e.svc.Requeue(context.Background(), r.ID); !errors.Is(err, ErrPayloadPurged) {
		t.Fatalf("requeue: %v", err)
	}
	if len(e.tg.Hits()) != 0 {
		t.Fatal("an empty body with a foreign key was sent")
	}
}

func TestReplayOfADisabledSubscriptionIsRetained(t *testing.T) {
	e := newEnv(t, nil, 422)
	w := e.create(t, e.sub(&Declarative{PayloadFields: []string{"id", "name"}, Enabled: true}))
	e.publish(t, "customers.customer.created", customerPayload("evt-r", "cus-1", nil))
	e.drain(1)
	dead := e.deliveries(t, w)[0]
	if _, err := e.svc.SetEnabled(context.Background(), w.ID, false); err != nil {
		t.Fatal(err)
	}
	hits := len(e.tg.Hits())
	rp, err := e.svc.Replay(context.Background(), dead.ID)
	if err != nil || !rp.Held || rp.IdempotencyKey != "evt-r" || len(e.tg.Hits()) != hits {
		t.Fatalf("replay on a disabled subscription must be retained: %+v %v", rp, err)
	}
}

// el guardado de EventID unico no debe romper Replay (se creaba otra fila con el mismo id)
func TestReplayDoesNotCollideWithTheUniqueEventIndex(t *testing.T) {
	e := newEnv(t, nil, 422)
	w := e.create(t, e.sub(&Declarative{PayloadFields: []string{"id", "name"}, Enabled: true}))
	e.publish(t, "customers.customer.created", customerPayload("evt-r2", "cus-1", nil))
	e.drain(1)
	dead := e.deliveries(t, w)[0]
	for i := 0; i < 2; i++ {
		if _, err := e.svc.Replay(context.Background(), dead.ID); err != nil {
			t.Fatalf("replay %d: %v", i, err)
		}
	}
}
