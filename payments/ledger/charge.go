package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/asteby/metacore-kernel/capability"
	"github.com/asteby/metacore-kernel/payments"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// ChargeRequest starts collecting an amount of a document.
type ChargeRequest struct {
	DocumentType string
	DocumentID   string
	OrderRef     string
	// DocumentTotal is the document total computed server-side from its
	// stored lines (payments.CheckLinesTotal is the channel's job).
	DocumentTotal string
	// Amount is what this payment collects (one tender of a split tender).
	Amount   string
	Currency string
	Channel  payments.Channel
	// Provider is the addon key ("cash" for drawer cash). Terminals: the
	// provider of the device.
	Provider string
	DeviceID string
	// IdempotencyKey is generated once per charge attempt and reused on
	// every retry; it is also the provider's idempotency key.
	IdempotencyKey string
	// HoldToken is the document hold the charge runs under (required when a
	// live hold exists).
	HoldToken string
	Market    string
	// Cash details of a drawer payment (tendered, change, rounding).
	Cash *payments.CashTender
	// Seller routes a marketplace payment.
	Seller *payments.Seller
	// ProviderInput carries the extra fields of commerce.payments.session /
	// terminal.create (return_url, buyer, lines, description, installments,
	// card_type, expires_in_seconds, ...). The ledger sets the money,
	// reference, device and key fields itself.
	ProviderInput map[string]any
	TestMode      bool
	// Caller is the addon that asked (host-stamped).
	Caller string
}

// ChargeResult is the answer to StartCharge.
type ChargeResult struct {
	Verdict payments.Verdict `json:"verdict"`
	Payment payments.Payment `json:"payment"`
	// Provider is the provider's raw output (session: redirect_url, qr_data,
	// client_secret...; terminal: expires_at, message...).
	Provider json.RawMessage `json:"provider_output,omitempty"`
	// Indeterminate: the provider call timed out; the payment stays pending
	// and is reconciled (do not start another attempt).
	Indeterminate bool `json:"indeterminate,omitempty"`
}

// StartCharge records a charge attempt and sends it to the provider. Under
// the document lock it checks the hold (payments.CheckCharge) and the
// replay rules (payments.CheckReplay): a replay, an already-paid document or
// an open payment answer without creating anything. Drawer cash is captured
// at once; every other channel is created pending and sent to the provider
// (commerce.payments.terminal.create or commerce.payments.session, pinned to
// req.Provider), whose answer is recorded.
func (l *Ledger) StartCharge(ctx context.Context, org uuid.UUID, req ChargeRequest) (ChargeResult, error) {
	if err := validateCharge(req); err != nil {
		return ChargeResult{}, err
	}
	var res ChargeResult
	var created *paymentRow
	err := l.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockDocument(tx, org, req.DocumentType, req.DocumentID); err != nil {
			return err
		}
		hold, err := activeHold(tx, org, req.DocumentType, req.DocumentID)
		if err != nil {
			return err
		}
		now := l.now()
		if hold != nil {
			h := hold.hold()
			if err := payments.CheckCharge(&h, hashToken(req.HoldToken), now); err != nil {
				return fail(CodeInPayment, err)
			}
		}
		doc, err := l.document(tx, org, req.DocumentType, req.DocumentID)
		if err != nil {
			return err
		}
		// The key is unique per provider: a key reused with another provider
		// is still this attempt.
		verdict, existing, err := payments.CheckReplay(doc.Payments, doc.Refunds, req.IdempotencyKey, req.DocumentTotal)
		if err != nil {
			return fail(CodeInvalidInput, err)
		}
		if verdict != payments.VerdictNew {
			res.Verdict = verdict
			if existing != nil {
				res.Payment = *existing
			}
			return nil
		}
		due := payments.MustAmount(req.DocumentTotal).Sub(payments.MustAmount(doc.Paid))
		if payments.MustAmount(req.Amount).Cmp(due) > 0 {
			return failf(CodeExceedsDue, "amount %s exceeds the %s still due", req.Amount, due)
		}
		row := paymentRow{
			ID: uuid.New(), OrganizationID: org, DocumentType: req.DocumentType, DocumentID: req.DocumentID,
			OrderRef: firstNonBlank(req.OrderRef, req.DocumentID), Channel: string(req.Channel), Provider: req.Provider,
			DeviceID: req.DeviceID, IdempotencyKey: req.IdempotencyKey, Status: string(payments.StatusPending),
			Amount: req.Amount, Currency: req.Currency, DocumentTotal: strPtr(req.DocumentTotal), AmountRefunded: "0", Seller: jsonPtr(req.Seller),
			TestMode: req.TestMode, CreatedBy: req.Caller, CreatedAt: now, UpdatedAt: now,
		}
		if req.Channel == payments.ChannelCash {
			// Drawer cash: counted by staff, nothing to re-read.
			ref := row.ID.String()
			row.ProviderRef, row.Status, row.Method, row.PaidAt = &ref, string(payments.StatusPaid), "cash", &now
			row.Cash = jsonPtr(req.Cash)
		}
		if errs := row.payment().Validate(); len(errs) > 0 {
			return failf(CodeInvalidInput, "%s", strings.Join(errs, "; "))
		}
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		if row.Status == string(payments.StatusPaid) {
			if err := l.settleDocument(tx, org, req.DocumentType, req.DocumentID, req.DocumentTotal); err != nil {
				return err
			}
		}
		res.Verdict, res.Payment, created = payments.VerdictNew, row.payment(), &row
		return nil
	})
	if err != nil || created == nil {
		return res, err
	}
	if req.Channel == payments.ChannelCash {
		l.publish(ctx, org, *created, outcomeOf(created.payment()), payments.SourceStaff)
		return res, nil
	}
	return l.sendToProvider(ctx, org, req, *created)
}

