package ledger

import (
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/capability"
	"github.com/asteby/metacore-kernel/payments"
	"gorm.io/gorm"
)

// Two points of sale charge the same document at the same instant with
// different keys: exactly one payment exists afterwards.
func TestConcurrentChargesOneWins(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		h := newHarness(t, db)
		var wg sync.WaitGroup
		verdicts := make([]payments.Verdict, 8)
		errs := make([]error, 8)
		for i := range verdicts {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				res, err := h.l.StartCharge(ctx, h.org, cashCharge("pos-"+strconv.Itoa(i), "100.00"))
				verdicts[i], errs[i] = res.Verdict, err
			}(i)
		}
		wg.Wait()
		news := 0
		for i, v := range verdicts {
			if errs[i] != nil {
				t.Fatalf("charge %d: %v", i, errs[i])
			}
			if v == payments.VerdictNew {
				news++
			} else if v != payments.VerdictAlreadyPaid {
				t.Fatalf("charge %d: verdict %s", i, v)
			}
		}
		doc, _ := h.l.Document(ctx, h.org, "sales_order", "SO-1")
		if news != 1 || len(doc.Payments) != 1 || doc.NetPaid != "100.00" {
			t.Fatalf("news=%d payments=%d net=%s", news, len(doc.Payments), doc.NetPaid)
		}
	})
}

func TestConcurrentHoldsOneWins(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		h := newHarness(t, db)
		var wg sync.WaitGroup
		var mu sync.Mutex
		won, held := 0, 0
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, err := h.l.AcquireHold(ctx, h.org, payments.HoldRequest{DocumentType: "sales_order", DocumentID: "SO-1", Holder: "pos:" + strconv.Itoa(i)}, "pos")
				mu.Lock()
				defer mu.Unlock()
				switch ErrorCode(err) {
				case "":
					won++
				case CodeInPayment:
					held++
				default:
					t.Errorf("hold %d: %v", i, err)
				}
			}(i)
		}
		wg.Wait()
		if won != 1 || held != 7 {
			t.Fatalf("won=%d held=%d", won, held)
		}
	})
}

func hostEnvelope(t *testing.T, res capability.QueryResult) (map[string]any, string) {
	t.Helper()
	if len(res.Results) != 1 || res.Results[0].Provider != HostProvider {
		t.Fatalf("results %+v", res.Results)
	}
	r := res.Results[0]
	if r.Err != nil {
		return nil, r.ErrorCode()
	}
	var m map[string]any
	if err := json.Unmarshal(r.Output, &m); err != nil {
		t.Fatal(err)
	}
	return m, ""
}

