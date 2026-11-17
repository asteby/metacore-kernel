package inbound

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"

	"github.com/google/uuid"
	"testing"
)

const crmSchema = `{"type":"object","required":["id","nombre","correo"],"additionalProperties":false,
 "properties":{"id":{"type":"string"},"nombre":{"type":"string"},"correo":{"type":"string"},
 "vip":{"type":"boolean"},"tags":{"type":"array","items":{"type":"string"}}}}`

func crmContract() Contract {
	return Contract{
		Key: "crm-cliente-nuevo", Version: 1, JSONSchema: json.RawMessage(crmSchema),
		Idempotency: "{id}", Auth: ProfileHMACCanonicalV1,
		Actions: []Action{
			{Do: ActionRecordUpsert, Model: "customers", Match: []string{"external_ref"},
				Mapping: map[string]string{"external_ref": "{{payload.id}}", "name": "{{payload.nombre}}", "email": "{{payload.correo}}"}},
			{Do: ActionEventEmit, Name: "customers.customer.imported", Payload: map[string]any{"id": "{{result.id}}", "label": "cliente {{payload.nombre}}"}},
			{Do: ActionNotify, Template: "cliente_importado", When: &Condition{Path: "result.created", Eq: json.RawMessage(`true`)}},
		},
	}
}

// the notify step follows event.emit, whose result has no "created"; to test
// the condition on the record step we use a dedicated contract below.

type fakeEmitter struct {
	mu     sync.Mutex
	reqs   []EmitRequest
	fail   error
	onEmit func(EmitRequest)
}

func (f *fakeEmitter) Emit(_ context.Context, r EmitRequest) error {
	f.mu.Lock()
	if f.fail != nil {
		e := f.fail
		f.fail = nil
		f.mu.Unlock()
		return e
	}
	f.reqs = append(f.reqs, r)
	cb := f.onEmit
	f.mu.Unlock()
	if cb != nil {
		cb(r)
	}
	return nil
}

type fakeNotifier struct {
	mu   sync.Mutex
	reqs []NotifyRequest
}

func (f *fakeNotifier) Notify(_ context.Context, r NotifyRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, r)
	return nil
}

type fakeAuto struct {
	mu  sync.Mutex
	evs []AutomationEvent
}

func (f *fakeAuto) IngestEvent(_ context.Context, ev AutomationEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.evs = append(f.evs, ev)
	return nil
}

type chainEnv struct {
	*env
	em   *fakeEmitter
	nt   *fakeNotifier
	auto *fakeAuto
}

func newChainEnv(t *testing.T, cs ...Contract) *chainEnv {
	t.Helper()
	reg := NewRegistry()
	for _, c := range cs {
		if err := reg.Register(c); err != nil {
			t.Fatal(err)
		}
	}
	ce := &chainEnv{em: &fakeEmitter{}, nt: &fakeNotifier{}, auto: &fakeAuto{}}
	ce.env = newEnv(t, func(cfg *Config) {
		cfg.Contracts, cfg.Emitter, cfg.Notifier, cfg.Automation = reg, ce.em, ce.nt, ce.auto
	})
	return ce
}

func crmBody(id string) string {
	return `{"id":"` + id + `","nombre":"Ana","correo":"ana@example.com"}`
}

func steps(t *testing.T, out map[string]any) []string {
	t.Helper()
	raw, _ := out["actions"].([]any)
	var s []string
	for _, x := range raw {
		m := x.(map[string]any)
		s = append(s, m["status"].(string))
	}
	return s
}