// settleDocument closes the live hold of a document once it is paid in full:
// any hold on a paid document is moot (payments.Release with force).
func (l *Ledger) settleDocument(tx *gorm.DB, org uuid.UUID, docType, docID, total string) error {
	if total == "" {
		return nil
	}
	doc, err := l.document(tx, org, docType, docID)
	if err != nil {
		return err
	}
	if payments.MustAmount(doc.Paid).Cmp(payments.MustAmount(total)) < 0 {
		return nil
	}
	return tx.Model(&holdRow{}).Where("organization_id = ? AND document_type = ? AND document_id = ? AND status = ?",
		org, docType, docID, string(payments.HoldActive)).
		Updates(map[string]any{"status": string(payments.HoldConsumed), "updated_at": l.now()}).Error
}

func validateCharge(req ChargeRequest) error {
	var errs []string
	if blank(req.DocumentType) || blank(req.DocumentID) {
		errs = append(errs, "document_type and document_id are required")
	}
	if blank(req.IdempotencyKey) {
		errs = append(errs, "idempotency_key is required")
	}
	if !req.Channel.Valid() {
		errs = append(errs, fmt.Sprintf("channel %q is not one of %v", req.Channel, capability.PaymentChannels))
	}
	if blank(req.Provider) {
		errs = append(errs, "provider is required")
	}
	if (req.Channel == payments.ChannelCash) != (req.Provider == payments.ProviderCash) {
		errs = append(errs, "channel cash goes with provider cash and only with it")
	}
	if req.Channel == payments.ChannelTerminal && blank(req.DeviceID) {
		errs = append(errs, "device_id is required for a terminal charge")
	}
	for name, v := range map[string]string{"amount": req.Amount, "document_total": req.DocumentTotal} {
		a, err := payments.ParseAmount(v)
		if err != nil || a.Sign() <= 0 {
			errs = append(errs, name+" must be a positive decimal string")
		}
	}
	if !capability.ValidCurrency(req.Currency) {
		errs = append(errs, "currency must be ISO 4217")
	}
	if len(errs) > 0 {
		return failf(CodeInvalidInput, "%s", strings.Join(errs, "; "))
	}
	return nil
}

func firstNonBlank(vals ...string) string {
	for _, v := range vals {
		if !blank(v) {
			return v
		}
	}
	return ""
}

// creation outputs of commerce.payments.terminal.create / session.
type creationOutput struct {
	ProviderRef string `json:"provider_ref"`
	SessionID   string `json:"session_id"`
	Status      string `json:"status"`
	DeviceID    string `json:"device_id"`
	ExpiresAt   string `json:"expires_at"`
	TestMode    bool   `json:"test_mode"`
}

