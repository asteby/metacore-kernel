package diag

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/capability"
)

func mkRec(t *testing.T, db *gorm.DB, clk *clock, opt Options) (*Recorder, *Store) {
	t.Helper()
	on := true
	opt.Enabled = &on
	opt.FlushEvery = time.Hour
	if clk != nil {
		opt.Now = clk.Now
	}
	st := NewStore(db)
	return NewRecorder(st, opt), st
}

func failure(org uuid.UUID, mut ...func(*Diagnostic)) Diagnostic {
	d := Diagnostic{Org: org, AddonKey: "acme", AddonVersion: "1.2.3", Kind: KindHTTP, Operation: "pay", Code: "provider_error",
		HTTPStatus: 400, ProviderCode: []string{"bad_request"}, Props: []string{"payer.email"}, Severity: SeverityError,
		CorrelationID: "corr-1", InvocationID: "inv-" + uuid.NewString()}
	for _, m := range mut {
		m(&d)
	}
	return d
}

func TestRecorder_DedupGroupsAndCapsSamples(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		clk := newClock()
		rec, st := mkRec(t, db, clk, Options{RatePerMin: 10000})
		org := uuid.New()
		ctx := context.Background()
		for round := 0; round < 6; round++ {
			for i := 0; i < 5; i++ {
				clk.Advance(time.Second)
				rec.Record(failure(org), nil)
			}
			rec.Flush(ctx)
		}
		gs, total, err := st.List(ctx, Filter{Org: org})
		if err != nil || total != 1 || len(gs) != 1 {
			t.Fatalf("groups=%v total=%d err=%v", gs, total, err)
		}
		g := gs[0]
		if g.Count != 30 || g.Status != StatusOpen || g.ID != GroupID(org, g.Fingerprint) {
			t.Fatalf("group=%+v", g)
		}
		_, evs, err := st.Get(ctx, org, g.ID)
		if err != nil {
			t.Fatal(err)
		}
		// 20 muestras + la primera.
		if len(evs) != MaxSamplesPerGroup+1 {
			t.Fatalf("events=%d, want %d", len(evs), MaxSamplesPerGroup+1)
		}
		firsts := 0
		for _, e := range evs {
			if e.IsFirst {
				firsts++
			}
		}
		if firsts != 1 {
			t.Fatalf("firsts=%d", firsts)
		}
		var n int64
		db.Raw("SELECT count(*) FROM " + TableEvents).Scan(&n)
		if n != MaxSamplesPerGroup+1 {
			t.Fatalf("table rows=%d", n)
		}
		// Otra huella = otro grupo.
		rec.Record(failure(org, func(d *Diagnostic) { d.HTTPStatus = 422 }), nil)
		rec.Flush(ctx)
		if _, total, _ := st.List(ctx, Filter{Org: org}); total != 2 {
			t.Fatalf("total=%d", total)
		}
	})
}

func TestRecorder_FloodControlOnlyBumpsCounter(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		clk := newClock()
		rec, st := mkRec(t, db, clk, Options{RatePerMin: 3})
		org := uuid.New()
		ctx := context.Background()
		for i := 0; i < 200; i++ { // mismo instante: sin reposicion de tokens
			rec.Record(failure(org), nil)
		}
		// Otro addon de la misma org tiene su propio cubo.
		rec.Record(failure(org, func(d *Diagnostic) { d.AddonKey = "other" }), nil)
		rec.Flush(ctx)
		gs, _, _ := st.List(ctx, Filter{Org: org, Addon: "acme"})
		if len(gs) != 1 || gs[0].Count != 200 {
			t.Fatalf("acme=%+v", gs)
		}
		_, evs, _ := st.Get(ctx, org, gs[0].ID)
		if len(evs) != 3 {
			t.Fatalf("samples=%d, want 3 (token bucket)", len(evs))
		}
		og, _, _ := st.List(ctx, Filter{Org: org, Addon: "other"})
		if len(og) != 1 {
			t.Fatalf("other=%+v", og)
		}
		_, oe, _ := st.Get(ctx, org, og[0].ID)
		if len(oe) != 1 {
			t.Fatalf("other samples=%d", len(oe))
		}
		// Pasado un minuto el cubo se repone.
		clk.Advance(time.Minute)
		rec.Record(failure(org), nil)
		rec.Flush(ctx)
		_, evs, _ = st.Get(ctx, org, gs[0].ID)
		if len(evs) != 4 {
			t.Fatalf("samples after refill=%d", len(evs))
		}
	})
}