// A: chain of 3 actions sees payload / result of the previous; duplicates do
// not repeat any of them.
func TestChainOfThreeActionsAndReplay(t *testing.T) {
	c := crmContract()
	ce := newChainEnv(t, c)
	code, out := ce.send(t, ce.orgA, c.Key, crmBody("c-1"), nil)
	if code != 200 || out["status"] != "applied" {
		t.Fatalf("%d %v", code, out)
	}
	if got := strings.Join(steps(t, out), ","); got != "done,done,done" {
		t.Fatalf("steps %s", got)
	}
	rows := ce.exec.rows[ce.orgA]
	if len(rows) != 1 || rows[0]["email"] != "ana@example.com" || rows[0]["external_ref"] != "c-1" {
		t.Fatalf("row %v", rows)
	}
	if len(ce.em.reqs) != 1 {
		t.Fatalf("emits %d", len(ce.em.reqs))
	}
	em := ce.em.reqs[0]
	if em.Payload["id"] != rows[0]["id"] || em.Payload["label"] != "cliente Ana" || em.Name != "customers.customer.imported" || em.Org != ce.orgA {
		t.Fatalf("emit %+v", em)
	}
	if em.Origin != "inbound:crm-cliente-nuevo" || ce.exec.last.Origin != em.Origin {
		t.Fatalf("origin %q / %q", em.Origin, ce.exec.last.Origin)
	}
	if len(ce.nt.reqs) != 1 || ce.nt.reqs[0].Origin != em.Origin {
		t.Fatalf("notify %+v", ce.nt.reqs)
	}

	execCalls := ce.exec.calls
	code, out = ce.send(t, ce.orgA, c.Key, crmBody("c-1"), nil)
	if code != 200 || out["status"] != "replayed" || ce.exec.calls != execCalls || len(ce.em.reqs) != 1 || len(ce.nt.reqs) != 1 {
		t.Fatalf("replay repeated actions: %d %v", code, out)
	}
	// same key, other body -> 409 (fingerprint respected by chains)
	code, out = ce.send(t, ce.orgA, c.Key, `{"id":"c-1","nombre":"Otro","correo":"x@example.com"}`, nil)
	if code != 409 || errCode(out) != CodeIdempotencyClash {
		t.Fatalf("%d %v", code, out)
	}
}

// B: failure on the 2nd action leaves a failed receipt with the action index;
// the retry resumes without repeating the first.
func TestChainFailureMidwayThenRetryResumes(t *testing.T) {
	c := crmContract()
	c.Actions[2].When = nil
	ce := newChainEnv(t, c)
	ce.em.fail = errors.New("bus down: ana@example.com")

	code, out := ce.send(t, ce.orgA, c.Key, crmBody("c-2"), nil)
	if code != 500 || errCode(out) != CodeInternal {
		t.Fatalf("%d %v", code, out)
	}
	if fa := out["error"].(map[string]any)["failed_action"]; fa != float64(1) {
		t.Fatalf("failed_action %v", fa)
	}
	if strings.Contains(toJSON(out), "ana@example.com") {
		t.Fatal("error text leaked")
	}
	rcs, _ := ce.h.Receipts(context.Background(), ce.orgA, ReceiptFilter{Contract: c.Key})
	if len(rcs) != 1 || rcs[0].Status != StatusFailed {
		t.Fatalf("%+v", rcs)
	}
	if i, ok := rcs[0].FailedAction(); !ok || i != 1 || rcs[0].Progress[0].Status != StepDone || rcs[0].Progress[1].ErrorCode != "internal" {
		t.Fatalf("progress %+v", rcs[0].Progress)
	}
	if strings.Contains(toJSON(rcs[0]), "ana@example.com") {
		t.Fatal("receipt kept payload data")
	}
	if ce.exec.calls != 1 || len(ce.nt.reqs) != 0 {
		t.Fatalf("calls %d notif %d", ce.exec.calls, len(ce.nt.reqs))
	}

	// Another body under the same key after a partial application: 409.
	code, out = ce.send(t, ce.orgA, c.Key, `{"id":"c-2","nombre":"Otra","correo":"o@example.com"}`, nil)
	if code != 409 || errCode(out) != CodeIdempotencyClash || ce.exec.calls != 1 {
		t.Fatalf("%d %v", code, out)
	}

	// Retry with the same body resumes at action 1.
	code, out = ce.send(t, ce.orgA, c.Key, crmBody("c-2"), nil)
	if code != 200 || out["status"] != "applied" {
		t.Fatalf("%d %v", code, out)
	}
	if ce.exec.calls != 1 || len(ce.em.reqs) != 1 || len(ce.nt.reqs) != 1 || len(ce.exec.rows[ce.orgA]) != 1 {
		t.Fatalf("resume repeated work: exec %d emit %d notify %d", ce.exec.calls, len(ce.em.reqs), len(ce.nt.reqs))
	}
	// the emit received the id of the record created BEFORE the failure
	if ce.em.reqs[0].Payload["id"] != ce.exec.rows[ce.orgA][0]["id"] {
		t.Fatalf("result not carried across attempts: %v", ce.em.reqs[0].Payload)
	}
	rcs, _ = ce.h.Receipts(context.Background(), ce.orgA, ReceiptFilter{Contract: c.Key})
	if rcs[0].Status != StatusApplied || rcs[0].Attempts != 2 {
		t.Fatalf("%+v", rcs[0])
	}
}

func toJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

// C: conditions: eq on the previous result, exists on the payload.
func TestChainConditions(t *testing.T) {
	c := crmContract()
	c.Key, c.Idempotency = "crm-cond", "{id}:{nombre}"
	c.Actions = []Action{
		{Do: ActionRecordUpsert, Model: "customers", Match: []string{"external_ref"},
			Mapping: map[string]string{"external_ref": "{{payload.id}}", "name": "{{payload.nombre}}"}},
		{Do: ActionNotify, Template: "nuevo", When: &Condition{Path: "result.created", Eq: json.RawMessage(`true`)}},
		{Do: ActionEventEmit, Name: "customers.vip", When: &Condition{Path: "payload.vip", Exists: ptr(true)},
			Payload: map[string]any{"id": "{{result.id}}"}},
		{Do: ActionAutomationEnqueue, Name: "customers.seen", When: &Condition{Path: "payload.tags.0", Eq: json.RawMessage(`"a"`)}},
	}
	ce := newChainEnv(t, c)
	_, out := ce.send(t, ce.orgA, c.Key, `{"id":"k1","nombre":"A","correo":"a@x.mx","vip":true,"tags":["a"]}`, nil)
	if got := strings.Join(steps(t, out), ","); got != "done,done,done,done" {
		t.Fatalf("first %s %v", got, out)
	}
	// second event for the same customer: not created, no vip, other tag
	_, out = ce.send(t, ce.orgA, c.Key, `{"id":"k1","nombre":"B","correo":"a@x.mx","tags":["b"]}`, nil)
	if got := strings.Join(steps(t, out), ","); got != "done,skipped,skipped,skipped" {
		t.Fatalf("second %s %v", got, out)
	}
	if len(ce.nt.reqs) != 1 || len(ce.em.reqs) != 1 || len(ce.auto.evs) != 1 {
		t.Fatalf("notify %d emit %d auto %d", len(ce.nt.reqs), len(ce.em.reqs), len(ce.auto.evs))
	}
	if ce.auto.evs[0].Origin != "inbound:crm-cond" || ce.auto.evs[0].ID == "" {
		t.Fatalf("%+v", ce.auto.evs[0])
	}
}

func ptr[T any](v T) *T { return &v }

// D: on_error continue keeps going and ends applied with the error recorded.
func TestChainOnErrorContinue(t *testing.T) {
	c := crmContract()
	c.Actions[2].When = nil
	c.Actions[1].OnError = OnErrorContinue
	ce := newChainEnv(t, c)
	ce.em.fail = errors.New("boom")
	code, out := ce.send(t, ce.orgA, c.Key, crmBody("c-3"), nil)
	if code != 200 || strings.Join(steps(t, out), ",") != "done,error,done" || len(ce.nt.reqs) != 1 {
		t.Fatalf("%d %v", code, out)
	}
	// contract level default
	c2 := crmContract()
	c2.Key, c2.OnError = "crm-cont", OnErrorContinue
	c2.Actions[2].When = nil
	ce = newChainEnv(t, c2)
	ce.em.fail = errors.New("boom")
	code, out = ce.send(t, ce.orgA, c2.Key, crmBody("c-4"), nil)
	if code != 200 || strings.Join(steps(t, out), ",") != "done,error,done" {
		t.Fatalf("%d %v", code, out)
	}
}

func marked(id string, origin any, hop any) string {
	b, _ := json.Marshal(map[string]any{"id": id, "nombre": "Ana", "correo": "ana@example.com", "_origin": origin, "_hop": hop})
	return string(b)
}

