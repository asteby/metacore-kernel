package webhooks

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/inbound"
)

// emitSink es el EventEmitter del lado receptor: guarda lo que la cadena emite.
type emitSink struct {
	mu   sync.Mutex
	reqs []inbound.EmitRequest
}

func (e *emitSink) Emit(_ context.Context, r inbound.EmitRequest) error {
	e.mu.Lock()
	e.reqs = append(e.reqs, r)
	e.mu.Unlock()
	return nil
}

// inboundSide monta un `inbound` real con un contrato que re-emite el evento.
func inboundSide(t *testing.T, ignore ...string) (*inbound.Handler, *emitSink, uuid.UUID) {
	t.Helper()
	reg := inbound.NewRegistry()
	err := reg.Register(inbound.Contract{
		Key: "echo", Version: 1,
		JSONSchema:  json.RawMessage(`{"type":"object","required":["id"],"properties":{"id":{"type":"string"}}}`),
		Idempotency: "{id}", Auth: "open", IgnoreOrigins: ignore,
		Actions: []inbound.Action{{Do: inbound.ActionEventEmit, Name: "customers.customer.echoed",
			Payload: map[string]any{"id": "{{payload.id}}"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	sink := &emitSink{}
	h, err := inbound.New(inbound.Config{
		Contracts: reg, Receipts: inbound.NewMemoryReceiptStore(), Emitter: sink,
		Verifiers: map[string]inbound.Verifier{"open": inbound.VerifierFunc(func(context.Context, inbound.VerifyRequest) error { return nil })},
	})
	if err != nil {
		t.Fatal(err)
	}
	return h, sink, uuid.New()
}

func deliverTo(t *testing.T, h *inbound.Handler, org uuid.UUID, body map[string]any) map[string]any {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/inbound/"+org.String()+"/echo", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if rr.Code != 200 {
		t.Fatalf("inbound answered %d: %s", rr.Code, rr.Body.String())
	}
	return out
}

func lastReceiptCode(t *testing.T, h *inbound.Handler, org uuid.UUID) string {
	t.Helper()
	rs, err := h.Receipts(context.Background(), org, inbound.ReceiptFilter{Contract: "echo", Limit: 1})
	if err != nil || len(rs) == 0 {
		t.Fatalf("receipts: %v %v", rs, err)
	}
	return rs[0].ErrorCode
}

// emitOut publica el evento en el servicio de `e` (con el host `host`) y
// devuelve el cuerpo que quedo en la entrega.
func emitOut(t *testing.T, e *testEnv, host, key string, payload map[string]any) map[string]any {
	t.Helper()
	e.svc.hostID = host
	w := e.create(t, e.sub(&Declarative{Key: key, Transform: &Transform{Body: map[string]any{"id": "{{payload.id}}"}}}))
	e.publish(t, "customers.customer.updated", payload)
	ds := e.deliveries(t, w)
	if len(ds) != 1 {
		t.Fatalf("deliveries %d", len(ds))
	}
	return map[string]any(ds[0].Payload)
}

// OPS -> X -> OPS: cada salto estampa _origin y sube _hop; al volver a OPS el
// contador ya llego al tope y `inbound` lo corta con origin_loop.
func TestOriginLoopOpsXOps(t *testing.T) {
	// 1) OPS emite: sin salto previo => _hop 1.
	ops := newEnv(t, nil)
	b1 := emitOut(t, ops, "ops", "ops-out", map[string]any{"id": "c1"})
	if b1["_origin"] != "ops:sub:ops-out" || b1["_hop"] != float64(1) {
		t.Fatalf("OPS body: %v", b1)
	}

	// 2) X recibe (hop 1 < 3), aplica y re-emite con _hop 2 dentro del cuerpo.
	xIn, xSink, orgX := inboundSide(t)
	if out := deliverTo(t, xIn, orgX, b1); out["status"] != "applied" || len(xSink.reqs) != 1 {
		t.Fatalf("X inbound: %v emits %d", out, len(xSink.reqs))
	}
	emitted := xSink.reqs[0].Payload
	if emitted["_hop"] != int64(2) {
		t.Fatalf("X emit hop: %v", emitted)
	}

	// 3) La suscripcion de X sobre ese evento toma el hop entrante (2) y emite 3.
	x := newEnv(t, nil)
	b2 := emitOut(t, x, "x", "x-out", emitted)
	if b2["_origin"] != "x:sub:x-out" || b2["_hop"] != float64(3) {
		t.Fatalf("X body: %v", b2)
	}

	// 4) De vuelta en OPS: hop 3 >= DefaultMaxHops => ignorado, recibo origin_loop.
	opsIn, opsSink, orgOps := inboundSide(t)
	if out := deliverTo(t, opsIn, orgOps, b2); out["status"] != "ignored" {
		t.Fatalf("expected ignored: %v", out)
	}
	if c := lastReceiptCode(t, opsIn, orgOps); c != inbound.CodeOriginLoop || len(opsSink.reqs) != 0 {
		t.Fatalf("receipt code %q emits %d", c, len(opsSink.reqs))
	}

	// 5) Igual con un salto bajo si el receptor ignora el origen de X.
	b3 := emitOut(t, newEnv(t, nil), "x", "x-out", map[string]any{"id": "c2"})
	ign, _, orgI := inboundSide(t, "x:sub:*")
	if out := deliverTo(t, ign, orgI, b3); out["status"] != "ignored" || lastReceiptCode(t, ign, orgI) != inbound.CodeOriginLoop {
		t.Fatalf("ignore_origins: %v", out)
	}
}

func TestPropagateOriginRules(t *testing.T) {
	// El mapeo manda: no se sobrescribe _origin/_hop.
	e := newEnv(t, nil)
	e.svc.hostID = "ops"
	w := e.create(t, e.sub(&Declarative{Key: "k", Transform: &Transform{Body: map[string]any{
		"id": "{{payload.id}}", "_origin": "manual", "_hop": 7}}}))
	e.publish(t, "customers.customer.updated", map[string]any{"id": "c1"})
	b := map[string]any(e.deliveries(t, w)[0].Payload)
	if b["_origin"] != "manual" || b["_hop"] != float64(7) {
		t.Fatalf("overwrote mapping: %v", b)
	}

	// PropagateOrigin=false: sin marcas. Sin Key: el id de la suscripcion.
	off := false
	e2 := newEnv(t, nil)
	e2.svc.hostID = "ops"
	w2 := e2.create(t, e2.sub(&Declarative{PropagateOrigin: &off, Transform: &Transform{Body: map[string]any{"id": "{{payload.id}}"}}}))
	e2.publish(t, "customers.customer.updated", map[string]any{"id": "c1"})
	b2 := map[string]any(e2.deliveries(t, w2)[0].Payload)
	if _, ok := b2["_origin"]; ok || b2["_hop"] != nil {
		t.Fatalf("propagate_origin=false stamped: %v", b2)
	}
	e3 := newEnv(t, nil)
	e3.svc.hostID = "ops"
	w3 := e3.create(t, e3.sub(&Declarative{Transform: &Transform{Body: map[string]any{"id": "{{payload.id}}"}}}))
	e3.publish(t, "customers.customer.updated", map[string]any{"id": "c1"})
	b3 := map[string]any(e3.deliveries(t, w3)[0].Payload)
	if o, _ := b3["_origin"].(string); o != "ops:sub:"+w3.ID.String() {
		t.Fatalf("default key: %v", b3)
	}

	// payload_fields tambien lleva marcas, en la raiz.
	e4 := newEnv(t, nil)
	e4.svc.hostID = "ops"
	w4 := e4.create(t, e4.sub(&Declarative{Key: "pf", PayloadFields: []string{"id"}}))
	e4.publish(t, "customers.customer.updated", map[string]any{"id": "c1"})
	b4 := map[string]any(e4.deliveries(t, w4)[0].Payload)
	if b4["_origin"] != "ops:sub:pf" || b4["_hop"] != float64(1) {
		t.Fatalf("payload_fields: %v", b4)
	}
}

func TestCorrelationHeader(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload map[string]any
		want    string
	}{
		{"correlation", map[string]any{"id": "c1", "envelope": map[string]any{"id": "evt-1", "correlation_id": "corr-9"}}, "corr-9"},
		{"fallback envelope.id", map[string]any{"id": "c1", "envelope": map[string]any{"id": "evt-2"}}, "evt-2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, nil)
			w := e.create(t, e.sub(&Declarative{Key: "c", PayloadFields: []string{"id"}, Enabled: true}))
			e.publish(t, "customers.customer.updated", tc.payload)
			e.drain(1)
			hits := e.tg.Hits()
			if len(hits) != 1 || hits[0].Header.Get(CorrelationHeader) != tc.want {
				t.Fatalf("hits %+v want %s", hits, tc.want)
			}
			var body struct {
				Event map[string]any `json:"event"`
			}
			_ = json.Unmarshal(hits[0].Body, &body)
			if body.Event["correlation"] != tc.want || !strings.HasPrefix(hits[0].Header.Get(DefaultIdempotencyHeader), "evt-") {
				t.Fatalf("body event %v", body.Event)
			}
			_ = w
		})
	}
}