func TestStore_OrgIsolation(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		rec, st := mkRec(t, db, nil, Options{})
		a, b := uuid.New(), uuid.New()
		ctx := context.Background()
		rec.Record(failure(a, func(d *Diagnostic) { d.CorrelationID = "shared-corr"; d.Subject = Subject{Type: "payment", ID: "p1"} }), nil)
		rec.Record(failure(b, func(d *Diagnostic) { d.CorrelationID = "shared-corr"; d.Subject = Subject{Type: "payment", ID: "p1"} }), nil)
		rec.Flush(ctx)
		ga, _, _ := st.List(ctx, Filter{Org: a})
		gb, _, _ := st.List(ctx, Filter{Org: b})
		if len(ga) != 1 || len(gb) != 1 || ga[0].ID == gb[0].ID {
			t.Fatalf("same fingerprint across orgs must be separate groups: %v %v", ga, gb)
		}
		if _, _, err := st.Get(ctx, a, gb[0].ID); err != ErrNotFound {
			t.Fatalf("org A read B's group: %v", err)
		}
		if err := st.SetStatus(ctx, a, gb[0].ID, StatusResolved); err != ErrNotFound {
			t.Fatalf("org A resolved B's group: %v", err)
		}
		evs, err := st.Events(ctx, EventQuery{Org: a, CorrelationID: "shared-corr"})
		if err != nil || len(evs) != 1 || evs[0].GroupID != ga[0].ID {
			t.Fatalf("events=%v err=%v", evs, err)
		}
		evs, _ = st.Events(ctx, EventQuery{Org: b, SubjectType: "payment", SubjectID: "p1"})
		if len(evs) != 1 || evs[0].GroupID != gb[0].ID {
			t.Fatalf("subject events=%v", evs)
		}
		if _, _, err := st.List(ctx, Filter{}); err != ErrOrgRequired {
			t.Fatalf("nil org: %v", err)
		}
		if _, err := st.Events(ctx, EventQuery{CorrelationID: "x"}); err != ErrOrgRequired {
			t.Fatalf("nil org events: %v", err)
		}
		if _, err := st.Events(ctx, EventQuery{Org: a}); err == nil {
			t.Fatal("events with no criteria must fail")
		}
		if _, _, err := st.Get(ctx, uuid.Nil, ga[0].ID); err != ErrOrgRequired {
			t.Fatalf("nil org get: %v", err)
		}
	})
}

func TestStore_StatusLifecycle(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		clk := newClock()
		rec, st := mkRec(t, db, clk, Options{})
		org := uuid.New()
		ctx := context.Background()
		rec.Record(failure(org), nil)
		rec.Flush(ctx)
		g, _, _ := st.List(ctx, Filter{Org: org})
		id := g[0].ID
		if err := st.SetStatus(ctx, org, id, StatusResolved); err != nil {
			t.Fatal(err)
		}
		clk.Advance(time.Minute)
		rec.Record(failure(org), nil)
		rec.Flush(ctx)
		g, _, _ = st.List(ctx, Filter{Org: org})
		if g[0].Status != StatusOpen || g[0].Count != 2 {
			t.Fatalf("a resolved group that recurs must reopen: %+v", g[0])
		}
		_ = st.SetStatus(ctx, org, id, StatusMuted)
		clk.Advance(time.Minute)
		rec.Record(failure(org), nil)
		rec.Flush(ctx)
		g, _, _ = st.List(ctx, Filter{Org: org})
		if g[0].Status != StatusMuted || g[0].Count != 3 {
			t.Fatalf("a muted group stays muted: %+v", g[0])
		}
		if err := st.SetStatus(ctx, org, id, "bogus"); err != ErrBadStatus {
			t.Fatal(err)
		}
		if gs, _, _ := st.List(ctx, Filter{Org: org, Status: StatusOpen}); len(gs) != 0 {
			t.Fatal("status filter")
		}
		if gs, _, _ := st.List(ctx, Filter{Org: org, Since: clk.Now().Add(time.Hour)}); len(gs) != 0 {
			t.Fatal("since filter")
		}
	})
}