func TestHostQuerier(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		h := newHarness(t, db)
		h.fake.on("mercadopago", capability.CommercePaymentsTerminalCreate, func(map[string]any) (any, error) {
			return map[string]any{"provider_ref": "ORD20", "status": "at_device"}, nil
		})
		hq := HostQuerier{Inner: h.fake, Ledger: h.l}
		// Non-ledger keys go to the providers.
		if _, err := hq.Query(ctx, capability.Query{Org: h.org, Capability: capability.CommercePaymentsStatus, Provider: "nobody"}); !errors.Is(err, capability.ErrNoProvider) {
			t.Fatalf("passthrough: %v", err)
		}
		// Typed input.
		if _, err := hq.Query(ctx, capability.Query{Org: h.org, Capability: ChargeStart, Caller: "pos", Input: map[string]any{"document_type": "sales_order"}}); !errors.Is(err, capability.ErrInvalidInput) {
			t.Fatalf("input check: %v", err)
		}
		hold, err := hq.Query(ctx, capability.Query{Org: h.org, Capability: HoldAcquire, Caller: "kiosk", Input: map[string]any{
			"document_type": "sales_order", "document_id": "SO-1", "holder": "kiosk:K1"}})
		if err != nil {
			t.Fatal(err)
		}
		hm, code := hostEnvelope(t, hold)
		if code != "" || hm["token"] == "" {
			t.Fatalf("hold: %v %s", hm, code)
		}
		start := map[string]any{"document_type": "sales_order", "document_id": "SO-1", "document_total": "100.00", "amount": "100.00",
			"currency": "MXN", "channel": "terminal", "provider": "mercadopago", "device_id": "PAX", "idempotency_key": "h-1"}
		// Without the kiosk's token another channel is refused with in_payment.
		res, err := hq.Query(ctx, capability.Query{Org: h.org, Capability: ChargeStart, Caller: "pos", Input: start})
		if _, code := hostEnvelope(t, res); err != nil || code != CodeInPayment {
			t.Fatalf("held: %v %s", err, code)
		}
		// A guest cannot force-release someone's hold.
		res, _ = hq.Query(ctx, capability.Query{Org: h.org, Capability: HoldRelease, Caller: "pos", Input: map[string]any{
			"document_type": "sales_order", "document_id": "SO-1", "token": "guess", "force": true}})
		if _, code := hostEnvelope(t, res); code != CodeHoldNotOwner {
			t.Fatalf("forced release by guest: %s", code)
		}
		start["hold_token"] = hm["token"]
		res, err = hq.Query(ctx, capability.Query{Org: h.org, Capability: ChargeStart, Caller: "kiosk", Input: start})
		sm, code := hostEnvelope(t, res)
		if err != nil || code != "" || sm["verdict"] != "new" || sm["payment"].(map[string]any)["status"] != "at_device" {
			t.Fatalf("start: %v %s %v", sm, code, err)
		}
		pid := sm["payment"].(map[string]any)["id"].(string)
		// The provider reports through payments.observe, only about its own.
		observe := map[string]any{"result": statusOf("ORD20", "paid", "100.00"), "source": "webhook"}
		res, _ = hq.Query(ctx, capability.Query{Org: h.org, Capability: ObserveKey, Caller: "storefront_cart", Input: observe})
		if _, code := hostEnvelope(t, res); code != CodeForbidden {
			t.Fatalf("non-provider observe: %s", code)
		}
		res, _ = hq.Query(ctx, capability.Query{Org: h.org, Capability: ObserveKey, Caller: "mercadopago", Provider: "clip", Input: observe})
		if _, code := hostEnvelope(t, res); code != CodeForbidden {
			t.Fatalf("observe for another provider: %s", code)
		}
		res, _ = hq.Query(ctx, capability.Query{Org: h.org, Capability: ObserveKey, Caller: "mercadopago", Input: observe})
		om, code := hostEnvelope(t, res)
		if code != "" || om["decision"] != "apply" {
			t.Fatalf("observe: %v %s", om, code)
		}
		res, _ = hq.Query(ctx, capability.Query{Org: h.org, Capability: DocumentGet, Caller: "pos", Input: map[string]any{"document_type": "sales_order", "document_id": "SO-1"}})
		dm, _ := hostEnvelope(t, res)
		if dm["net_paid"] != "100.00" || dm["hold"] != nil {
			t.Fatalf("document after payment (hold consumed): %v", dm)
		}
		res, _ = hq.Query(ctx, capability.Query{Org: h.org, Capability: RefundCreate, Caller: "returns", Input: map[string]any{
			"payment_id": pid, "amount": "10", "destination": "wallet", "idempotency_key": "r-1", "credit_note_required": true}})
		rm, code := hostEnvelope(t, res)
		if code != "" || rm["refund"].(map[string]any)["status"] != "pending" {
			t.Fatalf("refund: %v %s", rm, code)
		}
	})
}

