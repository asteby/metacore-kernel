package incidents

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/asteby/metacore-kernel/notifications"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var org = uuid.MustParse("22222222-2222-2222-2222-222222222222")

func randHex() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func eachDialect(t *testing.T, fn func(t *testing.T, db *gorm.DB)) {
	t.Run("SQLite", func(t *testing.T) {
		db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:inc_%s?mode=memory&cache=shared&_busy_timeout=5000", randHex())),
			&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
		if err != nil {
			t.Fatal(err)
		}
		s, _ := db.DB()
		s.SetMaxOpenConns(1)
		t.Cleanup(func() { _ = s.Close() })
		fn(t, db)
	})
	t.Run("Postgres", func(t *testing.T) {
		dsn := os.Getenv("TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("TEST_POSTGRES_DSN not set")
		}
		cfg := &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)}
		admin, err := gorm.Open(postgres.Open(dsn), cfg)
		if err != nil {
			t.Fatal(err)
		}
		schema := "inc_test_" + randHex()
		admin.Exec("CREATE SCHEMA " + schema)
		u, _ := url.Parse(dsn)
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		db, err := gorm.Open(postgres.Open(u.String()), cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if s, err := db.DB(); err == nil {
				_ = s.Close()
			}
			admin.Exec("DROP SCHEMA " + schema + " CASCADE")
			if s, err := admin.DB(); err == nil {
				_ = s.Close()
			}
		})
		fn(t, db)
	})
}

type fakeNotifier struct {
	mu   sync.Mutex
	reqs []notifications.EnqueueRequest
}

func (f *fakeNotifier) Enqueue(_ context.Context, r notifications.EnqueueRequest) (notifications.EnqueueResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, r)
	return notifications.EnqueueResult{EntryID: uuid.New()}, nil
}

func TestOpenGroupsByFingerprintAndBridgesOnce(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		var events []string
		notif := &fakeNotifier{}
		var flags atomic.Int32
		st, err := New(t.Context(), db, Config{
			Publish: func(_ context.Context, emitter, event string, o uuid.UUID, _ any) error {
				events = append(events, emitter+"|"+event)
				return nil
			},
			Notifier:   notif,
			Recipients: func(context.Context, uuid.UUID) ([]string, error) { return []string{"owner-1", "admin-1"}, nil },
			RuleFlag:   func(context.Context, Incident) error { flags.Add(1); return nil },
		})
		if err != nil {
			t.Fatal(err)
		}
		in := Input{Org: org, Domain: DomainPayments, Source: "mercadopago", Kind: "payments.signature_invalid", Title: "firma", ActionHint: "actualiza el secreto"}
		a, created, err := st.Open(t.Context(), in)
		if err != nil || !created || a.Severity != SeverityHigh || a.Fingerprint != "payments.signature_invalid:mercadopago" {
			t.Fatalf("first open: %+v %v %v", a, created, err)
		}
		b, created, err := st.Open(t.Context(), in)
		if err != nil || created || b.ID != a.ID || b.Occurrences != 2 {
			t.Fatalf("second open must group: %+v %v %v", b, created, err)
		}
		if len(events) != 1 || events[0] != "kernel|kernel.payments.incident_opened" {
			t.Errorf("event published as kernel exactly once: %v", events)
		}
		if len(notif.reqs) != 2 || notif.reqs[0].Channel != "in_app" || !strings.Contains(notif.reqs[0].Message, "actualiza el secreto") {
			t.Errorf("notifications: %+v", notif.reqs)
		}
		if flags.Load() != 1 {
			t.Errorf("rule flag calls = %d", flags.Load())
		}

		// Another organization is a separate incident.
		other, created, _ := st.Open(t.Context(), Input{Org: uuid.New(), Domain: DomainPayments, Source: "mercadopago", Kind: "payments.signature_invalid"})
		if !created || other.ID == a.ID {
			t.Error("org isolation")
		}

		// Escalation re-bridges.
		low, _, _ := st.Open(t.Context(), Input{Org: org, Domain: DomainPayments, Source: "stripe", Kind: "payments.late_payment", Severity: SeverityLow, Fingerprint: "late:1"})
		_, _, _ = st.Open(t.Context(), Input{Org: org, Domain: DomainPayments, Source: "stripe", Kind: "payments.late_payment", Severity: SeverityHigh, Fingerprint: "late:1"})
		again, _ := st.Get(t.Context(), org, low.ID)
		if again.Severity != SeverityHigh || len(events) != 4 {
			t.Errorf("escalation: %+v events=%v", again, events)
		}

		// Resolve rules.
		if err := st.Resolve(t.Context(), org, a.ID, "u1", " "); err == nil {
			t.Error("manual resolve needs a reason")
		}
		if err := st.Acknowledge(t.Context(), org, a.ID, "u1"); err != nil {
			t.Fatal(err)
		}
		if got, _ := st.Get(t.Context(), org, a.ID); got.Status != StatusAcknowledged || got.AckedBy != "u1" || !got.IsOpen() {
			t.Errorf("acknowledged must stay open: %+v", got)
		}
		if mid, _, _ := st.Open(t.Context(), in); mid.ID != a.ID || mid.Occurrences != 3 {
			t.Errorf("an acknowledged incident keeps grouping: %+v", mid)
		}
		if err := st.Resolve(t.Context(), org, a.ID, "u1", "secreto actualizado"); err != nil {
			t.Fatal(err)
		}
		if got, _ := st.Get(t.Context(), org, a.ID); got.Status != StatusResolved || got.ResolvedBy != "u1" {
			t.Errorf("resolved: %+v", got)
		}
		if err := st.Resolve(t.Context(), org, a.ID, "u1", "otra vez"); !errors.Is(err, gorm.ErrRecordNotFound) {
			t.Errorf("closing twice: %v", err)
		}
		// History is kept and a new occurrence after closing opens a NEW incident.
		c, created, _ := st.Open(t.Context(), in)
		if !created || c.ID == a.ID || c.Occurrences != 1 {
			t.Errorf("reopen after close: %+v %v", c, created)
		}
		counts, _ := st.OpenCounts(t.Context(), org, DomainPayments)
		if counts[SeverityHigh] != 2 {
			t.Errorf("counts: %v", counts)
		}
	})
}

