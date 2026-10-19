package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/asteby/metacore-kernel/capability"
	"github.com/asteby/metacore-kernel/payments"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// RefundRequest refunds part or all of a captured payment.
type RefundRequest struct {
	PaymentID   string
	Amount      string
	Destination payments.RefundDestination
	// IdempotencyKey is stable per refund operation (e.g. return id + payment).
	IdempotencyKey     string
	Reason             string
	SourceType         string
	SourceID           string
	CreditNoteRequired bool
	// AdjustsTotal: the refund comes with a matching reduction of the
	// document total (a return, a credit note); only then does it lower what
	// counts as paid (payments.Refund.AdjustsTotal).
	AdjustsTotal bool
	// RefundApplicationFee (marketplace) also returns the platform commission.
	RefundApplicationFee bool
	Caller               string
}

// RefundResult is a created (or replayed) refund and its payment.
type RefundResult struct {
	Refund        payments.Refund  `json:"refund"`
	Payment       payments.Payment `json:"payment"`
	Replay        bool             `json:"replay,omitempty"`
	Instructions  string           `json:"instructions,omitempty"`
	Indeterminate bool             `json:"indeterminate,omitempty"`
}

// CreateRefund validates and records a refund (payments.CheckRefund under a
// lock of the payment), then, for the original method, asks the payment's
// provider (commerce.payments.refund, same idempotency key on every retry).
// Wallet and cash refunds stay pending until the channel that pays them out
// settles them (SettleRefund).
func (l *Ledger) CreateRefund(ctx context.Context, org uuid.UUID, req RefundRequest) (RefundResult, error) {
	var res RefundResult
	var created, recheck *refundRow
	var pay paymentRow
	err := l.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		row, err := l.paymentRow(forUpdate(tx), org, req.PaymentID)
		if err != nil {
			return err
		}
		pay = row
		p := row.payment()
		existing, err := l.refundsOf(tx, org, row.ID)
		if err != nil {
			return err
		}
		next := payments.Refund{
			PaymentID: p.ID, Destination: req.Destination, Amount: req.Amount, Currency: p.Currency,
			IdempotencyKey: req.IdempotencyKey,
		}
		replay, err := payments.CheckRefund(p, existing, next)
		switch {
		case errors.Is(err, payments.ErrRefundExceeds):
			return fail(CodeRefundExceeds, err)
		case errors.Is(err, payments.ErrIdempotencyConflict):
			return fail(CodeIdempotency, err)
		case err != nil:
			return fail(CodeNotRefundable, err)
		case replay != nil:
			res = RefundResult{Refund: *replay, Payment: p, Replay: true}
			if replay.Status == payments.RefundPending && replay.Destination == payments.RefundToOriginal {
				// A retry of a refund still pending at the provider asks it
				// again (recheckRefund: a read once numbered, the same key
				// only inside the re-POST window).
				var r refundRow
				if err := tx.Where("organization_id = ? AND id = ?", org, replay.ID).Take(&r).Error; err != nil {
					return err
				}
				recheck = &r
			}
			return nil
		}
		if req.Destination == payments.RefundToOriginal && p.Provider == payments.ProviderCash {
			return failf(CodeNotRefundable, "drawer cash has no provider to refund through: use destination cash or wallet")
		}
		now := l.now()
		r := refundRow{
			ID: uuid.New(), OrganizationID: org, PaymentID: row.ID, Destination: string(req.Destination),
			Amount: norm(req.Amount), Currency: p.Currency, Status: string(payments.RefundPending), Reason: req.Reason,
			IdempotencyKey: req.IdempotencyKey, SourceType: req.SourceType, SourceID: req.SourceID,
			CreditNoteRequired: req.CreditNoteRequired, AdjustsTotal: req.AdjustsTotal, RefundApplicationFee: req.RefundApplicationFee,
			CreatedBy: req.Caller, CreatedAt: now, UpdatedAt: now,
		}
		if req.Destination == payments.RefundToOriginal {
			r.Provider = p.Provider
		}
		if err := tx.Create(&r).Error; err != nil {
			return err
		}
		created = &r
		res = RefundResult{Refund: r.refund(), Payment: p}
		return nil
	})
	if err == nil && recheck != nil {
		out, rerr := l.recheckRefund(ctx, org, pay, *recheck)
		out.Replay = true
		if rerr != nil && ErrorCode(rerr) == CodeProvider {
			// Still pending: the retry got its refund back, unresolved.
			return out, nil
		}
		return out, rerr
	}
	if err != nil || created == nil || req.Destination != payments.RefundToOriginal {
		return res, err
	}
	return l.refundAtProvider(ctx, org, pay, *created, true)
}

