package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"time"

	"github.com/asteby/metacore-kernel/capability"
	"github.com/asteby/metacore-kernel/payments"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Reconciler is the reconciliation job (payments standard, phase K3). Per
// organization, each run:
//
//  1. re-reads open payments not checked for MinAge (pending, requires_action,
//     at_device, authorized, cancelling): the server-side fallback of the
//     channel's polling, so a payment resolves even if the screen closed;
//  2. for open payments past expires_at (or older than StuckAfter) that the
//     provider still reports open: asks the provider to cancel, re-reads,
//     and raises "stuck" when it stays open;
//  3. re-reads payments flagged with an anomaly: a second consistent read
//     applies the provider's status as an audited correction;
//  4. lists every provider's payments of the last Window
//     (commerce.payments.reconcile) and records them: unknown ones are
//     orphans (money without a document), changed ones are applied;
//  5. re-queries original-method refunds still pending (timeout, lost
//     webhook) with their own idempotency key, so none stays reserved
//     forever.
//
// Runs are leased per organization (commerce_payment_reconcile_leases), so
// several replicas never reconcile the same org at once. A long run renews
// its lease as it goes and stops if another replica took it over.
type Reconciler struct {
	Ledger *Ledger
	// MinAge between two re-reads of the same open payment (default 30 s).
	MinAge time.Duration
	// StuckAfter: an open payment without expiry older than this is treated
	// as expired (default 30 min).
	StuckAfter time.Duration
	// Window listed by commerce.payments.reconcile (default 2 h; runs
	// overlap on purpose, the event dedupe makes it idempotent).
	Window time.Duration
	// Batch caps re-reads per step and run (default 200).
	Batch int
	// Lease is how long a run owns the org (default 5 min).
	Lease time.Duration
	// Owner identifies this replica in leases.
	Owner string
}

// Report counts what one run did.
type Report struct {
	Org       uuid.UUID `json:"organization_id"`
	Refreshed int       `json:"refreshed"`
	Applied   int       `json:"applied"`
	Cancelled int       `json:"cancel_requested"`
	Stuck     int       `json:"stuck"`
	Anomalies int       `json:"anomalies"`
	Corrected int       `json:"corrected"`
	Listed    int       `json:"listed"`
	Orphans   int       `json:"orphans"`
	Refunds   int       `json:"refunds_rechecked"`
	Errors    []string  `json:"errors,omitempty"`
	Skipped   bool      `json:"skipped,omitempty"`
}

func (r *Reconciler) defaults() {
	if r.MinAge <= 0 {
		r.MinAge = 30 * time.Second
	}
	if r.StuckAfter <= 0 {
		r.StuckAfter = 30 * time.Minute
	}
	if r.Window <= 0 {
		r.Window = 2 * time.Hour
	}
	if r.Batch <= 0 {
		r.Batch = 200
	}
	if r.Lease <= 0 {
		r.Lease = 5 * time.Minute
	}
	if r.Owner == "" {
		r.Owner = uuid.NewString()
	}
}

var openStatuses = []string{
	string(payments.StatusPending), string(payments.StatusRequiresAction), string(payments.StatusAtDevice),
	string(payments.StatusAuthorized), string(payments.StatusCancelling),
}