func TestRecorder_RetentionPurge(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		clk := newClock()
		rec, st := mkRec(t, db, clk, Options{})
		org := uuid.New()
		ctx := context.Background()
		rec.Record(failure(org, func(d *Diagnostic) { d.Code = "old_group" }), nil)
		rec.Record(failure(org, func(d *Diagnostic) { d.Code = "old_group" }), nil)
		rec.Flush(ctx)
		clk.Advance(20 * 24 * time.Hour)
		rec.Record(failure(org, func(d *Diagnostic) { d.Code = "old_group" }), nil)
		rec.Record(failure(org, func(d *Diagnostic) { d.Code = "fresh" }), nil)
		rec.Flush(ctx)
		clk.Advance(100 * 24 * time.Hour)
		rec.Record(failure(org, func(d *Diagnostic) { d.Code = "fresh" }), nil)
		rec.Flush(ctx)
		// En este punto: old_group ultimo visto hace 100d (>90d), fresh hace 0d.
		ev, gr, err := rec.PurgeNow(ctx)
		if err != nil || gr != 1 || ev < 3 {
			t.Fatalf("purged events=%d groups=%d err=%v", ev, gr, err)
		}
		gs, _, _ := st.List(ctx, Filter{Org: org})
		if len(gs) != 1 || gs[0].Code != "fresh" {
			t.Fatalf("groups left=%+v", gs)
		}
		// La primera muestra de "fresh" tiene 100d (>14d) pero vive con su grupo;
		// la muestra intermedia de 100d se va.
		_, evs, _ := st.Get(ctx, org, gs[0].ID)
		var firsts, others int
		for _, e := range evs {
			if e.IsFirst {
				firsts++
			} else {
				others++
			}
		}
		if firsts != 1 || others != 1 {
			t.Fatalf("firsts=%d others=%d", firsts, others)
		}
		var orphans int64
		db.Raw("SELECT count(*) FROM " + TableEvents + " WHERE group_id NOT IN (SELECT id FROM " + TableGroups + ")").Scan(&orphans)
		if orphans != 0 {
			t.Fatalf("orphan events=%d", orphans)
		}
	})
}

type fakeAlerter struct {
	mu sync.Mutex
	as []Alert
}

func (f *fakeAlerter) Alert(_ context.Context, a Alert) error {
	f.mu.Lock()
	f.as = append(f.as, a)
	f.mu.Unlock()
	return nil
}
func (f *fakeAlerter) reasons() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var r []string
	for _, a := range f.as {
		r = append(r, a.Reason)
	}
	return r
}