func (l *Ledger) sendToProvider(ctx context.Context, org uuid.UUID, req ChargeRequest, row paymentRow) (ChargeResult, error) {
	res := ChargeResult{Verdict: payments.VerdictNew, Payment: row.payment()}
	if l.cfg.Providers == nil {
		return res, failf(CodeUnavailable, "no provider querier configured")
	}
	input := map[string]any{}
	for k, v := range req.ProviderInput {
		input[k] = v
	}
	input["order_ref"] = row.OrderRef
	input["amount"] = req.Amount
	input["currency"] = req.Currency
	input["idempotency_key"] = req.IdempotencyKey
	key := capability.CommercePaymentsSession
	if req.Channel == payments.ChannelTerminal {
		key = capability.CommercePaymentsTerminalCreate
		input["device_id"] = req.DeviceID
	} else {
		input["channel"] = string(req.Channel)
	}
	if req.Seller != nil {
		input["seller"] = req.Seller
	}
	qr, err := l.cfg.Providers.Query(ctx, capability.Query{
		Org: org, Market: capability.Market{Code: req.Market}, Capability: key, Input: input, Provider: req.Provider,
	})
	var out json.RawMessage
	var r capability.Result
	if err == nil {
		if len(qr.Results) > 0 {
			r = qr.Results[0]
		}
		out, err = r.Output, r.Err
		if err == nil && len(qr.Results) == 0 {
			err = errors.New("provider returned nothing")
		}
	}
	if err != nil {
		if r.Indeterminate() {
			// The provider may have created it: keep pending, reconcile.
			res.Indeterminate = true
			_ = l.touch(ctx, org, row.ID, "indeterminate: "+err.Error())
			return res, nil
		}
		// A definite refusal: nothing was created at the provider.
		code := r.ErrorCode()
		if code == "" {
			code = CodeProvider
		}
		if ferr := l.markFailed(ctx, org, row.ID, code+": "+err.Error()); ferr != nil {
			return res, ferr
		}
		p, _ := l.Payment(ctx, org, row.ID.String())
		res.Payment = p
		return res, fail(code, err)
	}
	res.Provider = out
	var co creationOutput
	_ = json.Unmarshal(out, &co)
	ref := firstNonBlank(co.ProviderRef)
	if req.Channel != payments.ChannelTerminal {
		// A session: its id follows the payment until the provider pays it.
		row.SessionID = co.SessionID
	} else {
		row.SessionID = co.ProviderRef
	}
	status := payments.Status(co.Status)
	if co.Status == "created" || !status.Valid() {
		status = payments.StatusPending
	}
	updates := map[string]any{"session_id": row.SessionID, "updated_at": l.now(), "test_mode": co.TestMode || req.TestMode}
	if ref != "" {
		updates["provider_ref"] = ref
	}
	if co.DeviceID != "" {
		updates["device_id"] = co.DeviceID
	}
	if t, perr := time.Parse(time.RFC3339, co.ExpiresAt); perr == nil {
		updates["expires_at"] = t.UTC()
	}
	if err := l.db.WithContext(ctx).Model(&paymentRow{}).Where("organization_id = ? AND id = ?", org, row.ID).Updates(updates).Error; err != nil {
		return res, err
	}
	// The creation answer is a status like any other: classify and record
	// it (pending → at_device / paid / failed ...).
	if status != payments.StatusPending {
		p, _ := l.Payment(ctx, org, row.ID.String())
		o := outcomeOf(p)
		o.Status = status
		if _, err := l.observe(ctx, org, row.Provider, o, payments.SourcePoll); err != nil {
			return res, err
		}
	}
	p, err := l.Payment(ctx, org, row.ID.String())
	res.Payment = p
	return res, err
}

func (l *Ledger) touch(ctx context.Context, org uuid.UUID, id uuid.UUID, detail string) error {
	return l.db.WithContext(ctx).Model(&paymentRow{}).Where("organization_id = ? AND id = ?", org, id).
		Updates(map[string]any{"status_detail": truncate(detail, 500), "updated_at": l.now()}).Error
}