// refundAtProvider asks the payment's provider for refund r
// (commerce.payments.refund, the refund's own idempotency key). first is the
// original request; a re-POST (first=false, only through recheckRefund)
// sends the same request and only moves the refund forward: an error there
// never marks it failed, since the first request may have gone through.
func (l *Ledger) refundAtProvider(ctx context.Context, org uuid.UUID, pay paymentRow, r refundRow, first bool) (RefundResult, error) {
	p := pay.payment()
	input := map[string]any{
		"provider_ref": p.ProviderRef, "amount": r.Amount, "currency": r.Currency, "order_ref": p.OrderRef,
		"reason": r.Reason, "idempotency_key": r.IdempotencyKey,
	}
	if p.Seller != nil {
		input["seller"] = p.Seller
		input["refund_application_fee"] = r.RefundApplicationFee
	}
	return l.askRefund(ctx, org, pay, r, capability.CommercePaymentsRefund, input, first)
}

// refundStatusAtProvider re-reads a refund the provider already numbered
// (commerce.payments.refund.status, read only).
func (l *Ledger) refundStatusAtProvider(ctx context.Context, org uuid.UUID, pay paymentRow, r refundRow) (RefundResult, error) {
	input := map[string]any{"refund_id": r.ProviderRefundID, "provider_ref": pay.payment().ProviderRef, "idempotency_key": r.IdempotencyKey}
	return l.askRefund(ctx, org, pay, r, capability.CommercePaymentsRefundStatus, input, false)
}

// askRefund runs one refund query and records the answer. Only the first
// commerce.payments.refund request marks the refund failed on a definite
// error; any later ask leaves it pending (CodeProvider).
func (l *Ledger) askRefund(ctx context.Context, org uuid.UUID, pay paymentRow, r refundRow, key string, input map[string]any, first bool) (RefundResult, error) {
	res := RefundResult{Refund: r.refund(), Payment: pay.payment()}
	if l.cfg.Providers == nil {
		return res, failf(CodeUnavailable, "no provider querier configured")
	}
	qr, err := l.cfg.Providers.Query(ctx, capability.Query{Org: org, Capability: key, Input: input, Provider: pay.Provider})
	var raw json.RawMessage
	var one capability.Result
	if err == nil && len(qr.Results) > 0 {
		one = qr.Results[0]
		raw, err = one.Output, one.Err
	} else if err == nil {
		err = errors.New("provider returned nothing")
	}
	if err != nil {
		if one.Indeterminate() || !first {
			// May have gone through: stays pending; the reconciliation job
			// asks again (recheckRefund).
			res.Indeterminate = true
			if !first {
				return res, fail(CodeProvider, err)
			}
			return res, nil
		}
		st, uerr := l.updateRefund(ctx, org, r.ID, payments.RefundFailed, "", "", truncate(err.Error(), 500))
		if uerr != nil {
			return res, uerr
		}
		res.Refund.Status = st
		if st == payments.RefundFailed {
			return res, fail(CodeProvider, err)
		}
		return l.refundResult(ctx, org, r.ID, res)
	}
	var out struct {
		RefundID     string `json:"refund_id"`
		Status       string `json:"status"`
		Instructions string `json:"instructions"`
	}
	_ = json.Unmarshal(raw, &out)
	st := payments.RefundStatus(out.Status)
	if !st.Valid() {
		st = payments.RefundPending
	}
	st, err = l.updateRefund(ctx, org, r.ID, st, out.RefundID, out.Instructions, "")
	if err != nil {
		return res, err
	}
	res.Instructions = out.Instructions
	if st == payments.RefundSucceeded {
		if err := l.applyRefunds(ctx, org, pay.ID); err != nil {
			return res, err
		}
	}
	return l.refundResult(ctx, org, r.ID, res)
}