func TestRecorder_Alerts(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		clk := newClock()
		al := &fakeAlerter{}
		rec, _ := mkRec(t, db, clk, Options{Alerter: al})
		org := uuid.New()
		ctx := context.Background()
		rec.Record(failure(org), nil)
		rec.Flush(ctx)
		if got := strings.Join(al.reasons(), ","); got != "new_group" {
			t.Fatalf("first: %q", got)
		}
		// Mismo grupo, pocos fallos: sin alerta.
		for i := 0; i < 3; i++ {
			clk.Advance(time.Minute)
			rec.Record(failure(org), nil)
		}
		rec.Flush(ctx)
		if got := strings.Join(al.reasons(), ","); got != "new_group" {
			t.Fatalf("below threshold: %q", got)
		}
		// El quinto en 10 minutos dispara la rafaga, una sola vez.
		clk.Advance(time.Minute)
		rec.Record(failure(org), nil)
		clk.Advance(time.Minute)
		rec.Record(failure(org), nil)
		rec.Flush(ctx)
		if got := strings.Join(al.reasons(), ","); got != "new_group,burst" {
			t.Fatalf("burst: %q", got)
		}
		clk.Advance(time.Minute)
		rec.Record(failure(org), nil)
		rec.Flush(ctx)
		if got := strings.Join(al.reasons(), ","); got != "new_group,burst" {
			t.Fatalf("burst must not repeat inside the window: %q", got)
		}
		// Un grupo nuevo de severidad warning no alerta.
		rec.Record(failure(org, func(d *Diagnostic) { d.Code = "soft"; d.Severity = SeverityWarning }), nil)
		rec.Flush(ctx)
		if got := strings.Join(al.reasons(), ","); got != "new_group,burst" {
			t.Fatalf("warning: %q", got)
		}
		if al.as[0].Org != org || al.as[0].AddonKey != "acme" || al.as[0].Code != "provider_error" {
			t.Fatalf("alert=%+v", al.as[0])
		}
	})
}

func TestRecorder_SwitchAndNilSafety(t *testing.T) {
	t.Setenv(EnvSwitch, "off")
	if EnabledFromEnv() {
		t.Fatal("KERNEL_DIAGNOSTICS=off must disable")
	}
	rec := NewRecorder(NewStore(nil), Options{})
	if rec.Enabled() {
		t.Fatal("disabled by env")
	}
	rec.Record(failure(uuid.New()), nil) // no panic
	var nilRec *Recorder
	nilRec.Record(failure(uuid.New()), nil)
	nilRec.Flush(context.Background())
	nilRec.Start(context.Background())
	nilRec.Close()
	if nilRec.Enabled() || nilRec.VersionOf("x") != "" || nilRec.Dropped() != 0 {
		t.Fatal("nil recorder")
	}
	t.Setenv(EnvSwitch, "")
	if !EnabledFromEnv() {
		t.Fatal("default on")
	}
	_ = os.Getenv
}

func TestRecorder_StartCloseFlushes(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		rec, st := mkRec(t, db, nil, Options{})
		org := uuid.New()
		rec.Start(context.Background())
		rec.Record(failure(org), nil)
		rec.Close()
		rec.Close() // idempotente
		if _, total, _ := st.List(context.Background(), Filter{Org: org}); total != 1 {
			t.Fatalf("total=%d", total)
		}
	})
}

func TestRecorder_SanitizesBeforeStoring(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		rec, st := mkRec(t, db, nil, Options{})
		org := uuid.New()
		sc := NewScrubber("hunter2-secret")
		rec.Record(failure(org, func(d *Diagnostic) {
			d.Props = []string{"payer.email", "ana@x.com"}
			d.Breadcrumbs = []string{"password=hunter2-secret email ana@x.com pan 4111111111111111"}
			d.UpstreamHost = "https://u:p@Api.Example.com/x?token=1"
		}), sc)
		rec.Flush(context.Background())
		var raw []string
		db.Raw("SELECT payload FROM " + TableEvents).Scan(&raw)
		if len(raw) != 1 {
			t.Fatalf("rows=%d", len(raw))
		}
		for _, leak := range []string{"hunter2", "ana@x.com", "4111", "token=1", "u:p"} {
			if strings.Contains(raw[0], leak) {
				t.Fatalf("stored payload leaks %q: %s", leak, raw[0])
			}
		}
		_ = st
	})
}

