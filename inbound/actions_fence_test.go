package inbound

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/dynamic"
)

// M4: a slow worker whose lease was taken over must not apply further effects
// nor overwrite the new owner's receipt.
func TestChainLosesOwnershipAbortsBeforeNextEffect(t *testing.T) {
	c := crmContract()
	c.Key = "crm-fence"
	c.Actions[2].When = nil
	reg := NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatal(err)
	}
	st := &flakyStore{MemoryReceiptStore: NewMemoryReceiptStore(), hookOn: 3} // heartbeat before action 1
	em := &fakeEmitter{}
	nt := &fakeNotifier{}
	e := newEnv(t, func(cfg *Config) { cfg.Contracts, cfg.Receipts, cfg.Emitter, cfg.Notifier = reg, st, em, nt })
	st.hook = func() {
		// another replica re-claims the event (the lease expired for the slow one)
		rows, _ := st.List(context.Background(), e.orgA, ReceiptFilter{Contract: c.Key})
		r := rows[0]
		_ = st.Finish(context.Background(), r.ID, StatusFailed, "", "internal")
		_, claimed, _ := st.Begin(context.Background(), Receipt{OrgID: r.OrgID, Contract: r.Contract, Version: r.Version, KeyHash: r.KeyHash, BodyHash: r.BodyHash}, time.Minute)
		if !claimed {
			t.Error("takeover failed")
		}
	}
	code, out := e.send(t, e.orgA, c.Key, crmBody("fence"), nil)
	if code != 409 || errCode(out) != CodeLeaseLost {
		t.Fatalf("%d %v", code, out)
	}
	if len(em.reqs) != 0 || len(nt.reqs) != 0 {
		t.Fatalf("effects after losing the lease: emit %d notify %d", len(em.reqs), len(nt.reqs))
	}
	rows, _ := st.List(context.Background(), e.orgA, ReceiptFilter{Contract: c.Key})
	if rows[0].Status != StatusProcessing || rows[0].Attempts != 2 {
		t.Fatalf("the new owner receipt was overwritten: %+v", rows[0])
	}
}

func TestMemorySaveProgressIsFenced(t *testing.T) {
	st := NewMemoryReceiptStore()
	org := uuid.New()
	r := Receipt{OrgID: org, Contract: "c", Version: 1, KeyHash: HashKey(org, "c", "k"), BodyHash: "b"}
	first, _, _ := st.Begin(context.Background(), r, time.Minute)
	_ = st.Finish(context.Background(), first.ID, StatusFailed, "", "x")
	second, claimed, _ := st.Begin(context.Background(), r, time.Minute)
	if !claimed || second.Attempts != 2 {
		t.Fatal("reclaim")
	}
	steps := []ActionStep{{Index: 0, Do: "x", Status: StepDone}}
	if ok, _ := st.SaveProgress(context.Background(), first.ID, 1, steps); ok {
		t.Fatal("stale attempt wrote progress")
	}
	if ok, _ := st.SaveProgress(context.Background(), second.ID, 2, steps); !ok {
		t.Fatal("owner refused")
	}
}

const scopedSchema = `{"type":"object","required":["id","sku"],"additionalProperties":false,
 "properties":{"id":{"type":"string"},"sku":{"type":"string"},"branch":{"type":"string"},"entity":{"type":"string"}}}`

func scopedChain() Contract {
	c := crmContract()
	c.Key, c.JSONSchema = "inv-scoped", json.RawMessage(scopedSchema)
	c.Actions = []Action{{Do: ActionRecordUpsert, Model: "inv", Match: []string{"sku"},
		Mapping: map[string]string{"sku": "{{payload.sku}}"},
		Scope:   &ScopeMapping{Branch: "branch", Entity: "entity"}}}
	return c
}