func TestBridgeFailureNeverFailsOpen(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		st, _ := New(t.Context(), db, Config{
			Publish:    func(context.Context, string, string, uuid.UUID, any) error { return errors.New("bus down") },
			Notifier:   &fakeNotifier{},
			Recipients: func(context.Context, uuid.UUID) ([]string, error) { return nil, errors.New("no users") },
			RuleFlag:   func(context.Context, Incident) error { return errors.New("flag") },
		})
		if _, created, err := st.Open(t.Context(), Input{Org: org, Domain: DomainPayments, Kind: "payments.dead_letter", Source: "p"}); err != nil || !created {
			t.Fatalf("a failing bridge must not fail Open: %v", err)
		}
	})
}

func TestConcurrentOpenOneRow(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		st, _ := New(t.Context(), db, Config{})
		var wg sync.WaitGroup
		var created atomic.Int32
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, c, err := st.Open(context.Background(), Input{Org: org, Domain: DomainPayments, Kind: "payments.signature_invalid", Source: "mp"}); err == nil && c {
					created.Add(1)
				}
			}()
		}
		wg.Wait()
		rows, total, _ := st.List(t.Context(), Filter{Org: org, OpenOnly: true})
		if db.Dialector.Name() == "postgres" && (total != 1 || created.Load() != 1) {
			t.Fatalf("one open incident expected, got total=%d created=%d", total, created.Load())
		}
		if total >= 1 && rows[0].Occurrences < 1 {
			t.Fatal("bad row")
		}
	})
}

func TestOpenValidation(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		st, _ := New(t.Context(), db, Config{})
		if _, _, err := st.Open(t.Context(), Input{Kind: "x", Domain: "d"}); err == nil {
			t.Error("org required")
		}
		if _, _, err := st.Open(t.Context(), Input{Org: org, Domain: "d"}); err == nil {
			t.Error("kind required")
		}
	})
}

// The same fingerprint in two domains is two incidents; filters and counts
// are per domain; closing by fingerprint only touches its own domain.
func TestDomainsDoNotMix(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		st, _ := New(t.Context(), db, Config{})
		var events []string
		st.cfg.Publish = func(_ context.Context, _, ev string, _ uuid.UUID, _ any) error {
			events = append(events, ev)
			return nil
		}
		pay, c1, _ := st.Open(t.Context(), Input{Org: org, Domain: DomainPayments, Kind: "payments.x", Fingerprint: "same", Severity: SeverityHigh})
		inv, c2, _ := st.Open(t.Context(), Input{Org: org, Domain: DomainInventory, Kind: "inventory.low_stock", Fingerprint: "same", Severity: SeverityLow})
		if !c1 || !c2 || pay.ID == inv.ID {
			t.Fatalf("same fingerprint, different domains must not merge: %+v %+v", pay, inv)
		}
		if len(events) != 2 || events[0] != "kernel.payments.incident_opened" || events[1] != "kernel.inventory.incident_opened" {
			t.Errorf("events: %v", events)
		}
		rows, total, _ := st.List(t.Context(), Filter{Org: org, Domain: DomainInventory})
		if total != 1 || rows[0].ID != inv.ID {
			t.Errorf("domain filter: %+v", rows)
		}
		if _, total, _ := st.List(t.Context(), Filter{Org: org}); total != 2 {
			t.Errorf("no domain = all, got %d", total)
		}
		hi, _ := st.OpenCounts(t.Context(), org, DomainPayments)
		all, _ := st.OpenCounts(t.Context(), org, "")
		if hi[SeverityHigh] != 1 || hi[SeverityLow] != 0 || all[SeverityLow] != 1 {
			t.Errorf("counts: %v %v", hi, all)
		}
		n, _ := st.CloseByFingerprint(t.Context(), org, DomainPayments, "same", "ok")
		if n != 1 {
			t.Fatalf("closed %d", n)
		}
		if got, _ := st.Get(t.Context(), org, inv.ID); !got.IsOpen() {
			t.Error("closing payments/same must not close inventory/same")
		}
		if got, _ := st.Get(t.Context(), org, pay.ID); got.Status != StatusAutoResolved || got.ResolvedBy != "system" {
			t.Errorf("auto-resolved: %+v", got)
		}
		if _, _, err := st.Open(t.Context(), Input{Org: org, Kind: "x"}); err == nil {
			t.Error("domain required")
		}
	})
}

func TestMigrateIdempotent(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		for i := 0; i < 3; i++ {
			if err := Migrate(t.Context(), db); err != nil {
				t.Fatal(err)
			}
		}
	})
}