// E: the emitted event carries origin and hop INSIDE its (signed) body; when
// it comes back to the same contract it is ignored. A header alone is not trusted.
func TestChainLoopIsCut(t *testing.T) {
	c := crmContract()
	c.Actions[2].When = nil
	ce := newChainEnv(t, c)
	var inner map[string]any
	var innerCode int
	ce.em.onEmit = func(r EmitRequest) {
		if r.Payload[OriginField] != "inbound:crm-cliente-nuevo" || r.Payload[HopField] != int64(1) || r.Hop != 1 {
			t.Errorf("emitted body lacks origin/hop: %v", r.Payload)
		}
		// a subscription delivers it back, signed as a whole
		innerCode, inner = ce.send(t, ce.orgA, c.Key, marked("c-loop-2", r.Payload[OriginField], r.Payload[HopField]), nil)
	}
	code, out := ce.send(t, ce.orgA, c.Key, crmBody("c-loop"), nil)
	if code != 200 || out["status"] != "applied" {
		t.Fatalf("%d %v", code, out)
	}
	if innerCode != 200 || inner["status"] != "ignored" {
		t.Fatalf("loop not cut: %d %v", innerCode, inner)
	}
	if len(ce.exec.rows[ce.orgA]) != 1 || len(ce.em.reqs) != 1 {
		t.Fatalf("rows %d emits %d", len(ce.exec.rows[ce.orgA]), len(ce.em.reqs))
	}
	// control: an unsigned/forged header does not cut anything
	ce.em.onEmit = nil
	code, out = ce.send(t, ce.orgA, c.Key, crmBody("c-loop-3"), func(req *http.Request) {
		req.Header.Set("X-Metacore-Origin", "inbound:crm-cliente-nuevo")
	})
	if code != 200 || out["status"] != "applied" {
		t.Fatalf("header must not be trusted: %d %v", code, out)
	}
	// ignore_origins: another contract's origin, from the body
	c.Key, c.IgnoreOrigins = "crm-ign", []string{"inbound:*"}
	ce = newChainEnv(t, c)
	code, out = ce.send(t, ce.orgA, c.Key, marked("z", "inbound:otro", 1), nil)
	if code != 200 || out["status"] != "ignored" || ce.exec.calls != 0 {
		t.Fatalf("%d %v", code, out)
	}
	// malformed reserved fields are rejected, not silently ignored
	code, out = ce.send(t, ce.orgA, c.Key, marked("z2", 5, "x"), nil)
	if code != 422 || errCode(out) != CodeInvalidOrigin {
		t.Fatalf("%d %v", code, out)
	}
}

// M6: a round trip between two contracts (A -> B -> A ...) is bounded by the hop counter.
func TestChainHopLimitBoundsRoundTrips(t *testing.T) {
	mk := func(key, target string) Contract {
		c := crmContract()
		c.Key = key
		c.Actions = []Action{{Do: ActionEventEmit, Name: target, Payload: map[string]any{"x": "{{payload.id}}"}}}
		return c
	}
	a, b := mk("ping", "pong.event"), mk("pong", "ping.event")
	ce := newChainEnv(t, a, b)
	n := 0
	ce.em.onEmit = func(r EmitRequest) {
		n++
		target := "pong"
		if r.Name == "ping.event" {
			target = "ping"
		}
		ce.send(t, ce.orgA, target, marked("h"+strings.Repeat("x", n), r.Payload[OriginField], r.Payload[HopField]), nil)
	}
	code, out := ce.send(t, ce.orgA, "ping", crmBody("start"), nil)
	if code != 200 || out["status"] != "applied" {
		t.Fatalf("%d %v", code, out)
	}
	// hop 0 (external) -> emit hop1 -> emit hop2 -> emit hop3 -> ignored (MaxHops 3)
	if len(ce.em.reqs) != 3 {
		t.Fatalf("emits = %d, want 3", len(ce.em.reqs))
	}
	// a forged huge hop is ignored too
	code, out = ce.send(t, ce.orgA, "ping", marked("big", "inbound:zz", 50), nil)
	if code != 200 || out["status"] != "ignored" {
		t.Fatalf("%d %v", code, out)
	}
}

func TestOriginExcluded(t *testing.T) {
	if !OriginExcluded("inbound:a", []string{"inbound:*"}) || !OriginExcluded("inbound:a", []string{"inbound:a"}) {
		t.Fatal("should match")
	}
	if OriginExcluded("inbound:a", []string{"inbound:b", "x*"}) || OriginExcluded("", []string{"*"}) {
		t.Fatal("should not match")
	}
}