func (l *Ledger) refundResult(ctx context.Context, org uuid.UUID, refundID uuid.UUID, res RefundResult) (RefundResult, error) {
	var r refundRow
	if err := l.db.WithContext(ctx).Where("organization_id = ? AND id = ?", org, refundID).Take(&r).Error; err != nil {
		return res, err
	}
	res.Refund = r.refund()
	p, err := l.Payment(ctx, org, r.PaymentID.String())
	res.Payment = p
	return res, err
}

// updateRefund records a provider answer about a refund under a lock of its
// row and returns the status it ends with. The refund lifecycle is monotonic
// (payments.CanTransitionRefund): a late "pending" answer never takes back a
// refund already succeeded (its webhook or a re-query won the race), and a
// final refund keeps its status; the provider refund id is still recorded.
func (l *Ledger) updateRefund(ctx context.Context, org uuid.UUID, id uuid.UUID, st payments.RefundStatus, providerID, instructions, reason string) (payments.RefundStatus, error) {
	var final payments.RefundStatus
	err := l.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var r refundRow
		if err := forUpdate(tx).Where("organization_id = ? AND id = ?", org, id).Take(&r).Error; err != nil {
			return err
		}
		cur := payments.RefundStatus(r.Status)
		final = cur
		updates := map[string]any{"updated_at": l.now()}
		if st != cur && payments.CanTransitionRefund(cur, st) {
			final = st
			updates["status"] = string(st)
			if st == payments.RefundSucceeded {
				updates["settled_at"] = l.now()
			}
			if reason != "" {
				updates["reason"] = reason
			}
		}
		if providerID != "" && r.ProviderRefundID == "" {
			updates["provider_refund_id"] = providerID
		}
		if instructions != "" {
			updates["instructions"] = truncate(instructions, 1000)
		}
		q := tx.Model(&refundRow{}).Where("id = ?", r.ID)
		if _, ok := updates["status"]; ok {
			// Belt and braces with the row lock: only from the status read.
			q = q.Where("status = ?", string(cur))
		}
		return q.Updates(updates).Error
	})
	return final, err
}

// refundInFlight is how long a refund without a provider refund id may still
// be waiting for its first provider answer; re-queries leave it alone.
const refundInFlight = time.Minute

// DefaultRefundRepostWindow is how long after a refund was first requested
// the ledger may send the same commerce.payments.refund again (same
// idempotency key) when the provider never numbered it. Providers expire
// idempotency keys (Stripe: 24 h); past the window a re-POST could refund
// twice, so the refund goes to manual review instead.
const DefaultRefundRepostWindow = 12 * time.Hour

// ReasonRefundNeedsReview: a refund still pending past the re-POST window
// (or that the provider cannot be asked about); a human must settle it.
const ReasonRefundNeedsReview = "refund_needs_review"

// refundBackoff is the wait before the attempt-th automatic re-query
// (1 min, 2, 4 ... capped at 1 h).
func refundBackoff(attempt int) time.Duration {
	if attempt > 6 {
		return time.Hour
	}
	return time.Minute << attempt
}

func (l *Ledger) repostWindow(provider string) time.Duration {
	if w, ok := l.cfg.RefundRepostWindow[provider]; ok && w > 0 {
		return w
	}
	if l.cfg.DefaultRefundRepostWindow > 0 {
		return l.cfg.DefaultRefundRepostWindow
	}
	return DefaultRefundRepostWindow
}

