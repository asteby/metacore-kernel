package inbox

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/payments/incidents"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

var org = uuid.MustParse("11111111-1111-1111-1111-111111111111")

func mpReq(body string) Request {
	h := http.Header{}
	h.Set("X-Signature", "ts=1760997480123,v1="+hash64)
	h.Set("X-Request-Id", "req-1")
	h.Set("Authorization", "Bearer secret")
	return Request{
		Org: org, Provider: "mercadopago", Route: "/mercadopago/notifications", Method: "POST",
		Query: url.Values{"data.id": {"123"}, "type": {"payment"}}, Headers: h,
		Body: []byte(body), SourceIP: "203.0.113.9:4433",
	}
}

const mpBody = `{"action":"payment.updated","data":{"id":"123"},"type":"payment"}`

func TestRecordStoresBeforeDeciding(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		st, _ := newStores(t, db, Config{})
		n, err := st.Record(t.Context(), mpReq(mpBody))
		if err != nil {
			t.Fatal(err)
		}
		if n.Estado != EstadoRecibida || n.Auth != AuthPendiente || n.Intentos != 0 {
			t.Fatalf("unexpected row %+v", n)
		}
		if n.SourceIP != "203.0.113.9" || n.Method != "POST" || n.BodySHA256 == "" || n.DedupeKey == "" {
			t.Fatalf("input columns wrong: %+v", n)
		}
		raw, trunc, err := st.Raw(t.Context(), org, n.ID)
		if err != nil || string(raw) != mpBody || trunc {
			t.Fatalf("raw body not kept verbatim: %q %v %v", raw, trunc, err)
		}
		// Nothing secret in the journal row.
		js, _ := n.Headers.MarshalJSON()
		for _, bad := range []string{hash64, "Bearer", "secret"} {
			if strings.Contains(string(js), bad) {
				t.Errorf("%q leaked in headers %s", bad, js)
			}
		}
		// Other orgs cannot read it.
		if _, err := st.Get(t.Context(), uuid.New(), n.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("cross-org Get = %v", err)
		}
	})
}

func TestRecordTruncatesButHashesWholeBody(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		st, _ := newStores(t, db, Config{})
		big := `{"x":"` + strings.Repeat("a", MaxBodyBytes+10) + `"}`
		n, err := st.Record(t.Context(), mpReq(big))
		if err != nil {
			t.Fatal(err)
		}
		raw, trunc, _ := st.Raw(t.Context(), org, n.ID)
		if !trunc || len(raw) != MaxBodyBytes || !n.BodyTruncated {
			t.Fatalf("truncation: %d %v", len(raw), trunc)
		}
		again, _ := st.Record(t.Context(), mpReq(big))
		if again.BodySHA256 != n.BodySHA256 {
			t.Error("hash must cover the full body")
		}
	})
}

// T1 + T5: processed once, the twin is recorded as duplicate.
func TestDuplicateWindow(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		st, _ := newStores(t, db, Config{})
		first, _ := st.Record(t.Context(), mpReq(mpBody))
		if _, err := st.Resolve(t.Context(), org, first.ID, Result{Auth: AuthFirmaOK, Estado: EstadoProcesada, ResourceKind: "payment", ResourceID: "123"}); err != nil {
			t.Fatal(err)
		}
		twin, err := st.Record(t.Context(), mpReq(mpBody))
		if err != nil {
			t.Fatal(err)
		}
		if twin.Estado != EstadoDuplicada || twin.ID == first.ID {
			t.Fatalf("twin should be a stored duplicate: %+v", twin)
		}
		other, _ := st.Record(t.Context(), mpReq(strings.Replace(mpBody, "123", "124", 1)))
		if other.Estado != EstadoRecibida {
			t.Errorf("different body must not be duplicate: %s", other.Estado)
		}
		_, total, _ := st.List(t.Context(), Filter{Org: org})
		if total != 3 {
			t.Errorf("journal must keep all 3 rows, got %d", total)
		}
	})
}