// RunOrg reconciles one organization (skipped when another replica holds
// its lease).
func (r *Reconciler) RunOrg(ctx context.Context, org uuid.UUID) (Report, error) {
	r.defaults()
	l := r.Ledger
	rep := Report{Org: org}
	ok, err := r.claim(ctx, org)
	if err != nil {
		return rep, err
	}
	if !ok {
		rep.Skipped = true
		return rep, nil
	}
	defer r.release(ctx, org, &rep)
	db := l.db.WithContext(ctx)
	now := l.now()
	renewed, lost := now, false
	// alive renews the lease once half of it went by; false once lost.
	alive := func() bool {
		if lost {
			return false
		}
		if t := l.now(); t.Sub(renewed) >= r.Lease/2 {
			ok, err := r.renew(ctx, org)
			if err != nil || !ok {
				lost = true
				rep.Errors = append(rep.Errors, "reconcile lease lost: another replica took over")
				return false
			}
			renewed = t
		}
		return true
	}
	note := func(err error) {
		if err != nil {
			rep.Errors = append(rep.Errors, err.Error())
		}
	}
	count := func(o Observation) {
		rep.Refreshed++
		switch {
		case o.Corrected:
			rep.Corrected++
		case o.Decision == payments.Apply:
			rep.Applied++
		case o.Decision == payments.Anomaly:
			rep.Anomalies++
		}
	}

	// 1. open payments due for a re-read.
	var open []paymentRow
	note(db.Where("organization_id = ? AND status IN ? AND provider <> ? AND (last_checked_at IS NULL OR last_checked_at < ?)",
		org, openStatuses, payments.ProviderCash, now.Add(-r.MinAge)).
		Order("created_at").Limit(r.Batch).Find(&open).Error)
	for _, p := range open {
		if !alive() {
			return rep, nil
		}
		o, err := l.Refresh(ctx, org, p.ID.String(), payments.SourceReconcile)
		note(err)
		if err == nil {
			count(o)
		}
	}

	// 2. expired / stuck open payments.
	var stale []paymentRow
	note(db.Where("organization_id = ? AND status IN ? AND provider <> ? AND ((expires_at IS NOT NULL AND expires_at < ?) OR (expires_at IS NULL AND created_at < ?))",
		org, openStatuses, payments.ProviderCash, now, now.Add(-r.StuckAfter)).
		Order("created_at").Limit(r.Batch).Find(&stale).Error)
	for _, p := range stale {
		if !alive() {
			return rep, nil
		}
		o, err := l.Cancel(ctx, org, p.ID.String(), "reconcile-expire:"+p.ID.String(), "expired")
		rep.Cancelled++
		if err != nil && !errors.Is(err, context.Canceled) {
			note(err)
		}
		if o.Payment.Status.Open() {
			rep.Stuck++
			l.raise(ctx, Anomaly{Org: org, PaymentID: p.ID.String(), DocumentType: p.DocumentType, DocumentID: p.DocumentID,
				Provider: p.Provider, ProviderRef: deref(p.ProviderRef), OrderRef: p.OrderRef, Reason: ReasonStuck,
				Stored: payments.Status(p.Status), Observed: o.Payment.Status, Amount: norm(p.Amount), Currency: p.Currency,
				Source: payments.SourceReconcile})
		}
	}

	// 3. anomalies: confirm with a second read.
	var flagged []paymentRow
	note(db.Where("organization_id = ? AND anomaly <> '' AND provider <> ?", org, payments.ProviderCash).
		Order("updated_at").Limit(r.Batch).Find(&flagged).Error)
	for _, p := range flagged {
		if !alive() {
			return rep, nil
		}
		o, err := l.Refresh(ctx, org, p.ID.String(), payments.SourceReconcile)
		note(err)
		if err == nil {
			count(o)
		}
	}

	// 4. provider listing of the window.
	var providers []string
	note(db.Model(&paymentRow{}).Where("organization_id = ? AND provider <> ?", org, payments.ProviderCash).
		Distinct("provider").Pluck("provider", &providers).Error)
	for _, prov := range providers {
		if !alive() {
			return rep, nil
		}
		r.listProvider(ctx, org, prov, now, &rep, alive)
	}

	// 5. refunds still pending at the provider.
	if alive() {
		n, err := l.recheckRefunds(ctx, org, uuid.Nil, r.MinAge, r.Batch)
		rep.Refunds += n
		note(err)
	}
	return rep, nil
}

