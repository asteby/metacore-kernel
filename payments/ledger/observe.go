package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/asteby/metacore-kernel/capability"
	"github.com/asteby/metacore-kernel/payments"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Observation is what recording one provider update did.
type Observation struct {
	Payment  payments.Payment  `json:"payment"`
	Decision payments.Decision `json:"decision"`
	Reason   string            `json:"reason,omitempty"`
	// Duplicate: the update was already recorded (same provider event).
	Duplicate bool `json:"duplicate,omitempty"`
	// Corrected: an anomaly confirmed by a second read was applied.
	Corrected bool `json:"corrected,omitempty"`
	// Orphan: the provider reported a payment the ledger does not know.
	Orphan bool `json:"orphan,omitempty"`
}

// Observe records an update a provider observed (its webhook, its own poll)
// about one of ITS payments. From a guest (caller != ""), the caller must be
// that provider and provide a commerce.payments.* capability: an addon can
// never write about another provider's payments, and a non-provider writes
// nothing. The outcome must already be a re-read of the provider's API
// (commerce.payments.confirm rules), never the webhook body.
func (l *Ledger) Observe(ctx context.Context, org uuid.UUID, caller, provider string, o payments.Outcome, source payments.Source) (Observation, error) {
	if caller != "" {
		if provider != "" && provider != caller {
			return Observation{}, failf(CodeForbidden, "addon %q cannot record payments of provider %q", caller, provider)
		}
		provider = caller
		if l.cfg.IsPaymentsProvider == nil {
			return Observation{}, failf(CodeForbidden, "host cannot verify payment providers")
		}
		ok, err := l.cfg.IsPaymentsProvider(ctx, org, caller)
		if err != nil {
			return Observation{}, fail(CodeUnavailable, err)
		}
		if !ok {
			return Observation{}, failf(CodeForbidden, "addon %q provides no commerce.payments.* capability", caller)
		}
	}
	if blank(provider) {
		return Observation{}, failf(CodeInvalidInput, "provider is required")
	}
	if source == "" {
		source = payments.SourceWebhook
	}
	return l.observe(ctx, org, provider, o, source)
}

func validOutcome(o payments.Outcome) error {
	var errs []string
	if !o.Status.Valid() {
		errs = append(errs, fmt.Sprintf("status %q is not normalized", o.Status))
	}
	if _, err := payments.ParseAmount(o.Amount); err != nil {
		errs = append(errs, "amount: "+err.Error())
	}
	if o.AmountRefunded != "" {
		if _, err := payments.ParseAmount(o.AmountRefunded); err != nil {
			errs = append(errs, "amount_refunded: "+err.Error())
		}
	}
	if !capability.ValidCurrency(o.Currency) {
		errs = append(errs, "currency must be ISO 4217")
	}
	if blank(o.ProviderRef) && blank(o.SessionID) && blank(o.OrderRef) {
		errs = append(errs, "provider_ref, session_id or order_ref is required")
	}
	if len(errs) > 0 {
		return failf(CodeInvalidInput, "%s", strings.Join(errs, "; "))
	}
	return nil
}