// queueRefundRecheck makes the pending original-method refunds of a payment
// due for a re-query now (the reconciliation job runs it).
func queueRefundRecheck(tx *gorm.DB, org, paymentID uuid.UUID, now time.Time) error {
	return tx.Model(&refundRow{}).Where("organization_id = ? AND payment_id = ? AND status = ? AND destination = ? AND NOT needs_review",
		org, paymentID, string(payments.RefundPending), string(payments.RefundToOriginal)).
		Update("next_recheck_at", now).Error
}

// recheckRefund asks the provider about a pending original-method refund:
//
//   - numbered by the provider (provider_refund_id): only a read,
//     commerce.payments.refund.status; never a second refund request;
//   - not numbered, within the provider's re-POST window: the same
//     commerce.payments.refund again, same idempotency key;
//   - still pending past the window: needs_review (anomaly event), no more
//     automatic asks.
//
// Otherwise a refund left pending is due again after an exponential backoff.
func (l *Ledger) recheckRefund(ctx context.Context, org uuid.UUID, pay paymentRow, r refundRow) (RefundResult, error) {
	res := RefundResult{Refund: r.refund(), Payment: pay.payment()}
	if r.NeedsReview || r.Status != string(payments.RefundPending) {
		return res, nil
	}
	now := l.now()
	expired := now.Sub(r.CreatedAt) > l.repostWindow(r.Provider)
	var err error
	switch {
	case r.ProviderRefundID != "":
		res, err = l.refundStatusAtProvider(ctx, org, pay, r)
	case !expired:
		res, err = l.refundAtProvider(ctx, org, pay, r, false)
	}
	if res.Refund.Status != payments.RefundPending {
		return res, err
	}
	updates := map[string]any{"recheck_attempts": r.RecheckAttempts + 1, "next_recheck_at": now.Add(refundBackoff(r.RecheckAttempts))}
	if expired {
		updates = map[string]any{"needs_review": true, "next_recheck_at": nil}
	}
	if uerr := l.db.WithContext(ctx).Model(&refundRow{}).Where("id = ? AND status = ?", r.ID, string(payments.RefundPending)).
		Updates(updates).Error; uerr != nil {
		return res, uerr
	}
	if expired {
		res.Refund.NeedsReview = true
		p := pay.payment()
		l.raise(ctx, Anomaly{Org: org, PaymentID: p.ID, DocumentType: p.DocumentType, DocumentID: p.DocumentID,
			Provider: r.Provider, ProviderRef: p.ProviderRef, OrderRef: p.OrderRef, Reason: ReasonRefundNeedsReview,
			Stored: p.Status, Observed: p.Status, Amount: norm(r.Amount), Currency: r.Currency, Source: payments.SourceReconcile})
	}
	return res, err
}

// recheckRefunds runs recheckRefund on the pending original-method refunds
// of the org that are due (backoff elapsed, not in flight, not in review).
// It returns how many it looked at.
func (l *Ledger) recheckRefunds(ctx context.Context, org uuid.UUID, limit int) (int, error) {
	now := l.now()
	q := l.db.WithContext(ctx).Where("organization_id = ? AND status = ? AND destination = ? AND provider <> '' AND NOT needs_review"+
		" AND (next_recheck_at IS NULL OR next_recheck_at <= ?) AND (provider_refund_id <> '' OR created_at < ?)",
		org, string(payments.RefundPending), string(payments.RefundToOriginal), now, now.Add(-refundInFlight))
	if limit > 0 {
		q = q.Limit(limit)
	}
	var rows []refundRow
	if err := q.Order("created_at").Find(&rows).Error; err != nil {
		return 0, err
	}
	var errs []string
	for _, r := range rows {
		pay, err := l.paymentRow(l.db.WithContext(ctx), org, r.PaymentID.String())
		if err == nil {
			_, err = l.recheckRefund(ctx, org, pay, r)
		}
		if err != nil {
			errs = append(errs, r.ID.String()+": "+err.Error())
		}
	}
	if len(errs) > 0 {
		return len(rows), errors.New(strings.Join(errs, "; "))
	}
	return len(rows), nil
}