func TestFailedTwinDoesNotBlockProviderRetry(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		st, _ := newStores(t, db, Config{})
		first, _ := st.Record(t.Context(), mpReq(mpBody))
		if _, err := st.Resolve(t.Context(), org, first.ID, Result{Estado: EstadoReintentar, Motivo: "api down"}); err != nil {
			t.Fatal(err)
		}
		retry, _ := st.Record(t.Context(), mpReq(mpBody))
		if retry.Estado != EstadoRecibida {
			t.Fatalf("provider retry of a failed delivery must be processed, got %s", retry.Estado)
		}
	})
}

func TestConcurrentTwinsOneOriginal(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		st, _ := newStores(t, db, Config{})
		var wg sync.WaitGroup
		var originals atomic.Int32
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				n, err := st.Record(context.Background(), mpReq(mpBody))
				if err == nil && n.Estado == EstadoRecibida {
					originals.Add(1)
				}
			}()
		}
		wg.Wait()
		if db.Dialector.Name() == "postgres" && originals.Load() != 1 {
			t.Fatalf("exactly one original expected on Postgres, got %d", originals.Load())
		}
		if originals.Load() < 1 {
			t.Fatal("at least one original expected")
		}
	})
}

// T18: when the journal cannot be written, Record fails (host answers 503).
func TestRecordFailsWhenStoreUnavailable(t *testing.T) {
	t.Run("SQLite", func(t *testing.T) {
		db := sqliteDB(t)
		st, _ := newStores(t, db, Config{})
		sqlDB, _ := db.DB()
		_ = sqlDB.Close()
		if _, err := st.Record(t.Context(), mpReq(mpBody)); err == nil {
			t.Fatal("Record must fail on a closed database")
		}
	})
	t.Run("Postgres", func(t *testing.T) {
		db, open := postgresDBWith(t)
		newStores(t, db, Config{})
		ro := open("default_transaction_read_only=on")
		st := NewStore(ro, Config{})
		if _, err := st.Record(t.Context(), mpReq(mpBody)); err == nil {
			t.Fatal("Record must fail on a read-only database")
		}
		if r := StoreFailureReply(); r.Status != http.StatusServiceUnavailable || r.Headers["Retry-After"] == "" {
			t.Errorf("store failure reply = %+v", r)
		}
	})
}

func TestAppendOnlyInputColumnsPostgres(t *testing.T) {
	db := postgresDB(t)
	st, _ := newStores(t, db, Config{})
	n, _ := st.Record(t.Context(), mpReq(mpBody))
	for _, set := range []string{"body_sha256 = 'x'", "received_at = now() - interval '1 day'", "provider = 'other'", "query = '{}'",
		"body_redacted = '{}'", "dedupe_key = 'x'", "source_ip = '1.1.1.1'", "headers = '{}'", "body_raw = NULL"} {
		if err := db.Exec("UPDATE "+TableNotifications+" SET "+set+" WHERE id = ?", n.ID).Error; err == nil {
			t.Errorf("UPDATE SET %s must be rejected", set)
		}
	}
	// Result columns stay writable.
	if err := db.Exec("UPDATE "+TableNotifications+" SET motivo = 'ok', estado = 'procesada' WHERE id = ?", n.ID).Error; err != nil {
		t.Errorf("result columns must be writable: %v", err)
	}
	// Migrate twice is a no-op.
	if err := Migrate(t.Context(), db); err != nil {
		t.Errorf("second Migrate: %v", err)
	}
}

func TestMigrateIdempotentAndConcurrent(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		var wg sync.WaitGroup
		errs := make(chan error, 6)
		for i := 0; i < 6; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs <- Migrate(context.Background(), db)
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil && db.Dialector.Name() == "postgres" {
				t.Errorf("concurrent Migrate: %v", err)
			}
		}
		if err := Migrate(t.Context(), db); err != nil {
			t.Fatal(err)
		}
	})
}