func (l *Ledger) markFailed(ctx context.Context, org uuid.UUID, id uuid.UUID, detail string) error {
	return l.db.WithContext(ctx).Model(&paymentRow{}).
		Where("organization_id = ? AND id = ? AND status = ?", org, id, string(payments.StatusPending)).
		Updates(map[string]any{"status": string(payments.StatusFailed), "status_detail": truncate(detail, 500), "updated_at": l.now()}).Error
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// Refresh re-reads a payment from its provider (commerce.payments.status
// pinned to it) and records what it says. Drawer cash has nothing to re-read.
func (l *Ledger) Refresh(ctx context.Context, org uuid.UUID, paymentID string, source payments.Source) (Observation, error) {
	row, err := l.paymentRow(l.db.WithContext(ctx), org, paymentID)
	if err != nil {
		return Observation{}, err
	}
	p := row.payment()
	if p.Provider == payments.ProviderCash {
		return Observation{Payment: p, Decision: payments.Ignore, Reason: "no_provider_state"}, nil
	}
	o, err := l.readStatus(ctx, org, p)
	if err != nil {
		_ = l.db.WithContext(ctx).Model(&paymentRow{}).Where("id = ?", row.ID).
			Updates(map[string]any{"last_checked_at": l.now(), "status_detail": truncate("status: "+err.Error(), 500)}).Error
		return Observation{Payment: p}, err
	}
	return l.observe(ctx, org, p.Provider, o, source)
}

func (l *Ledger) readStatus(ctx context.Context, org uuid.UUID, p payments.Payment) (payments.Outcome, error) {
	if l.cfg.Providers == nil {
		return payments.Outcome{}, failf(CodeUnavailable, "no provider querier configured")
	}
	input := map[string]any{"order_ref": p.OrderRef}
	if p.ProviderRef != "" {
		input["provider_ref"] = p.ProviderRef
	}
	if p.SessionID != "" {
		input["session_id"] = p.SessionID
	}
	qr, err := l.cfg.Providers.Query(ctx, capability.Query{Org: org, Capability: capability.CommercePaymentsStatus, Input: input, Provider: p.Provider})
	if err != nil {
		return payments.Outcome{}, fail(CodeProvider, err)
	}
	raw, err := qr.One()
	if err != nil {
		return payments.Outcome{}, fail(CodeProvider, err)
	}
	var o payments.Outcome
	if err := json.Unmarshal(raw, &o); err != nil {
		return o, fail(CodeProvider, err)
	}
	return o, nil
}

// Cancel asks the provider to cancel an open payment, records the
// cancellation request (cancelling) and re-reads the payment: a cancel the
// device lost to the buyer ends paid, and that is what gets recorded.
func (l *Ledger) Cancel(ctx context.Context, org uuid.UUID, paymentID, idempotencyKey, reason string) (Observation, error) {
	row, err := l.paymentRow(l.db.WithContext(ctx), org, paymentID)
	if err != nil {
		return Observation{}, err
	}
	p := row.payment()
	if blank(idempotencyKey) {
		return Observation{Payment: p}, failf(CodeInvalidInput, "idempotency_key is required")
	}
	if !p.Status.Open() {
		return Observation{Payment: p}, failf(CodeInvalidTransition, "a %s payment cannot be cancelled (refund it)", p.Status)
	}
	if l.cfg.Providers == nil {
		return Observation{Payment: p}, failf(CodeUnavailable, "no provider querier configured")
	}
	input := map[string]any{"order_ref": p.OrderRef, "idempotency_key": idempotencyKey, "reason": reason}
	if p.ProviderRef != "" {
		input["provider_ref"] = p.ProviderRef
	}
	if p.SessionID != "" {
		input["session_id"] = p.SessionID
	}
	if p.DeviceID != "" {
		input["device_id"] = p.DeviceID
	}
	qr, err := l.cfg.Providers.Query(ctx, capability.Query{Org: org, Capability: capability.CommercePaymentsCancel, Input: input, Provider: p.Provider})
	if err == nil {
		var raw json.RawMessage
		if raw, err = qr.One(); err == nil {
			var out struct {
				Status string `json:"status"`
			}
			_ = json.Unmarshal(raw, &out)
			if out.Status == "cancelling" && p.Status != payments.StatusCancelling {
				if terr := payments.Transition(p.Status, payments.StatusCancelling); terr == nil {
					_ = l.db.WithContext(ctx).Model(&paymentRow{}).Where("id = ? AND status = ?", row.ID, string(p.Status)).
						Updates(map[string]any{"status": string(payments.StatusCancelling), "cancelled_from": string(p.Status), "updated_at": l.now()}).Error
				}
			}
		}
	}
	// Whatever the cancel said, the provider's status decides.
	obs, rerr := l.Refresh(ctx, org, paymentID, payments.SourcePoll)
	if err != nil {
		return obs, fail(CodeProvider, err)
	}
	return obs, rerr
}
