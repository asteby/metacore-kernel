package ledger

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/asteby/metacore-kernel/capability"
	"github.com/asteby/metacore-kernel/payments"
	"github.com/google/uuid"
)

// Host-implemented contracts: the ledger's API for guests. A channel or a
// provider addon calls them with the capability_query import like any
// capability, gated by its manifest grant (`capability:query payments.charge.*`),
// and the host answers from the ledger instead of resolving a provider. The
// guest never names an organization: the host binds the invocation's.
const (
	HoldAcquire   = "payments.hold.acquire"
	HoldRelease   = "payments.hold.release"
	ChargeStart   = "payments.charge.start"
	ChargeRefresh = "payments.charge.refresh"
	ChargeCancel  = "payments.charge.cancel"
	DocumentGet   = "payments.document.get"
	RefundCreate  = "payments.refund.create"
	RefundSettle  = "payments.refund.settle"
	// ObserveKey is for provider addons only: record what their webhook
	// re-read about one of their payments.
	ObserveKey = "payments.observe"
)

// HostProvider is the provider key host-implemented results carry.
const HostProvider = "kernel"

// Namespace is the prefix of every host-implemented ledger contract.
const Namespace = "payments."

func init() {
	for _, c := range hostContracts() {
		if err := capability.Register(c); err != nil {
			panic(err)
		}
	}
}

func r(name string, t capability.FieldType, doc string) capability.Field {
	return capability.Field{Name: name, Type: t, Required: true, Doc: doc}
}

func o(name string, t capability.FieldType, doc string) capability.Field {
	return capability.Field{Name: name, Type: t, Doc: doc}
}

func in(fields ...capability.Field) *capability.Schema { return &capability.Schema{Fields: fields} }

// out schemas are open: the host builds them, guests read the documented keys.
func out(fields ...capability.Field) *capability.Schema {
	return &capability.Schema{Fields: fields, Open: true}
}