// A3: scope requirements of each record action are enforced before any
// effect (422 scope_required) and before the nonce is consumed.
func TestChainScopeRequiredAndNonceNotBurned(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(scopedChain()); err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, func(cfg *Config) { cfg.Contracts = reg })
	send := func(body string) (int, map[string]any) {
		return e.send(t, e.orgA, "inv-scoped", body, func(r *http.Request) {
			ts := strconv.FormatInt(e.now.Unix(), 10)
			r.Header.Set("X-Metacore-Timestamp", ts)
			r.Header.Set("X-Metacore-Nonce", "same-nonce")
			r.Header.Set("X-Metacore-Signature", "sha256="+SignCanonicalV1(e.secrets[e.orgA], ts, "same-nonce", "POST", r.URL.Path, []byte(body)))
		})
	}
	code, out := send(`{"id":"1","sku":"S","branch":"b1"}`)
	if code != 422 || errCode(out) != CodeScopeRequired || e.exec.calls != 0 {
		t.Fatalf("%d %v", code, out)
	}
	// the very same nonce is still usable: the rejection did not consume it
	code, out = send(`{"id":"1","sku":"S","branch":"b1","entity":"e1"}`)
	if code != 200 || out["status"] != "applied" {
		t.Fatalf("nonce was burned by the rejection: %d %v", code, out)
	}
	if e.exec.last.Entity != "e1" || e.exec.last.Branch != "b1" || !e.exec.last.RequireEntity || !e.exec.last.RequireBranch {
		t.Fatalf("scope not passed to the executor: %+v", e.exec.last)
	}
	// and now it is a replay -> 401
	code, _ = send(`{"id":"2","sku":"S","branch":"b1","entity":"e1"}`)
	if code != 401 {
		t.Fatalf("nonce replay: %d", code)
	}
}

func TestChainEntityUnresolvedIs403(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(scopedChain()); err != nil {
		t.Fatal(err)
	}
	resolve := func(ok bool) func(context.Context, uuid.UUID, string) (dynamic.EntityScope, bool) {
		return func(context.Context, uuid.UUID, string) (dynamic.EntityScope, bool) {
			return dynamic.EntityScope{EntityID: "e1", Restricted: true, BranchIDs: []string{"b1"}}, ok
		}
	}
	body := `{"id":"1","sku":"S","branch":"b1","entity":"e1"}`
	e := newEnv(t, func(cfg *Config) {
		cfg.Contracts, cfg.Executor = reg, &DynamicExecutor{store: newFakeDyn(), EntityScope: resolve(false)}
	})
	code, out := e.send(t, e.orgA, "inv-scoped", body, nil)
	if code != 403 || errCode(out) != "scope_unresolved" || out["error"].(map[string]any)["failed_action"] != float64(0) {
		t.Fatalf("%d %v", code, out)
	}
	// resolvable but the branch is outside the entity -> 403 scope_forbidden
	e = newEnv(t, func(cfg *Config) {
		cfg.Contracts, cfg.Executor = reg, &DynamicExecutor{store: newFakeDyn(), EntityScope: resolve(true)}
	})
	code, out = e.send(t, e.orgA, "inv-scoped", `{"id":"2","sku":"S","branch":"b9","entity":"e1"}`, nil)
	if code != 403 || errCode(out) != "scope_forbidden" {
		t.Fatalf("%d %v", code, out)
	}
	code, out = e.send(t, e.orgA, "inv-scoped", body, nil)
	if code != 200 {
		t.Fatalf("in scope: %d %v", code, out)
	}
}

// Registration validates record actions against the model (match and
// idempotency_field must be real columns), also with an empty table.
func TestChainChecksModelColumnsAtRegistration(t *testing.T) {
	mk := func(idem string) Contract {
		c := crmContract()
		c.Key = "inv-check"
		c.Actions = []Action{{Do: ActionRecordCreate, Model: "inv", IdempotencyField: idem,
			Mapping: map[string]string{"sku": "{{payload.nombre}}"}}}
		return c
	}
	h, err := New(Config{Contracts: NewRegistry(), Receipts: NewMemoryReceiptStore(), Executor: &DynamicExecutor{store: newFakeDyn()},
		Verifiers: map[string]Verifier{ProfileHMACCanonicalV1: VerifierFunc(func(context.Context, VerifyRequest) error { return nil })}})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.RegisterContract(context.Background(), mk("nope_col")); !errors.Is(err, ErrInvalidContract) || !strings.Contains(err.Error(), "nope_col") {
		t.Fatalf("idempotency_field not checked: %v", err)
	}
	c := mk("warehouse")
	c.Actions = append(c.Actions, Action{Do: ActionRecordUpsert, Model: "inv", Match: []string{"bin"},
		Mapping: map[string]string{"bin": "{{payload.nombre}}"}})
	if err := h.RegisterContract(context.Background(), c); !errors.Is(err, ErrInvalidContract) || !strings.Contains(err.Error(), `"bin"`) {
		t.Fatalf("match column not checked: %v", err)
	}
	if len(h.cfg.Contracts.Keys()) != 0 {
		t.Fatal("rejected contract was registered")
	}
	if err := h.RegisterContract(context.Background(), mk("warehouse")); err != nil {
		t.Fatalf("valid chain refused: %v", err)
	}
	if len(h.cfg.Contracts.Keys()) != 1 {
		t.Fatal("valid contract not registered")
	}
}