func TestReconciler(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		h := newHarness(t, db)
		h.fake.on("mercadopago", capability.CommercePaymentsTerminalCreate, func(in map[string]any) (any, error) {
			return map[string]any{"provider_ref": "ORD-" + in["idempotency_key"].(string), "status": "at_device",
				"expires_at": h.now.Add(10 * time.Minute).Format(time.RFC3339)}, nil
		})
		states := map[string]string{"ORD-r1": "paid", "ORD-r2": "at_device"}
		h.fake.on("mercadopago", capability.CommercePaymentsStatus, func(in map[string]any) (any, error) {
			ref := in["provider_ref"].(string)
			return statusOf(ref, states[ref], "100.00"), nil
		})
		cancels := 0
		h.fake.on("mercadopago", capability.CommercePaymentsCancel, func(in map[string]any) (any, error) {
			cancels++
			return map[string]any{"status": "cancelling"}, nil
		})
		h.fake.on("mercadopago", capability.CommercePaymentsReconcile, func(in map[string]any) (any, error) {
			if in["cursor"] == nil {
				return map[string]any{"payments": []any{statusOf("ORD-r1", "paid", "100.00")}, "next_cursor": "p2"}, nil
			}
			// Money nobody recorded (e.g. a session that timed out but paid).
			return map[string]any{"payments": []any{statusOf("ORD-ghost", "paid", "75.00")}}, nil
		})
		r1 := terminalCharge("r1")
		r2 := terminalCharge("r2")
		r2.DocumentID = "SO-2"
		for _, req := range []ChargeRequest{r1, r2} {
			if _, err := h.l.StartCharge(ctx, h.org, req); err != nil {
				t.Fatal(err)
			}
		}
		rec := &Reconciler{Ledger: h.l, Owner: "replica-a"}
		h.advance(time.Minute)
		// Another replica holding the lease: skipped.
		other := &Reconciler{Ledger: h.l, Owner: "replica-b", Lease: time.Hour}
		if ok, err := other.claim(ctx, h.org); err != nil || !ok {
			t.Fatalf("claim: %v %v", ok, err)
		}
		if rep, err := rec.RunOrg(ctx, h.org); err != nil || !rep.Skipped {
			t.Fatalf("leased org not skipped: %+v %v", rep, err)
		}
		other.release(ctx, h.org, &Report{})
		orgs, err := rec.Orgs(ctx)
		if err != nil || len(orgs) != 1 || orgs[0] != h.org {
			t.Fatalf("orgs %v %v", orgs, err)
		}
		rep, err := rec.RunOrg(ctx, h.org)
		if err != nil || rep.Refreshed < 2 || rep.Applied < 1 || rep.Orphans != 1 || rep.Listed != 2 || len(rep.Errors) != 0 {
			t.Fatalf("run 1: %+v %v", rep, err)
		}
		doc, _ := h.l.Document(ctx, h.org, "sales_order", "SO-1")
		if doc.NetPaid != "100.00" {
			t.Fatalf("closed screen still paid: %+v", doc)
		}
		// SO-2 stays on the device past its expiry: cancel requested, re-read,
		// and raised as stuck while the provider keeps it open.
		h.advance(15 * time.Minute)
		rep, err = rec.RunOrg(ctx, h.org)
		if err != nil || rep.Cancelled != 1 || rep.Stuck != 1 || cancels != 1 {
			t.Fatalf("run 2: %+v %v cancels=%d", rep, err, cancels)
		}
		found := false
		for _, a := range h.anomalies {
			found = found || (a.Reason == ReasonStuck && a.DocumentID == "SO-2")
		}
		if !found {
			t.Fatalf("stuck not raised: %+v", h.anomalies)
		}
		// The provider finally expires it.
		states["ORD-r2"] = "expired"
		h.advance(time.Minute)
		if _, err := rec.RunOrg(ctx, h.org); err != nil {
			t.Fatal(err)
		}
		if doc, _ := h.l.Document(ctx, h.org, "sales_order", "SO-2"); doc.Payments[0].Status != payments.StatusExpired {
			t.Fatalf("SO-2: %+v", doc.Payments[0])
		}
		// RunOnce goes over every org.
		h.advance(time.Minute)
		if reps := rec.RunOnce(ctx); len(reps) != 1 {
			t.Fatalf("RunOnce %+v", reps)
		}
	})
}