// F: tenants never see each other's chain state.
func TestChainOrganizationIsolation(t *testing.T) {
	c := crmContract()
	c.Actions[2].When = nil
	ce := newChainEnv(t, c)
	for _, org := range []string{"A", "B"} {
		o := ce.orgA
		if org == "B" {
			o = ce.orgB
		}
		code, out := ce.send(t, o, c.Key, crmBody("same-id"), nil)
		if code != 200 || out["status"] != "applied" {
			t.Fatalf("%s: %d %v", org, code, out)
		}
	}
	if len(ce.exec.rows[ce.orgA]) != 1 || len(ce.exec.rows[ce.orgB]) != 1 {
		t.Fatal("rows not partitioned")
	}
	seen := map[string]bool{}
	for _, r := range ce.em.reqs {
		seen[r.Org.String()] = true
	}
	if len(ce.em.reqs) != 2 || !seen[ce.orgA.String()] || !seen[ce.orgB.String()] {
		t.Fatalf("emit orgs %+v", ce.em.reqs)
	}
	for _, o := range []string{ce.orgA.String(), ce.orgB.String()} {
		_ = o
	}
	ra, _ := ce.h.Receipts(context.Background(), ce.orgA, ReceiptFilter{})
	rb, _ := ce.h.Receipts(context.Background(), ce.orgB, ReceiptFilter{})
	if len(ra) != 1 || len(rb) != 1 || ra[0].ID == rb[0].ID {
		t.Fatal("receipts mixed")
	}
	// a failed chain in A does not block B's retry state
	ce.em.fail = errors.New("x")
	code, _ := ce.send(t, ce.orgA, c.Key, crmBody("only-a"), nil)
	if code != 500 {
		t.Fatal(code)
	}
	code, out := ce.send(t, ce.orgB, c.Key, crmBody("only-a"), nil)
	if code != 200 || out["status"] != "applied" {
		t.Fatalf("%d %v", code, out)
	}
}

func TestCommandRunAction(t *testing.T) {
	c := crmContract()
	c.Key = "crm-cmd"
	c.Actions = []Action{{Do: ActionCommandRun, Command: "cust.sync", Mapping: map[string]string{"ref": "{{payload.id}}", "n": "x-{{payload.nombre}}"}}}
	cmds := NewCommandRegistry()
	var got Operation
	cmds.Register("cust.sync", func(_ context.Context, op Operation) (Outcome, error) { got = op; return Outcome{RecordID: "r9"}, nil })
	reg := NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, func(cfg *Config) { cfg.Contracts, cfg.Commands = reg, cmds })
	code, out := e.send(t, e.orgA, c.Key, crmBody("q"), nil)
	if code != 200 || out["record_id"] != "r9" || got.Values["n"] != "x-Ana" || got.Values["ref"] != "q" || got.Route.Command != "cust.sync" {
		t.Fatalf("%d %v %+v", code, out, got)
	}
}