func (l *Ledger) observe(ctx context.Context, org uuid.UUID, provider string, o payments.Outcome, source payments.Source) (Observation, error) {
	if err := validOutcome(o); err != nil {
		return Observation{}, err
	}
	if o.Kind == "" {
		o.Kind = payments.KindPayment
	}
	now := l.now()
	var obs Observation
	var publish bool
	var anomaly *Anomaly
	var stored paymentRow
	err := l.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		row, found, err := findPayment(forUpdate(tx), org, provider, o)
		if err != nil {
			return err
		}
		eventID := o.DedupeEventID()
		ev := eventRow{
			ID: uuid.New(), OrganizationID: org, Provider: provider, EventID: eventID, Kind: string(o.Kind),
			Source: string(source), Status: string(o.Status), Outcome: jsonPtr(o), ObservedAt: now,
		}
		if !found {
			ev.Decision, ev.Reason = string(payments.Anomaly), ReasonOrphan
			dup, err := insertEvent(tx, ev)
			if err != nil || dup {
				obs.Duplicate = dup
				return err
			}
			obs = Observation{Decision: payments.Anomaly, Reason: ReasonOrphan, Orphan: true}
			anomaly = &Anomaly{Org: org, Provider: provider, ProviderRef: o.ProviderRef, OrderRef: o.OrderRef,
				Reason: ReasonOrphan, Observed: o.Status, Amount: o.Amount, Currency: o.Currency, Source: source}
			return nil
		}
		ev.PaymentID = &row.ID
		p := row.payment()
		// Status AND money: another amount, currency, provider_ref or a
		// refunded total that went down is an anomaly (payments.ObserveOutcome).
		decision := payments.ObserveOutcome(p, o)
		corrected := false
		if decision.Decision == payments.Anomaly && source == payments.SourceReconcile &&
			row.Anomaly == decision.Reason && row.AnomalyStatus == string(o.Status) && !moneyMismatch[decision.Reason] {
			// Second consistent read: the provider decides. Audited correction.
			corrected = true
		}
		ev.Decision, ev.Reason = string(decision.Decision), decision.Reason
		if corrected {
			// Its own audit row: the confirming read may derive the same
			// event id as the read that raised the anomaly.
			ev.Decision, ev.EventID = "corrected", ev.EventID+"|corrected"
		}
		dup, err := insertEvent(tx, ev)
		if err != nil {
			return err
		}
		updates := map[string]any{"last_checked_at": now, "updated_at": now}
		if dup && !corrected {
			obs = Observation{Payment: p, Decision: payments.Ignore, Reason: payments.ReasonDuplicate, Duplicate: true}
			return tx.Model(&paymentRow{}).Where("id = ?", row.ID).Updates(updates).Error
		}
		switch {
		case decision.Decision == payments.Apply || corrected:
			applyOutcome(updates, row, o, now)
			if corrected {
				updates["anomaly"], updates["anomaly_status"] = "", ""
			}
			publish = o.Status != payments.StatusPending && o.Status != payments.StatusRequiresAction
		case decision.Decision == payments.Anomaly:
			updates["anomaly"], updates["anomaly_status"] = decision.Reason, string(o.Status)
		}
		if err := tx.Model(&paymentRow{}).Where("id = ?", row.ID).Updates(updates).Error; err != nil {
			return err
		}
		if err := tx.Where("id = ?", row.ID).Take(&stored).Error; err != nil {
			return err
		}
		if (decision.Decision == payments.Apply || corrected) && stored.payment().Status.Captured() {
			if err := l.matchProviderRefunds(tx, org, stored); err != nil {
				return err
			}
		}
		obs = Observation{Payment: stored.payment(), Decision: decision.Decision, Reason: decision.Reason, Corrected: corrected}
		if decision.Decision == payments.Anomaly || corrected {
			anomaly = &Anomaly{Org: org, PaymentID: row.ID.String(), DocumentType: row.DocumentType, DocumentID: row.DocumentID,
				Provider: provider, ProviderRef: firstNonBlank(o.ProviderRef, p.ProviderRef), OrderRef: row.OrderRef,
				Reason: decision.Reason, Stored: p.Status, Observed: o.Status, Amount: o.Amount, Currency: o.Currency,
				Source: source, Resolved: corrected}
		}
		if publish && o.Status.Counts() {
			// A paid document's hold is moot.
			return l.settleDocument(tx, org, row.DocumentType, row.DocumentID, normPtr(row.DocumentTotal))
		}
		return nil
	})
	if err != nil {
		return obs, err
	}
	if anomaly != nil {
		l.raise(ctx, *anomaly)
	}
	if publish {
		l.publish(ctx, org, stored, o, source)
	}
	return obs, nil
}