// T2 (kernel side): invalid signature, reconciled by API -> incident opened,
// grouped on the second one, closed when the signature matches again.
func TestInvalidSignatureIncidentAndGrouping(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		st, inc := newStores(t, db, Config{})
		started := time.Now()
		n1, _ := st.Record(t.Context(), mpReq(mpBody))
		r1, err := st.Resolve(t.Context(), org, n1.ID, Result{Auth: AuthFirmaInvalida, AuthReason: "signature_mismatch",
			Estado: EstadoReintentar, ResourceKind: "payment", ResourceID: "123", Motivo: "reconciliando"})
		if err != nil {
			t.Fatal(err)
		}
		if el := time.Since(started); el > 5*time.Second {
			t.Fatalf("P0: incident must exist in < 5 s, took %s", el)
		}
		if r1.IncidentID == nil {
			t.Fatal("notification must link its incident")
		}
		open, total, _ := inc.List(t.Context(), incidents.Filter{Org: org, OpenOnly: true})
		if total != 2 { // signature_invalid + notification_unresolved
			t.Fatalf("want 2 open incidents, got %d: %+v", total, open)
		}
		var sig incidents.Incident
		for _, i := range open {
			if i.Kind == incidents.KindSignatureInvalid {
				sig = i
			}
		}
		if sig.Severity != incidents.SeverityHigh || sig.Fingerprint != "payments.signature_invalid:mercadopago" || sig.Occurrences != 1 {
			t.Fatalf("signature incident wrong: %+v", sig)
		}

		// A second invalid notification groups (occurrences=2), no new row.
		n2, _ := st.Record(t.Context(), mpReq(strings.Replace(mpBody, "123", "999", 1)))
		if _, err := st.Resolve(t.Context(), org, n2.ID, Result{Auth: AuthFirmaInvalida, Estado: EstadoReconciliadaAPI, ResourceKind: "payment", ResourceID: "999"}); err != nil {
			t.Fatal(err)
		}
		sig2, _ := inc.Get(t.Context(), org, sig.ID)
		if sig2.Occurrences != 2 || len(sig2.EvidenceIDs) != 2 {
			t.Fatalf("grouping: occurrences=%d ids=%d", sig2.Occurrences, len(sig2.EvidenceIDs))
		}

		// The retry of #1 succeeds: its unresolved incident closes by itself,
		// the signature one stays open (the secret is still wrong).
		if _, err := st.Resolve(t.Context(), org, n1.ID, Result{Auth: AuthFirmaInvalida, Estado: EstadoReconciliadaAPI}); err != nil {
			t.Fatal(err)
		}
		open, _, _ = inc.List(t.Context(), incidents.Filter{Org: org, OpenOnly: true})
		if len(open) != 1 || open[0].Kind != incidents.KindSignatureInvalid {
			t.Fatalf("after reconcile only the signature incident remains: %+v", open)
		}

		// A good signature closes it.
		n3, _ := st.Record(t.Context(), mpReq(strings.Replace(mpBody, "123", "555", 1)))
		if _, err := st.Resolve(t.Context(), org, n3.ID, Result{Auth: AuthFirmaOK, Estado: EstadoProcesada, ResourceKind: "payment", ResourceID: "555"}); err != nil {
			t.Fatal(err)
		}
		open, _, _ = inc.List(t.Context(), incidents.Filter{Org: org, OpenOnly: true})
		if len(open) != 0 {
			t.Fatalf("firma_ok must close the signature incident: %+v", open)
		}
	})
}

func TestRotationMatchedPreviousIsLowIncident(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		st, inc := newStores(t, db, Config{})
		n, _ := st.Record(t.Context(), mpReq(mpBody))
		if _, err := st.Resolve(t.Context(), org, n.ID, Result{Auth: AuthFirmaOK, AuthReason: AuthReasonMatchedPrevious, Estado: EstadoProcesada}); err != nil {
			t.Fatal(err)
		}
		open, _, _ := inc.List(t.Context(), incidents.Filter{Org: org, OpenOnly: true})
		if len(open) != 1 || open[0].Severity != incidents.SeverityLow {
			t.Fatalf("T20: want one low incident, got %+v", open)
		}
	})
}