func TestRecorder_ConcurrencyExactCounts(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		if db.Dialector.Name() != "postgres" {
			t.Skip("la concurrencia entre replicas se prueba en Postgres")
		}
		org := uuid.New()
		ctx := context.Background()
		const replicas, perReplica = 6, 400
		var recs []*Recorder
		for i := 0; i < replicas; i++ {
			rec, _ := mkRec(t, db, nil, Options{RatePerMin: 100000})
			recs = append(recs, rec)
		}
		var wg sync.WaitGroup
		var flushes atomic.Int64
		for _, rec := range recs {
			rec := rec
			wg.Add(1)
			go func() {
				defer wg.Done()
				var inner sync.WaitGroup
				for g := 0; g < 8; g++ {
					inner.Add(1)
					go func() {
						defer inner.Done()
						for i := 0; i < perReplica/8; i++ {
							rec.Record(failure(org), nil)
							if i%10 == 0 {
								rec.Flush(ctx)
								flushes.Add(1)
							}
						}
					}()
				}
				inner.Wait()
				rec.Flush(ctx)
			}()
		}
		wg.Wait()
		st := NewStore(db)
		gs, total, err := st.List(ctx, Filter{Org: org})
		if err != nil || total != 1 {
			t.Fatalf("groups=%v err=%v", gs, err)
		}
		if gs[0].Count != replicas*perReplica {
			t.Fatalf("count=%d want %d (flushes=%d)", gs[0].Count, replicas*perReplica, flushes.Load())
		}
		var firsts, rows int64
		db.Raw("SELECT count(*) FROM " + TableEvents + " WHERE is_first = true").Scan(&firsts)
		db.Raw("SELECT count(*) FROM " + TableEvents + " WHERE is_first = false").Scan(&rows)
		if firsts != 1 || rows > MaxSamplesPerGroup {
			t.Fatalf("firsts=%d others=%d", firsts, rows)
		}
		var typ string
		if err := db.Raw("SELECT DISTINCT jsonb_typeof(payload) FROM " + TableEvents).Scan(&typ).Error; err != nil || typ != "object" {
			t.Fatalf("jsonb_typeof=%q err=%v", typ, err)
		}
	})
}

func TestOnCapabilityFailure(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		rec, st := mkRec(t, db, nil, Options{})
		org := uuid.New()
		ex := capability.Executor{Timeout: 20 * time.Millisecond, OnFailure: rec.OnCapabilityFailure}
		c, ok := capability.Lookup(capability.CommerceShippingRates)
		if !ok {
			t.Fatal("no contract")
		}
		providers := []capability.Provider{{Addon: "slowpost"}, {Addon: "broken"}, {Addon: "fine"}}
		invoke := func(ctx context.Context, call capability.Call) ([]byte, error) {
			switch call.Provider.Addon {
			case "slowpost":
				<-ctx.Done()
				return nil, ctx.Err()
			case "broken":
				return nil, capability.NewCodedError("bad_gateway", "upstream rate.zone is down for ana@x.com")
			}
			return []byte(`{"rates":[]}`), nil
		}
		in := map[string]any{
			"origin":      map[string]any{"postal_code": "64000", "country": "MX"},
			"destination": map[string]any{"postal_code": "06600", "country": "MX"},
			"parcels":     []any{map[string]any{"weight_kg": "1.5"}}, "currency": "MXN",
		}
		_, err := ex.Run(context.Background(), c, org, "mx", providers, in, func(ctx context.Context, call capability.Call) (json.RawMessage, error) {
			out, err := invoke(ctx, call)
			return out, err
		})
		if err != nil {
			t.Fatal(err)
		}
		rec.Flush(context.Background())
		gs, _, _ := st.List(context.Background(), Filter{Org: org})
		got := map[string]string{}
		for _, g := range gs {
			got[g.AddonKey] = g.Code
			if g.Kind != "capability" || g.Operation != capability.CommerceShippingRates {
				t.Fatalf("group=%+v", g)
			}
		}
		if len(got) != 2 || got["slowpost"] != "timeout" || got["broken"] != "provider_error" {
			t.Fatalf("got=%v", got)
		}
		for _, g := range gs {
			if g.AddonKey == "broken" {
				_, evs, _ := st.Get(context.Background(), org, g.ID)
				d := evs[0].Diagnostic
				if !has(d.ProviderCode, "bad_gateway") || !has(d.Props, "rate.zone") || strings.Contains(strings.Join(d.Props, ","), "ana") {
					t.Fatalf("diag=%+v", d)
				}
			}
		}
	})
}