// applyRefunds recomputes the payment's refunded amount and status from its
// succeeded refunds and publishes the change.
func (l *Ledger) applyRefunds(ctx context.Context, org uuid.UUID, paymentID uuid.UUID) error {
	var stored paymentRow
	var changed bool
	err := l.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		row, err := l.paymentRow(forUpdate(tx), org, paymentID.String())
		if err != nil {
			return err
		}
		refunds, err := l.refundsOf(tx, org, row.ID)
		if err != nil {
			return err
		}
		p := row.payment()
		before := p.Status
		if err := payments.ApplySucceededRefunds(&p, refunds); err != nil {
			return fail(CodeRefundExceeds, err)
		}
		changed = p.Status != before || norm(row.AmountRefunded) != p.AmountRefunded
		if !changed {
			stored = row
			return nil
		}
		if err := tx.Model(&paymentRow{}).Where("id = ?", row.ID).
			Updates(map[string]any{"status": string(p.Status), "amount_refunded": p.AmountRefunded, "updated_at": l.now()}).Error; err != nil {
			return err
		}
		return tx.Where("id = ?", row.ID).Take(&stored).Error
	})
	if err != nil || !changed {
		return err
	}
	o := outcomeOf(stored.payment())
	o.Kind = payments.KindRefund
	o.Amount = norm(stored.AmountRefunded)
	l.publish(ctx, org, stored, o, payments.SourceStaff)
	return nil
}

func (l *Ledger) refundsOf(tx *gorm.DB, org uuid.UUID, paymentID uuid.UUID) ([]payments.Refund, error) {
	var rows []refundRow
	if err := tx.Where("organization_id = ? AND payment_id = ?", org, paymentID).Order("created_at").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]payments.Refund, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.refund())
	}
	return out, nil
}

// SettleRequest moves a refund forward: a wallet / cash refund paid out by
// the channel, a manual_required refund the merchant settled, the credit
// note issued for an invoiced sale.
type SettleRequest struct {
	RefundID      string
	Status        payments.RefundStatus
	CashSessionID string
	CreditNoteID  string
}

// SettleRefund applies a refund status change (refund transitions enforced)
// and/or records its credit note. Setting the status it already has only
// records the other fields.
func (l *Ledger) SettleRefund(ctx context.Context, org uuid.UUID, req SettleRequest) (RefundResult, error) {
	rid, err := uuid.Parse(strings.TrimSpace(req.RefundID))
	if err != nil {
		return RefundResult{}, failf(CodeInvalidInput, "refund_id %q is not a uuid", req.RefundID)
	}
	var paymentID uuid.UUID
	var succeeded bool
	err = l.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var r refundRow
		err := forUpdate(tx).Where("organization_id = ? AND id = ?", org, rid).Take(&r).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return failf(CodeNotFound, "refund %s not found", req.RefundID)
		}
		if err != nil {
			return err
		}
		paymentID = r.PaymentID
		updates := map[string]any{"updated_at": l.now()}
		cur := payments.RefundStatus(r.Status)
		if req.Status != "" && req.Status != cur {
			if !payments.CanTransitionRefund(cur, req.Status) {
				return failf(CodeInvalidTransition, "refund %s -> %s", cur, req.Status)
			}
			updates["status"] = string(req.Status)
			updates["needs_review"] = false
			if req.Status == payments.RefundSucceeded {
				updates["settled_at"] = l.now()
				succeeded = true
			}
		}
		if req.CashSessionID != "" {
			updates["cash_session_id"] = req.CashSessionID
		}
		if req.CreditNoteID != "" {
			updates["credit_note_id"] = req.CreditNoteID
		}
		return tx.Model(&refundRow{}).Where("id = ?", r.ID).Updates(updates).Error
	})
	if err != nil {
		return RefundResult{}, err
	}
	if succeeded {
		if err := l.applyRefunds(ctx, org, paymentID); err != nil {
			return RefundResult{}, err
		}
	}
	return l.refundResult(ctx, org, rid, RefundResult{})
}