// T3 + T17: retries with backoff, dead letter at the end, always an incident.
func TestRetryScheduleAndDeadLetter(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		clock := time.Date(2026, 10, 20, 22, 0, 0, 0, time.UTC)
		var retried []time.Time
		st, inc := newStores(t, db, Config{
			Backoff: []time.Duration{10 * time.Second, 30 * time.Second}, Jitter: -1,
			Now:     func() time.Time { return clock },
			OnRetry: func(_ context.Context, n Notification) { retried = append(retried, *n.ProximoIntentoAt) },
		})
		n, _ := st.Record(t.Context(), mpReq(mpBody))
		r, _ := st.Resolve(t.Context(), org, n.ID, Result{Auth: AuthFirmaOK, Estado: EstadoReintentar, Motivo: "api 503", ResourceKind: "payment", ResourceID: "123"})
		if r.Estado != EstadoReintentar || r.Intentos != 1 || !r.ProximoIntentoAt.Equal(clock.Add(10*time.Second)) {
			t.Fatalf("first retry: %+v", r)
		}
		if due, _ := st.Due(t.Context(), 10); len(due) != 0 {
			t.Fatalf("not due yet: %+v", due)
		}
		clock = clock.Add(11 * time.Second)
		due, _ := st.Due(t.Context(), 10)
		if len(due) != 1 {
			t.Fatalf("due: %+v", due)
		}
		got, attempt, ok, err := st.Claim(t.Context(), org, n.ID, "system", false)
		if err != nil || !ok || attempt != 2 || got.Estado != EstadoProcesando {
			t.Fatalf("claim: %+v %d %v %v", got, attempt, ok, err)
		}
		if _, _, ok2, _ := st.Claim(t.Context(), org, n.ID, "system", false); ok2 {
			t.Fatal("a claimed row must not be claimable twice")
		}
		r, _ = st.Resolve(t.Context(), org, n.ID, Result{Estado: EstadoReintentar, Motivo: "api 503"})
		if !r.ProximoIntentoAt.Equal(clock.Add(30 * time.Second)) {
			t.Fatalf("second backoff step: %v", r.ProximoIntentoAt)
		}
		clock = clock.Add(time.Minute)
		st.Claim(t.Context(), org, n.ID, "system", false)
		r, _ = st.Resolve(t.Context(), org, n.ID, Result{Estado: EstadoReintentar, Motivo: "api 503"})
		if r.Estado != EstadoDeadLetter || !strings.Contains(r.Motivo, "agotados") {
			t.Fatalf("backoff exhausted must dead-letter: %+v", r)
		}
		if len(retried) != 2 {
			t.Errorf("OnRetry calls = %d", len(retried))
		}
		open, _, _ := inc.List(t.Context(), incidents.Filter{Org: org, OpenOnly: true, Kind: incidents.KindDeadLetter})
		if len(open) != 1 || open[0].Severity != incidents.SeverityHigh {
			t.Fatalf("dead letter must open a high incident: %+v", open)
		}
		atts, _ := st.Attempts(t.Context(), org, n.ID)
		if len(atts) != 3 || atts[2].Resultado != EstadoDeadLetter || atts[0].EndedAt == nil {
			t.Fatalf("attempts: %+v", atts)
		}
		// Manual reprocess is allowed on dead letters, and resolving closes the incident.
		if _, _, ok, _ := st.Claim(t.Context(), org, n.ID, "user-1", true); !ok {
			t.Fatal("force claim of a dead letter")
		}
		if _, err := st.Resolve(t.Context(), org, n.ID, Result{Estado: EstadoReconciliadaAPI, Actor: "user-1", ResourceKind: "payment", ResourceID: "123"}); err != nil {
			t.Fatal(err)
		}
		open, _, _ = inc.List(t.Context(), incidents.Filter{Org: org, OpenOnly: true})
		if len(open) != 0 {
			t.Fatalf("incident should auto-close: %+v", open)
		}
		atts, _ = st.Attempts(t.Context(), org, n.ID)
		if atts[len(atts)-1].Actor != "user-1" {
			t.Errorf("attempt actor = %s", atts[len(atts)-1].Actor)
		}
	})
}