// applyOutcome copies what the provider says onto the payment.
func applyOutcome(updates map[string]any, row paymentRow, o payments.Outcome, now time.Time) {
	updates["status"] = string(o.Status)
	if o.ProviderRef != "" && (row.ProviderRef == nil || *row.ProviderRef == "") {
		updates["provider_ref"] = o.ProviderRef
	}
	if o.SessionID != "" && row.SessionID == "" {
		updates["session_id"] = o.SessionID
	}
	if o.DeviceID != "" {
		updates["device_id"] = o.DeviceID
	}
	switch {
	case o.Status == payments.StatusRefunded:
		updates["amount_refunded"] = row.Amount
	case o.AmountRefunded != "":
		updates["amount_refunded"] = o.AmountRefunded
	case o.Kind == payments.KindRefund && o.Status == payments.StatusPartiallyRefunded:
		// No total given: the update amount is this refund; add it.
		updates["amount_refunded"] = payments.MustAmount(norm(row.AmountRefunded)).Add(payments.MustAmount(o.Amount)).String()
	}
	if o.Fee != "" {
		updates["fee"] = o.Fee
	}
	if o.NetAmount != "" {
		updates["net"] = o.NetAmount
	}
	if o.Method != "" {
		updates["method"] = o.Method
	}
	if o.Installments > 0 {
		updates["installments"] = o.Installments
	}
	if o.Card != nil {
		updates["card"] = jsonPtr(o.Card)
	}
	if o.Cash != nil {
		updates["cash"] = jsonPtr(o.Cash)
	}
	if o.TestMode {
		updates["test_mode"] = true
	}
	if d := firstNonBlank(o.StatusDetail, o.ProviderStatus); d != "" {
		updates["status_detail"] = truncate(d, 500)
	}
	if o.Status.Captured() && row.PaidAt == nil {
		paid := now
		if t, err := time.Parse(time.RFC3339, o.PaidAt); err == nil {
			paid = t.UTC()
		}
		updates["paid_at"] = paid
	}
}

// findPayment resolves the payment an outcome is about: by provider_ref, then
// session id, then the newest open payment of the order reference.
func findPayment(tx *gorm.DB, org uuid.UUID, provider string, o payments.Outcome) (paymentRow, bool, error) {
	var row paymentRow
	try := func(q *gorm.DB) (bool, error) {
		err := q.Take(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil
		}
		return err == nil, err
	}
	base := func() *gorm.DB { return tx.Where("organization_id = ? AND provider = ?", org, provider) }
	if o.ProviderRef != "" {
		if ok, err := try(base().Where("provider_ref = ?", o.ProviderRef)); ok || err != nil {
			return row, ok, err
		}
	}
	if o.SessionID != "" {
		if ok, err := try(base().Where("session_id = ?", o.SessionID).Order("created_at DESC")); ok || err != nil {
			return row, ok, err
		}
	}
	if o.OrderRef != "" {
		if ok, err := try(base().Where("order_ref = ? AND (provider_ref IS NULL OR provider_ref = '')", o.OrderRef).Order("created_at DESC")); ok || err != nil {
			return row, ok, err
		}
	}
	return row, false, nil
}

// insertEvent records an event; dup=true when (org, provider, event_id) was
// already recorded.
func insertEvent(tx *gorm.DB, ev eventRow) (dup bool, err error) {
	var n int64
	if err := tx.Model(&eventRow{}).Where("organization_id = ? AND provider = ? AND event_id = ?", ev.OrganizationID, ev.Provider, ev.EventID).
		Count(&n).Error; err != nil {
		return false, err
	}
	if n > 0 {
		return true, nil
	}
	return false, tx.Create(&ev).Error
}