// G: templates and structure are validated when the contract is registered.
func TestChainRegistrationValidation(t *testing.T) {
	mut := func(f func(c *Contract)) error {
		c := crmContract()
		f(&c)
		return NewRegistry().Register(c)
	}
	bad := map[string]func(c *Contract){
		"payload path missing":   func(c *Contract) { c.Actions[0].Mapping["name"] = "{{payload.nope}}" },
		"nested path missing":    func(c *Contract) { c.Actions[0].Mapping["name"] = "{{payload.nombre.x}}" },
		"result in first action": func(c *Contract) { c.Actions[0].Mapping["name"] = "{{result.id}}" },
		"result field unknown":   func(c *Contract) { c.Actions[1].Payload["id"] = "{{result.foo}}" },
		"event field unknown":    func(c *Contract) { c.Actions[1].Payload["id"] = "{{event.bogus}}" },
		"bad namespace":          func(c *Contract) { c.Actions[1].Payload["id"] = "{{env.HOME}}" },
		"unbalanced":             func(c *Contract) { c.Actions[1].Payload["id"] = "{{payload.id" },
		"unknown action":         func(c *Contract) { c.Actions[1].Do = "http.post" },
		"foreign field":          func(c *Contract) { c.Actions[2].Model = "customers" },
		"match not in mapping":   func(c *Contract) { c.Actions[0].Match = []string{"zzz"} },
		"forbidden target":       func(c *Contract) { c.Actions[0].Mapping["organization_id"] = "{{payload.id}}" },
		"bad on_error":           func(c *Contract) { c.OnError = "retry" },
		"bad action on_error":    func(c *Contract) { c.Actions[0].OnError = "x" },
		"route and actions":      func(c *Contract) { c.Route = Route{Model: "m", Op: OpCreate, Mapping: map[string]string{"a": "id"}} },
		"when path missing":      func(c *Contract) { c.Actions[2].When = &Condition{Path: "payload.zzz", Exists: ptr(true)} },
		"when both": func(c *Contract) {
			c.Actions[2].When = &Condition{Path: "payload.id", Exists: ptr(true), Eq: json.RawMessage(`1`)}
		},
		"when neither":   func(c *Contract) { c.Actions[2].When = &Condition{Path: "payload.id"} },
		"when eq object": func(c *Contract) { c.Actions[2].When = &Condition{Path: "payload.id", Eq: json.RawMessage(`{"a":1}`)} },
		"emit name":      func(c *Contract) { c.Actions[1].Name = "Bad Name" },
		"too many": func(c *Contract) {
			for len(c.Actions) < 12 {
				c.Actions = append(c.Actions, c.Actions[2])
			}
		},
	}
	for name, f := range bad {
		if err := mut(f); !errors.Is(err, ErrInvalidContract) {
			t.Errorf("%s: want ErrInvalidContract, got %v", name, err)
		}
	}
	if err := mut(func(c *Contract) {}); err != nil {
		t.Fatalf("valid contract rejected: %v", err)
	}
	// array index paths and event/result refs are accepted
	if err := mut(func(c *Contract) {
		c.Actions[1].Payload["t"] = "{{payload.tags.0}}-{{event.key}}-{{event.version}}"
	}); err != nil {
		t.Fatal(err)
	}
}

func TestNewChecksChainWiring(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(crmContract()); err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, nil)
	_, err := New(Config{Contracts: reg, Verifiers: e.h.cfg.Verifiers, Receipts: NewMemoryReceiptStore(), Logger: quietLogger()})
	if err == nil || !strings.Contains(err.Error(), "no Executor") || !strings.Contains(err.Error(), "no Emitter") || !strings.Contains(err.Error(), "no Notifier") {
		t.Fatalf("%v", err)
	}
}

// Existing route contracts keep working next to chain contracts.
func TestRouteContractUnchangedWithChainsRegistered(t *testing.T) {
	ce := newChainEnv(t, stockContract(), crmContract())
	code, out := ce.send(t, ce.orgA, "stock.changed", stockBody("ev-x", "S1", 2), nil)
	if code != 200 || out["status"] != "applied" || out["actions"] != nil {
		t.Fatalf("%d %v", code, out)
	}
}

// M5: defaults apply on create only; an upsert/update must not revert them.
func TestDefaultsOnlyOnCreate(t *testing.T) {
	// route contract
	e := newEnv(t, nil)
	e.send(t, e.orgA, "stock.changed", stockBody("d1", "SKU-D", 1), nil)
	e.exec.rows[e.orgA][0]["source"] = "manual"
	e.send(t, e.orgA, "stock.changed", stockBody("d2", "SKU-D", 2), nil)
	if got := e.exec.rows[e.orgA][0]["source"]; got != "manual" {
		t.Fatalf("route: default reverted a field on update: %v", got)
	}
	// chain action
	c := crmContract()
	c.Key, c.Idempotency = "crm-def", "{id}:{nombre}"
	c.Actions = []Action{{Do: ActionRecordUpsert, Model: "customers", Match: []string{"external_ref"},
		Mapping:  map[string]string{"external_ref": "{{payload.id}}", "name": "{{payload.nombre}}"},
		Defaults: map[string]any{"status": "new"}}}
	ce := newChainEnv(t, c)
	ce.send(t, ce.orgA, c.Key, `{"id":"k","nombre":"A","correo":"a@x.mx"}`, nil)
	if ce.exec.rows[ce.orgA][0]["status"] != "new" {
		t.Fatal("default not applied on create")
	}
	ce.exec.rows[ce.orgA][0]["status"] = "won"
	ce.send(t, ce.orgA, c.Key, `{"id":"k","nombre":"B","correo":"a@x.mx"}`, nil)
	if got := ce.exec.rows[ce.orgA][0]["status"]; got != "won" || ce.exec.rows[ce.orgA][0]["name"] != "B" {
		t.Fatalf("chain: default reverted state: %v", ce.exec.rows[ce.orgA][0])
	}
}