func TestOrphanedRowsAreDue(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		clock := time.Date(2026, 10, 20, 22, 0, 0, 0, time.UTC)
		st, _ := newStores(t, db, Config{Now: func() time.Time { return clock }})
		n, _ := st.Record(t.Context(), mpReq(mpBody))
		clock = clock.Add(10 * time.Second)
		if due, _ := st.Due(t.Context(), 10); len(due) != 0 {
			t.Fatal("fresh recibida is not orphaned")
		}
		clock = clock.Add(time.Minute)
		due, _ := st.Due(t.Context(), 10)
		if len(due) != 1 || due[0].ID != n.ID {
			t.Fatalf("recibida older than 30 s is orphaned: %+v", due)
		}
	})
}

// T17: a guest write failure becomes an incident and a retry.
func TestWriteFailureOpensIncident(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		st, inc := newStores(t, db, Config{})
		n, _ := st.Record(t.Context(), mpReq(mpBody))
		r, err := st.Resolve(t.Context(), org, n.ID, Result{Auth: AuthFirmaOK, Estado: EstadoReintentar, ResourceKind: "payment", ResourceID: "123",
			WriteFailures: []WriteFailure{{Table: "mp_payments", Cause: "update refused"}}})
		if err != nil {
			t.Fatal(err)
		}
		open, _, _ := inc.List(t.Context(), incidents.Filter{Org: org, OpenOnly: true})
		if r.Estado != EstadoReintentar || len(open) != 1 || open[0].Kind != incidents.KindPaymentWriteFailed ||
			open[0].Severity != incidents.SeverityHigh || !strings.Contains(open[0].Detail, "update refused") {
			t.Fatalf("write failure: %+v / %+v", r, open)
		}
	})
}

func TestResolveManualNeedsReasonAndReviewState(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		st, inc := newStores(t, db, Config{})
		n, _ := st.Record(t.Context(), mpReq(mpBody))
		if _, err := st.ResolveManual(t.Context(), org, n.ID, "u1", "x"); err == nil {
			t.Fatal("recibida cannot be resolved manually")
		}
		st.Resolve(t.Context(), org, n.ID, Result{Estado: EstadoRequiereRevision, ResourceID: "123", ResourceKind: "payment", Motivo: "?"})
		if _, err := st.ResolveManual(t.Context(), org, n.ID, "u1", " "); err == nil {
			t.Fatal("reason required")
		}
		r, err := st.ResolveManual(t.Context(), org, n.ID, "u1", "pago verificado en el panel de MP")
		if err != nil || r.Estado != EstadoResueltaManual {
			t.Fatalf("%+v %v", r, err)
		}
		if open, _, _ := inc.List(t.Context(), incidents.Filter{Org: org, OpenOnly: true}); len(open) != 0 {
			t.Fatalf("manual resolution closes the unresolved incident: %+v", open)
		}
	})
}

func TestStatsAndList(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		st, _ := newStores(t, db, Config{})
		a, _ := st.Record(t.Context(), mpReq(mpBody))
		st.Resolve(t.Context(), org, a.ID, Result{Auth: AuthFirmaOK, Estado: EstadoProcesada, ResourceID: "123"})
		b, _ := st.Record(t.Context(), mpReq(strings.Replace(mpBody, "123", "2", 1)))
		st.Resolve(t.Context(), org, b.ID, Result{Auth: AuthFirmaInvalida, Estado: EstadoRequiereRevision, ResourceID: "2"})
		stats, err := st.Stats(t.Context(), org)
		if err != nil {
			t.Fatal(err)
		}
		if stats.ByEstado[EstadoRequiereRevision] != 1 || stats.InvalidSignature24h["mercadopago"] != 1 {
			t.Fatalf("stats: %+v", stats)
		}
		if _, ok := stats.LastSigned["mercadopago"]; !ok {
			t.Fatalf("last signed missing: %+v", stats.LastSigned)
		}
		rows, total, _ := st.List(t.Context(), Filter{Org: org, Estados: ReviewEstados})
		if total != 1 || len(rows) != 1 || rows[0].ID != b.ID {
			t.Fatalf("review list: %d %+v", total, rows)
		}
	})
}