func hostContracts() []capability.Contract {
	doc := []capability.Field{
		r("document_type", capability.TypeString, "paid document kind (sales_order, checkout, invoice, layaway...)"),
		r("document_id", capability.TypeString, "paid document id"),
	}
	payment := o("payment", capability.TypeObject, "canonical payment (payments.Payment)")
	observation := out(payment, o("decision", capability.TypeString, "apply | ignore | anomaly"), o("reason", capability.TypeString, ""),
		o("duplicate", capability.TypeBoolean, ""), o("corrected", capability.TypeBoolean, ""), o("orphan", capability.TypeBoolean, ""))
	refund := out(o("refund", capability.TypeObject, "canonical refund (payments.Refund)"), payment,
		o("replay", capability.TypeBoolean, "same idempotency_key: the existing refund"), o("instructions", capability.TypeString, ""),
		o("indeterminate", capability.TypeBoolean, "provider timed out: stays pending, retry with the same key"))
	q := func(key, docText string, input, output *capability.Schema, mutating bool) capability.Contract {
		return capability.Contract{Key: key, Kind: capability.KindQuery, Cardinality: capability.One, Doc: docText,
			Input: input, Output: output, Idempotent: mutating}
	}
	return []capability.Contract{
		settleContract(),
		q(HoldAcquire, "take or renew the charge hold of a document before a physical charge (one live hold per document across POS, kiosk, cash desk, storefront)",
			in(append(append([]capability.Field{}, doc...),
				r("holder", capability.TypeString, "point of sale shown to others: pos:<register>, kiosk:<station>"),
				o("channel", capability.TypeString, "terminal / cash / ..."),
				o("user_id", capability.TypeString, ""),
				o("token", capability.TypeString, "renew your own hold"),
				o("ttl_seconds", capability.TypeInteger, "default 300, bounded to [30, 3600]"))...),
			out(r("token", capability.TypeString, "secret; present it to charge, renew and release"),
				r("holder", capability.TypeString, ""), r("expires_at", capability.TypeString, "RFC 3339")), false),
		q(HoldRelease, "release your charge hold of a document",
			in(append(append([]capability.Field{}, doc...), r("token", capability.TypeString, ""))...),
			out(r("released", capability.TypeBoolean, "")), false),
		q(ChargeStart, "charge (part of) a document: hold check, idempotent replay, then the provider (terminal.create or session) or drawer cash",
			in(append(append([]capability.Field{}, doc...),
				r("document_total", capability.TypeDecimal, "document total from its STORED lines"),
				r("amount", capability.TypeDecimal, "this payment's amount (one tender)"),
				r("currency", capability.TypeCurrency, ""),
				capability.Field{Name: "channel", Type: capability.TypeString, Required: true, Enum: capability.PaymentChannels},
				r("provider", capability.TypeString, "provider addon key; cash for drawer cash"),
				r("idempotency_key", capability.TypeString, "generated once per charge attempt, reused on every retry"),
				o("order_ref", capability.TypeString, "reference sent to the provider (default document_id)"),
				o("device_id", capability.TypeString, "terminal / cash recycler (channel terminal)"),
				o("hold_token", capability.TypeString, "required when the document has a live hold"),
				o("market", capability.TypeString, ""),
				o("cash", capability.TypeObject, "drawer cash: tendered, change, rounding"),
				o("seller", capability.TypeObject, "marketplace seller (account_ref, application_fee)"),
				o("provider_input", capability.TypeObject, "extra session / terminal.create fields (return_url, buyer, lines, installments...)"),
				o("test_mode", capability.TypeBoolean, ""))...),
			out(r("verdict", capability.TypeString, "new | replay | already_paid | in_progress | retry_with_new_key"), payment,
				o("provider_output", capability.TypeObject, "session / terminal.create output (redirect_url, qr_data...)"),
				o("indeterminate", capability.TypeBoolean, "provider timed out: the payment stays pending and is reconciled")), true),
		q(ChargeRefresh, "re-read a payment from its provider and record it (polling while it is on a device)",
			in(r("payment_id", capability.TypeString, "")), observation, false),
		q(ChargeCancel, "cancel an open payment at its provider and re-read it (it may still end paid)",
			in(r("payment_id", capability.TypeString, ""), r("idempotency_key", capability.TypeString, ""),
				o("reason", capability.TypeString, "")), observation, true),
		q(DocumentGet, "payments, refunds, net paid and live hold of a document",
			in(doc...), out(r("payments", capability.TypeArray, ""), r("refunds", capability.TypeArray, ""),
				r("net_paid", capability.TypeDecimal, ""), r("open", capability.TypeBoolean, ""), o("hold", capability.TypeObject, "")), false),
		q(RefundCreate, "refund a captured payment to its original method, the wallet or cash",
			in(r("payment_id", capability.TypeString, ""), r("amount", capability.TypeDecimal, ""),
				capability.Field{Name: "destination", Type: capability.TypeString, Required: true, Enum: []string{"original_method", "wallet", "cash"}},
				r("idempotency_key", capability.TypeString, "stable per refund (e.g. return id + payment id)"),
				o("reason", capability.TypeString, ""), o("source_type", capability.TypeString, "sales_return / cancellation / dispute"),
				o("source_id", capability.TypeString, ""),
				o("credit_note_required", capability.TypeBoolean, "the paid document was invoiced: the refund is complete only with its credit note"),
				o("refund_application_fee", capability.TypeBoolean, "marketplace")), refund, true),
		q(RefundSettle, "settle a wallet / cash / manual refund, or record its credit note",
			in(r("refund_id", capability.TypeString, ""),
				capability.Field{Name: "status", Type: capability.TypeString, Enum: []string{"succeeded", "failed", "manual_required"}},
				o("cash_session_id", capability.TypeString, ""), o("credit_note_id", capability.TypeString, "")), refund, false),
		q(ObserveKey, "provider only: record a re-read status of one of YOUR payments (from your webhook)",
			in(r("result", capability.TypeObject, "commerce.payments.status output, re-read from the provider API"),
				capability.Field{Name: "source", Type: capability.TypeString, Enum: capability.PaymentEventSources}), observation, false),
	}
}

// HostQuerier answers the ledger contracts and passes every other query to
// Inner. The host installs it as the capability querier of the wasm host.
type HostQuerier struct {
	Inner  capability.Querier
	Ledger *Ledger
}

// Query implements capability.Querier.
func (h HostQuerier) Query(ctx context.Context, q capability.Query) (capability.QueryResult, error) {
	if !strings.HasPrefix(q.Capability, Namespace) {
		if h.Inner == nil {
			return capability.QueryResult{Capability: q.Capability}, fmt.Errorf("%w: no querier", capability.ErrNoProvider)
		}
		return h.Inner.Query(ctx, q)
	}
	res := capability.QueryResult{Capability: q.Capability, Market: q.Market.Code}
	c, ok := capability.Lookup(q.Capability)
	// payments.settle is a pure rule: answered with or without a ledger.
	if !ok || (h.Ledger == nil && q.Capability != SettleKey) {
		return res, fmt.Errorf("%w: unknown ledger contract %q", capability.ErrNotQuery, q.Capability)
	}
	if problems := c.CheckInput(q.Input); len(problems) > 0 {
		return res, &capability.InputError{Capability: c.Key, Problems: problems}
	}
	start := time.Now()
	value, err := h.dispatch(ctx, q)
	r := capability.Result{Provider: HostProvider, Duration: time.Since(start)}
	if err != nil {
		r.Err = capability.NewCodedError(ErrorCode(err), err.Error())
	} else {
		r.Output = mustJSON(value)
	}
	res.Results = []capability.Result{r}
	return res, nil
}