// matchProviderRefunds marks pending original-method refunds succeeded, oldest
// first, as far as the refunded total the provider reports covers them (the
// provider's refund webhook usually arrives after commerce.payments.refund
// answered pending).
func (l *Ledger) matchProviderRefunds(tx *gorm.DB, org uuid.UUID, row paymentRow) error {
	reported := payments.MustAmount(norm(row.AmountRefunded))
	if reported.Sign() <= 0 {
		return nil
	}
	var rows []refundRow
	if err := tx.Where("organization_id = ? AND payment_id = ? AND status IN ?", org, row.ID,
		[]string{string(payments.RefundSucceeded), string(payments.RefundPending)}).Order("created_at").Find(&rows).Error; err != nil {
		return err
	}
	covered := payments.Amount{}
	for _, r := range rows {
		if r.Status == string(payments.RefundSucceeded) {
			covered = covered.Add(payments.MustAmount(norm(r.Amount)))
		}
	}
	for _, r := range rows {
		if r.Status != string(payments.RefundPending) || r.Destination != string(payments.RefundToOriginal) {
			continue
		}
		next := covered.Add(payments.MustAmount(norm(r.Amount)))
		if next.Cmp(reported) > 0 {
			break
		}
		covered = next
		if err := tx.Model(&refundRow{}).Where("id = ?", r.ID).
			Updates(map[string]any{"status": string(payments.RefundSucceeded), "settled_at": l.now(), "updated_at": l.now()}).Error; err != nil {
			return err
		}
	}
	return nil
}

func outcomeOf(p payments.Payment) payments.Outcome {
	return payments.Outcome{
		OrderRef: p.OrderRef, Status: p.Status, Amount: p.Amount, Currency: p.Currency, ProviderRef: p.ProviderRef,
		Kind: payments.KindPayment, Method: p.Method, Installments: p.Installments, AmountRefunded: p.AmountRefunded,
		Fee: p.Fee, NetAmount: p.Net, TestMode: p.TestMode, Channel: p.Channel, SessionID: p.SessionID,
		DeviceID: p.DeviceID, Card: p.Card, Cash: p.Cash, Seller: p.Seller,
	}
}

// publish announces an applied change on commerce.payments.confirmed under
// the provider's key, with the canonical payment id and document.
func (l *Ledger) publish(ctx context.Context, org uuid.UUID, row paymentRow, o payments.Outcome, source payments.Source) {
	if l.cfg.Publish == nil {
		return
	}
	p := row.payment()
	result := outcomeOf(p)
	result.Kind, result.EventID, result.OccurredAt, result.PaidAt = o.Kind, o.DedupeEventID(), o.OccurredAt, o.PaidAt
	if o.Kind == payments.KindRefund {
		result.Amount = o.Amount
	}
	payload := map[string]any{
		"provider":        p.Provider,
		"result":          result,
		"organization_id": org.String(),
		"occurred_at":     firstNonBlank(o.OccurredAt, l.now().Format(time.RFC3339)),
		"source":          string(source),
		"payment_id":      p.ID,
		"document_type":   p.DocumentType,
		"document_id":     p.DocumentID,
	}
	if err := l.cfg.Publish(ctx, p.Provider, capability.CommercePaymentsConfirmed, org, payload); err != nil {
		log.Printf("payments ledger: publish %s payment=%s: %v", capability.CommercePaymentsConfirmed, p.ID, err)
	}
}

func (l *Ledger) raise(ctx context.Context, a Anomaly) {
	log.Printf("payments ledger: anomaly org=%s payment=%s provider=%s reason=%s stored=%s observed=%s resolved=%v",
		a.Org, a.PaymentID, a.Provider, a.Reason, a.Stored, a.Observed, a.Resolved)
	if l.cfg.Anomalies != nil {
		if err := l.cfg.Anomalies.PaymentAnomaly(ctx, a); err != nil {
			log.Printf("payments ledger: anomaly sink: %v", err)
		}
	}
	if l.cfg.Publish != nil {
		if err := l.cfg.Publish(ctx, "kernel", AnomalyEvent, a.Org, a); err != nil {
			log.Printf("payments ledger: publish %s: %v", AnomalyEvent, err)
		}
	}
}

// raw helper for host outputs.
func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