// flakyStore loses the progress write number failOn (1-based); the chain does
// a heartbeat write before each action and one after, so failOn=2 is "the
// effect of action 0 committed, its progress write failed".
type flakyStore struct {
	*MemoryReceiptStore
	mu     sync.Mutex
	calls  int
	failOn int
	// hook runs instead of the write on call number hookOn (1-based).
	hookOn int
	hook   func()
}

func (f *flakyStore) SaveProgress(ctx context.Context, id uuid.UUID, attempt int, s []ActionStep) (bool, error) {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()
	if n == f.failOn {
		return false, errors.New("db gone")
	}
	if n == f.hookOn && f.hook != nil {
		f.hook()
	}
	return f.MemoryReceiptStore.SaveProgress(ctx, id, attempt, s)
}

// M3: record.create committed but the progress write failed: the retry must
// not create a second row.
func TestRecordCreateNotDuplicatedAfterAmbiguousFailure(t *testing.T) {
	c := crmContract()
	c.Key = "crm-create"
	c.Actions = []Action{
		{Do: ActionRecordCreate, Model: "customers", IdempotencyField: "inbound_key",
			Mapping: map[string]string{"external_ref": "{{payload.id}}", "name": "{{payload.nombre}}"}},
		{Do: ActionEventEmit, Name: "customers.created", Payload: map[string]any{"id": "{{result.id}}"}},
	}
	reg := NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatal(err)
	}
	st := &flakyStore{MemoryReceiptStore: NewMemoryReceiptStore(), failOn: 2}
	em := &fakeEmitter{}
	e := newEnv(t, func(cfg *Config) { cfg.Contracts, cfg.Receipts, cfg.Emitter = reg, st, em })

	code, _ := e.send(t, e.orgA, c.Key, crmBody("m3"), nil)
	if code != 500 || len(e.exec.rows[e.orgA]) != 1 {
		t.Fatalf("first: %d rows %d", code, len(e.exec.rows[e.orgA]))
	}
	code, out := e.send(t, e.orgA, c.Key, crmBody("m3"), nil)
	if code != 200 || out["status"] != "applied" {
		t.Fatalf("retry: %d %v", code, out)
	}
	rows := e.exec.rows[e.orgA]
	if len(rows) != 1 {
		t.Fatalf("duplicated record: %d rows", len(rows))
	}
	key, _ := rows[0]["inbound_key"].(string)
	if !strings.HasSuffix(key, ":0") || len(em.reqs) != 1 || em.reqs[0].Payload["id"] != rows[0]["id"] {
		t.Fatalf("key %q emits %+v", key, em.reqs)
	}
	if e.exec.last.IdempotencyKey != key {
		t.Fatalf("operation key %q vs %q", e.exec.last.IdempotencyKey, key)
	}
}

func TestRecordCreateRequiresIdempotencyField(t *testing.T) {
	c := crmContract()
	c.Actions = []Action{{Do: ActionRecordCreate, Model: "m", Mapping: map[string]string{"name": "{{payload.nombre}}"}}}
	if err := NewRegistry().Register(c); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("%v", err)
	}
	c.Actions[0].IdempotencyField = "name"
	if err := NewRegistry().Register(c); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("field must not be mapped: %v", err)
	}
	c.Actions[0].IdempotencyField = "inbound_key"
	if err := NewRegistry().Register(c); err != nil {
		t.Fatal(err)
	}
	c.Actions[0] = Action{Do: ActionRecordUpsert, Model: "m", Match: []string{"name"}, IdempotencyField: "k", Mapping: map[string]string{"name": "{{payload.nombre}}"}}
	if err := NewRegistry().Register(c); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("idempotency_field only for create: %v", err)
	}
	c.Actions = []Action{{Do: ActionEventEmit, Name: "x.y", Payload: map[string]any{"_hop": 1}}}
	if err := NewRegistry().Register(c); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("reserved key: %v", err)
	}
}