func TestPurgeAndExport(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		clock := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
		st, _ := newStores(t, db, Config{Now: func() time.Time { return clock }})
		old := mpReq(mpBody)
		old.ReceivedAt = clock.Add(-100 * 24 * time.Hour)
		oldN, _ := st.Record(t.Context(), old)
		st.Resolve(t.Context(), org, oldN.ID, Result{Auth: AuthFirmaOK, Estado: EstadoProcesada, ResourceID: "123"})
		pend := mpReq(strings.Replace(mpBody, "123", "2", 1))
		pend.ReceivedAt = clock.Add(-100 * 24 * time.Hour)
		pendN, _ := st.Record(t.Context(), pend)
		st.Resolve(t.Context(), org, pendN.ID, Result{Estado: EstadoRequiereRevision})
		fresh, _ := st.Record(mpCtx(), mpReq(strings.Replace(mpBody, "123", "3", 1)))

		rep, err := st.Purge(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if rep.Blanked != 1 || rep.Deleted != 0 {
			t.Fatalf("purge report %+v", rep)
		}
		raw, _, _ := st.Raw(t.Context(), org, oldN.ID)
		if len(raw) != 0 {
			t.Error("old body_raw must be gone")
		}
		got, _ := st.Get(t.Context(), org, oldN.ID)
		if got.PurgedAt == nil || got.BodySHA256 == "" || got.BodyRedacted == "" || got.Estado != EstadoProcesada {
			t.Errorf("result columns and hash must stay: %+v", got)
		}
		if raw, _, _ := st.Raw(t.Context(), org, pendN.ID); len(raw) == 0 {
			t.Error("rows awaiting review are never purged")
		}
		if raw, _, _ := st.Raw(t.Context(), org, fresh.ID); len(raw) == 0 {
			t.Error("fresh body must stay")
		}

		// Export: no raw body, CSV and NDJSON.
		var buf bytes.Buffer
		n, err := st.Export(t.Context(), &buf, Filter{Org: org}, FormatCSV)
		if err != nil || n != 3 {
			t.Fatalf("csv export: %d %v", n, err)
		}
		recs, err := csv.NewReader(&buf).ReadAll()
		if err != nil || len(recs) != 4 || recs[0][0] != "id" {
			t.Fatalf("csv shape: %v %v", recs, err)
		}
		if strings.Contains(buf.String(), hash64) {
			t.Error("export leaked a signature")
		}
		buf.Reset()
		if n, err := st.Export(t.Context(), &buf, Filter{Org: org, Estados: []string{EstadoRequiereRevision}}, FormatNDJSON); err != nil || n != 1 ||
			!strings.Contains(buf.String(), `"estado":"requiere_revision"`) || strings.Contains(buf.String(), "body_raw") {
			t.Fatalf("ndjson export: %d %v %s", n, err, buf.String())
		}

		// 24 months later everything closed is deleted.
		clock = clock.Add(800 * 24 * time.Hour)
		rep, err = st.Purge(t.Context())
		if err != nil || rep.Deleted < 1 {
			t.Fatalf("retention delete: %+v %v", rep, err)
		}
		if _, err := st.Get(t.Context(), org, oldN.ID); !errors.Is(err, ErrNotFound) {
			t.Error("old closed row should be deleted")
		}
		if _, err := st.Get(t.Context(), org, pendN.ID); err != nil {
			t.Error("row awaiting review must survive retention")
		}
	})
}

func mpCtx() context.Context { return context.Background() }

func TestSourceIP(t *testing.T) {
	h := http.Header{}
	h.Set("CF-Connecting-IP", "198.51.100.7")
	if got := SourceIP("127.0.0.1:5555", h, nil); got != "198.51.100.7" {
		t.Errorf("behind own proxy: %s", got)
	}
	if got := SourceIP("203.0.113.1:5555", h, nil); got != "203.0.113.1" {
		t.Errorf("an untrusted peer must not be able to spoof CF-Connecting-IP: %s", got)
	}
	if got := SourceIP("garbage", h, nil); got != "" {
		t.Errorf("bad peer: %q", got)
	}
}