func (r *Reconciler) listProvider(ctx context.Context, org uuid.UUID, provider string, now time.Time, rep *Report, alive func() bool) {
	l := r.Ledger
	if l.cfg.Providers == nil {
		return
	}
	cursor := ""
	for page := 0; page < 50 && alive(); page++ {
		in := map[string]any{"from": now.Add(-r.Window).Format(time.RFC3339), "to": now.Format(time.RFC3339), "limit": r.Batch}
		if cursor != "" {
			in["cursor"] = cursor
		}
		qr, err := l.cfg.Providers.Query(ctx, capability.Query{Org: org, Capability: capability.CommercePaymentsReconcile, Input: in, Provider: provider})
		if err != nil {
			// A provider without commerce.payments.reconcile is simply not
			// listed (no_provider): steps 1-3 still cover its payments.
			if !errors.Is(err, capability.ErrNoProvider) {
				rep.Errors = append(rep.Errors, provider+": "+err.Error())
			}
			return
		}
		raw, err := qr.One()
		if err != nil {
			rep.Errors = append(rep.Errors, provider+": "+err.Error())
			return
		}
		var pageOut struct {
			Payments   []payments.Outcome `json:"payments"`
			NextCursor string             `json:"next_cursor"`
		}
		if err := json.Unmarshal(raw, &pageOut); err != nil {
			rep.Errors = append(rep.Errors, provider+": "+err.Error())
			return
		}
		for _, o := range pageOut.Payments {
			rep.Listed++
			obs, err := l.observe(ctx, org, provider, o, payments.SourceReconcile)
			if err != nil {
				rep.Errors = append(rep.Errors, provider+": "+err.Error())
				continue
			}
			switch {
			case obs.Orphan:
				rep.Orphans++
			case obs.Corrected:
				rep.Corrected++
			case obs.Decision == payments.Apply && !obs.Duplicate:
				rep.Applied++
			case obs.Decision == payments.Anomaly && !obs.Duplicate:
				rep.Anomalies++
			}
		}
		if pageOut.NextCursor == "" || pageOut.NextCursor == cursor {
			return
		}
		cursor = pageOut.NextCursor
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

type leaseRow struct {
	OrganizationID uuid.UUID  `gorm:"column:organization_id;primaryKey"`
	LockedBy       string     `gorm:"column:locked_by"`
	LockedUntil    *time.Time `gorm:"column:locked_until"`
	LastRunAt      *time.Time `gorm:"column:last_run_at"`
	LastReport     *string    `gorm:"column:last_report"`
}

func (leaseRow) TableName() string { return TableLeases }

func (r *Reconciler) claim(ctx context.Context, org uuid.UUID) (bool, error) {
	db := r.Ledger.db.WithContext(ctx)
	now := r.Ledger.now()
	if err := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&leaseRow{OrganizationID: org}).Error; err != nil {
		return false, err
	}
	until := now.Add(r.Lease)
	res := db.Model(&leaseRow{}).
		Where("organization_id = ? AND (locked_until IS NULL OR locked_until < ? OR locked_by = ?)", org, now, r.Owner).
		Updates(map[string]any{"locked_by": r.Owner, "locked_until": until})
	return res.RowsAffected == 1, res.Error
}

// renew extends the lease this replica holds; false when it is not ours any
// more.
func (r *Reconciler) renew(ctx context.Context, org uuid.UUID) (bool, error) {
	res := r.Ledger.db.WithContext(ctx).Model(&leaseRow{}).Where("organization_id = ? AND locked_by = ?", org, r.Owner).
		Update("locked_until", r.Ledger.now().Add(r.Lease))
	return res.RowsAffected == 1, res.Error
}

func (r *Reconciler) release(ctx context.Context, org uuid.UUID, rep *Report) {
	now := r.Ledger.now()
	err := r.Ledger.db.WithContext(ctx).Model(&leaseRow{}).Where("organization_id = ? AND locked_by = ?", org, r.Owner).
		Updates(map[string]any{"locked_until": nil, "last_run_at": now, "last_report": jsonPtr(rep)}).Error
	if err != nil {
		log.Printf("payments reconcile: release lease org=%s: %v", org, err)
	}
}

// Orgs lists the organizations to reconcile: those with open or flagged
// payments, or any payment in the last Window (cheap: index scans).
func (r *Reconciler) Orgs(ctx context.Context) ([]uuid.UUID, error) {
	r.defaults()
	var ids []uuid.UUID
	err := r.Ledger.db.WithContext(ctx).Model(&paymentRow{}).
		Where("status IN ? OR anomaly <> '' OR updated_at > ?", openStatuses, r.Ledger.now().Add(-r.Window)).
		Distinct("organization_id").Pluck("organization_id", &ids).Error
	return ids, err
}

// Run reconciles every org returned by Orgs each interval until ctx ends.
// Errors are logged; the job never stops on them.
func (r *Reconciler) Run(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = time.Minute
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		r.RunOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// RunOnce reconciles every org once.
func (r *Reconciler) RunOnce(ctx context.Context) []Report {
	orgs, err := r.Orgs(ctx)
	if err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			log.Printf("payments reconcile: list orgs: %v", err)
		}
		return nil
	}
	out := make([]Report, 0, len(orgs))
	for _, org := range orgs {
		if ctx.Err() != nil {
			break
		}
		rep, err := r.RunOrg(ctx, org)
		if err != nil {
			log.Printf("payments reconcile: org=%s: %v", org, err)
			continue
		}
		if len(rep.Errors) > 0 || rep.Anomalies+rep.Orphans+rep.Stuck > 0 {
			log.Printf("payments reconcile: org=%s refreshed=%d applied=%d anomalies=%d orphans=%d stuck=%d corrected=%d errors=%d",
				org, rep.Refreshed, rep.Applied, rep.Anomalies, rep.Orphans, rep.Stuck, rep.Corrected, len(rep.Errors))
		}
		out = append(out, rep)
	}
	return out
}
