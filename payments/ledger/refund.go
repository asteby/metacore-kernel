package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

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
	var created *refundRow
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
			CreditNoteRequired: req.CreditNoteRequired, AdjustsTotal: req.AdjustsTotal, CreatedBy: req.Caller, CreatedAt: now, UpdatedAt: now,
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
	if err != nil || created == nil || req.Destination != payments.RefundToOriginal {
		return res, err
	}
	return l.refundAtProvider(ctx, org, req, pay, *created)
}

func (l *Ledger) refundAtProvider(ctx context.Context, org uuid.UUID, req RefundRequest, pay paymentRow, r refundRow) (RefundResult, error) {
	p := pay.payment()
	res := RefundResult{Refund: r.refund(), Payment: p}
	if l.cfg.Providers == nil {
		return res, failf(CodeUnavailable, "no provider querier configured")
	}
	input := map[string]any{
		"provider_ref": p.ProviderRef, "amount": r.Amount, "currency": r.Currency, "order_ref": p.OrderRef,
		"reason": r.Reason, "idempotency_key": r.IdempotencyKey,
	}
	if p.Seller != nil {
		input["seller"] = p.Seller
		input["refund_application_fee"] = req.RefundApplicationFee
	}
	qr, err := l.cfg.Providers.Query(ctx, capability.Query{Org: org, Capability: capability.CommercePaymentsRefund, Input: input, Provider: p.Provider})
	var raw json.RawMessage
	var one capability.Result
	if err == nil && len(qr.Results) > 0 {
		one = qr.Results[0]
		raw, err = one.Output, one.Err
	} else if err == nil {
		err = errors.New("provider returned nothing")
	}
	if err != nil {
		if one.Indeterminate() {
			// May have gone through: stays pending; the payment status
			// (amount_refunded) and a retry with the same key resolve it.
			res.Indeterminate = true
			return res, nil
		}
		if uerr := l.updateRefund(ctx, org, r.ID, payments.RefundFailed, "", "", truncate(err.Error(), 500)); uerr != nil {
			return res, uerr
		}
		res.Refund.Status = payments.RefundFailed
		return res, fail(CodeProvider, err)
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
	if err := l.updateRefund(ctx, org, r.ID, st, out.RefundID, out.Instructions, ""); err != nil {
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

func (l *Ledger) updateRefund(ctx context.Context, org uuid.UUID, id uuid.UUID, st payments.RefundStatus, providerID, instructions, reason string) error {
	updates := map[string]any{"status": string(st), "updated_at": l.now()}
	if providerID != "" {
		updates["provider_refund_id"] = providerID
	}
	if instructions != "" {
		updates["instructions"] = truncate(instructions, 1000)
	}
	if reason != "" {
		updates["reason"] = reason
	}
	if st == payments.RefundSucceeded {
		updates["settled_at"] = l.now()
	}
	return l.db.WithContext(ctx).Model(&refundRow{}).Where("organization_id = ? AND id = ?", org, id).Updates(updates).Error
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