func (h HostQuerier) dispatch(ctx context.Context, q capability.Query) (any, error) {
	l, org, in := h.Ledger, q.Org, q.Input
	if q.Capability == SettleKey {
		return settle(in)
	}
	if org == uuid.Nil {
		return nil, failf(CodeInvalidInput, "no organization")
	}
	switch q.Capability {
	case HoldAcquire:
		res, err := l.AcquireHold(ctx, org, payments.HoldRequest{
			DocumentType: str(in, "document_type"), DocumentID: str(in, "document_id"), Holder: str(in, "holder"),
			Channel: payments.Channel(str(in, "channel")), UserID: str(in, "user_id"), Token: str(in, "token"),
			TTL: time.Duration(integer(in, "ttl_seconds")) * time.Second,
		}, q.Caller)
		if err != nil {
			return nil, err
		}
		return map[string]any{"token": res.Token, "holder": res.Hold.Holder, "expires_at": res.Hold.ExpiresAt.Format(time.RFC3339)}, nil
	case HoldRelease:
		// A guest releases only its own hold; force is the ledger's (paid
		// documents) and the host's.
		force := q.Caller == "" && boolean(in, "force")
		if err := l.ReleaseHold(ctx, org, str(in, "document_type"), str(in, "document_id"), str(in, "token"), force); err != nil {
			return nil, err
		}
		return map[string]any{"released": true}, nil
	case ChargeStart:
		req := ChargeRequest{
			DocumentType: str(in, "document_type"), DocumentID: str(in, "document_id"), OrderRef: str(in, "order_ref"),
			DocumentTotal: str(in, "document_total"), Amount: str(in, "amount"), Currency: str(in, "currency"),
			Channel: payments.Channel(str(in, "channel")), Provider: str(in, "provider"), DeviceID: str(in, "device_id"),
			IdempotencyKey: str(in, "idempotency_key"), HoldToken: str(in, "hold_token"), Market: firstNonBlank(str(in, "market"), q.Market.Code),
			TestMode: boolean(in, "test_mode"), Caller: q.Caller,
		}
		if err := decode(in["cash"], &req.Cash); err != nil {
			return nil, failf(CodeInvalidInput, "cash: %v", err)
		}
		if err := decode(in["seller"], &req.Seller); err != nil {
			return nil, failf(CodeInvalidInput, "seller: %v", err)
		}
		if m, ok := in["provider_input"].(map[string]any); ok {
			req.ProviderInput = m
		}
		return l.StartCharge(ctx, org, req)
	case ChargeRefresh:
		return l.Refresh(ctx, org, str(in, "payment_id"), payments.SourcePoll)
	case ChargeCancel:
		return l.Cancel(ctx, org, str(in, "payment_id"), str(in, "idempotency_key"), str(in, "reason"))
	case DocumentGet:
		return l.Document(ctx, org, str(in, "document_type"), str(in, "document_id"))
	case RefundCreate:
		return l.CreateRefund(ctx, org, RefundRequest{
			PaymentID: str(in, "payment_id"), Amount: str(in, "amount"), Destination: payments.RefundDestination(str(in, "destination")),
			IdempotencyKey: str(in, "idempotency_key"), Reason: str(in, "reason"), SourceType: str(in, "source_type"),
			SourceID: str(in, "source_id"), CreditNoteRequired: boolean(in, "credit_note_required"),
			RefundApplicationFee: boolean(in, "refund_application_fee"), Caller: q.Caller,
		})
	case RefundSettle:
		return l.SettleRefund(ctx, org, SettleRequest{
			RefundID: str(in, "refund_id"), Status: payments.RefundStatus(str(in, "status")),
			CashSessionID: str(in, "cash_session_id"), CreditNoteID: str(in, "credit_note_id"),
		})
	case ObserveKey:
		var outcome payments.Outcome
		if err := decode(in["result"], &outcome); err != nil {
			return nil, failf(CodeInvalidInput, "result: %v", err)
		}
		source := payments.Source(str(in, "source"))
		if source == payments.SourceStaff && q.Caller != "" {
			source = payments.SourceWebhook
		}
		// q.Provider is ignored for guests: Observe forces the caller.
		return l.Observe(ctx, org, q.Caller, q.Provider, outcome, source)
	}
	return nil, failf(CodeInvalidInput, "unknown ledger contract %q", q.Capability)
}

func str(m map[string]any, k string) string {
	switch v := m[k].(type) {
	case string:
		return strings.TrimSpace(v)
	case json.Number:
		return v.String()
	case nil:
		return ""
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

func boolean(m map[string]any, k string) bool { b, _ := m[k].(bool); return b }

func integer(m map[string]any, k string) int {
	switch v := m[k].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	}
	return 0
}

func decode(v any, dst any) error {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dst)
}
