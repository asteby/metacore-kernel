package automation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/asteby/metacore-kernel/agent/contextmgr"
	"github.com/asteby/metacore-kernel/agent/tools"
	"github.com/asteby/metacore-kernel/genui"
	"github.com/asteby/metacore-kernel/outbox"
)

// ---- fakes -------------------------------------------------------------------

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Set(t time.Time) { c.mu.Lock(); c.t = t; c.mu.Unlock() }

type fakeCatalog struct {
	mu        sync.Mutex
	items     []map[string]any
	fail      error
	forbidden bool
	calls     int
}

func (f *fakeCatalog) List(_ context.Context, _ Identity) []CatalogTool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.forbidden {
		return nil
	}
	return []CatalogTool{{
		Name: "buscar_hallazgos", Title: "Buscar hallazgos", Description: "Hallazgos",
		Parameters: map[string]any{
			"territorio": map[string]any{"type": "string"},
			"texto":      map[string]any{"type": "string"},
			"limite":     map[string]any{"type": "integer"},
		},
		ItemsPath: "resultados", ItemKey: "id", ItemTitle: "titulo", ItemURL: "evidencia.url", ItemDate: "evidencia.fecha",
	}}
}

func (f *fakeCatalog) Call(_ context.Context, _ Identity, tool string, args map[string]any) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.fail != nil {
		return nil, f.fail
	}
	if f.forbidden {
		return nil, ErrForbidden
	}
	out := []any{}
	for _, it := range f.items {
		if t, _ := args["territorio"].(string); t != "" && it["territorio"] != t {
			continue
		}
		out = append(out, it)
	}
	return map[string]any{"total": len(out), "resultados": out}, nil
}

func (f *fakeCatalog) set(items ...map[string]any) {
	f.mu.Lock()
	f.items = items
	f.mu.Unlock()
}

type fakeDeliverer struct {
	mu   sync.Mutex
	sent []Delivery
	fail error
}

func (d *fakeDeliverer) Deliver(_ context.Context, del Delivery) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.fail != nil {
		return 0, d.fail
	}
	d.sent = append(d.sent, del)
	return len(del.Channels), nil
}

func (d *fakeDeliverer) count() int { d.mu.Lock(); defer d.mu.Unlock(); return len(d.sent) }
func (d *fakeDeliverer) last() Delivery {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sent[len(d.sent)-1]
}

type fakeLLM struct {
	mu    sync.Mutex
	calls int
}

func (l *fakeLLM) Summarize(_ context.Context, req SummarizeRequest) (SummarizeResult, error) {
	l.mu.Lock()
	l.calls++
	l.mu.Unlock()
	return SummarizeResult{Text: fmt.Sprintf("Resumen IA de %d hallazgos", len(req.Items)), Model: req.Model, InputTokens: 100, OutputTokens: 40, CostUSD: 0.001}, nil
}

type harness struct {
	t   *testing.T
	db  *gorm.DB
	ob  *outbox.Service
	svc *Service
	clk *clock
	cat *fakeCatalog
	del *fakeDeliverer
	llm *fakeLLM
	org uuid.UUID
	ana uuid.UUID
}

var mx, _ = time.LoadLocation("America/Mexico_City")

func sqliteDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:auto_%s?mode=memory&cache=shared&_busy_timeout=5000", strings.ReplaceAll(uuid.NewString(), "-", ""))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

func newHarness(t *testing.T, db *gorm.DB, mutate func(*Policy)) *harness {
	t.Helper()
	clk := &clock{t: time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)} // 06:00 in Mexico City
	ob, err := outbox.New(db, outbox.Config{Now: clk.Now, MaxAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, db: db, ob: ob, clk: clk, cat: &fakeCatalog{}, del: &fakeDeliverer{}, llm: &fakeLLM{}, org: uuid.New(), ana: uuid.New()}
	p := DefaultPolicy()
	p.DefaultTimezone = "America/Mexico_City"
	p.Channels = []string{ChannelInApp, ChannelPush, ChannelWhatsApp}
	p.AssistantName = "Asistente"
	if mutate != nil {
		mutate(&p)
	}
	svc, err := New(Config{DB: db, Outbox: ob, Catalog: h.cat, Deliverer: h.del, Summarizer: h.llm, Policy: p, Now: clk.Now, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	h.svc = svc
	return h
}

func (h *harness) id() Identity { return Identity{OrganizationID: h.org, UserID: h.ana} }

// drain runs every pending job once (attempt = previous attempts + 1), like
// the outbox workers would, and marks it done or pending again.
func (h *harness) drain() int {
	h.t.Helper()
	ctx := context.Background()
	n := 0
	for range 5 {
		jobs, _, err := h.ob.List(ctx, outbox.Filter{Status: outbox.StatusPending, Limit: 100})
		if err != nil {
			h.t.Fatal(err)
		}
		if len(jobs) == 0 {
			return n
		}
		for _, j := range jobs {
			j.Attempts++
			var herr error
			switch j.Kind {
			case JobRun:
				herr = h.svc.handleRunJob(ctx, j)
			case JobEvent:
				herr = h.svc.handleEventJob(ctx, j)
			}
			status := "done"
			if herr != nil && !outbox.IsPermanent(herr) && j.Attempts < j.MaxAttempts {
				status = "pending"
			} else if herr != nil {
				status = "dead"
			}
			h.db.Exec("UPDATE metacore_outbox_jobs SET status = ?, attempts = ? WHERE id = ?", status, j.Attempts, j.ID)
			n++
		}
	}
	return n
}

func (h *harness) get(id uuid.UUID) *Automation {
	h.t.Helper()
	var a Automation
	if err := h.db.First(&a, "id = ?", id).Error; err != nil {
		h.t.Fatal(err)
	}
	return &a
}

func (h *harness) runs(id uuid.UUID) []Run {
	var rs []Run
	h.db.Where("automation_id = ?", id).Order("started_at").Find(&rs)
	return rs
}

func newsSpec(llm bool) json.RawMessage {
	mode := "template"
	if llm {
		mode = "llm"
	}
	return json.RawMessage(`{
	  "schema_version": "automation/v1",
	  "name": "Noticias de seguridad en Zona Norte",
	  "trigger": {"type": "cron", "cron": "0 7 * * *", "timezone": "America/Mexico_City"},
	  "steps": [
	    {"id": "buscar", "type": "tool", "tool": "buscar_hallazgos", "args": {"territorio": "Zona Norte", "texto": "seguridad", "limite": 10}},
	    {"id": "nuevos", "type": "filter", "from": "buscar.items", "unseen": true},
	    {"id": "hay", "type": "condition", "if": {"field": "nuevos.count", "op": "gt", "value": 0}},
	    {"id": "texto", "type": "summarize", "from": "nuevos.items", "mode": "` + mode + `", "template": "{{count}} hallazgos nuevos:\n{{lista}}"},
	    {"id": "enviar", "type": "deliver", "channels": ["in_app", "whatsapp"], "title": "Seguridad · Zona Norte", "body": "{{texto.text}}", "items_from": "nuevos.items", "link": "/m/findings"}
	  ]
	}`)
}

func item(id, title string) map[string]any {
	return map[string]any{"id": id, "titulo": title, "territorio": "Zona Norte", "evidencia": map[string]any{"url": "https://ejemplo.mx/" + id, "fecha": "2026-10-12"}}
}

// ---- tests ---------------------------------------------------------------------

func TestSchemaRejectsArbitraryExecution(t *testing.T) {
	h := newHarness(t, sqliteDB(t), nil)
	ctx := context.Background()
	bad := []string{
		`{"schema_version":"automation/v1","name":"x","trigger":{"type":"cron","cron":"0 7 * * *"},"steps":[{"id":"h","type":"http","url":"http://169.254.169.254"}]}`,
		`{"schema_version":"automation/v1","name":"x","trigger":{"type":"cron","cron":"0 7 * * *"},"steps":[{"id":"s","type":"tool","tool":"database.query","args":{"sql":"DROP TABLE users"}}]}`,
		`{"schema_version":"automation/v1","name":"x","trigger":{"type":"cron","cron":"0 7 * * *"},"steps":[{"id":"d","type":"deliver","channels":["in_app"],"title":"t","script":"alert(1)"}]}`,
		`{"schema_version":"automation/v2","name":"x","steps":[{"id":"d","type":"deliver","channels":["in_app"],"title":"t"}]}`,
		`{"schema_version":"automation/v1","name":"x","trigger":{"type":"cron","cron":"*/5 * * * *"},"steps":[{"id":"d","type":"deliver","channels":["in_app"],"title":"t"}]}`,
		`{"schema_version":"automation/v1","name":"x","trigger":{"type":"cron","cron":"0 7 * * *"},"steps":[{"id":"d","type":"deliver","channels":["sms"],"title":"t"}]}`,
		`{"schema_version":"automation/v1","name":"x","trigger":{"type":"cron","cron":"0 7 * * *"},"steps":[{"id":"d","type":"deliver","channels":["in_app"],"title":"t","link":"https://phishing.example"}]}`,
		`{"schema_version":"automation/v1","name":"x","trigger":{"type":"cron","cron":"0 7 * * *"},"steps":[{"id":"f","type":"filter","from":"nadie.items"},{"id":"d","type":"deliver","channels":["in_app"],"title":"t"}]}`,
	}
	for i, raw := range bad {
		if _, err := h.svc.Create(ctx, h.id(), CreateInput{Spec: json.RawMessage(raw), Origin: OriginUI}); !errors.Is(err, ErrInvalid) {
			t.Errorf("spec %d: want ErrInvalid, got %v", i, err)
		}
	}
}

func TestProposalLifecycleAndCronSchedule(t *testing.T) {
	h := newHarness(t, sqliteDB(t), nil)
	ctx := context.Background()
	a, err := h.svc.Create(ctx, h.id(), CreateInput{Spec: newsSpec(false), Origin: OriginChat, Assisted: true})
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != StatusProposed || a.NextRunAt != nil || a.AssistedBy != "Asistente" || a.AuthorizedBy != nil {
		t.Fatalf("agent creation must be an unconfirmed proposal: %+v", a)
	}
	if n, _ := h.svc.Tick(ctx); n != 0 {
		t.Fatalf("a proposal must never be scheduled, enqueued %d", n)
	}
	// Somebody else cannot confirm it.
	other := Identity{OrganizationID: h.org, UserID: uuid.New()}
	if _, err := h.svc.Confirm(ctx, other, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign confirm: %v", err)
	}
	a, err = h.svc.Confirm(ctx, h.id(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 10, 13, 7, 0, 0, 0, mx)
	if a.Status != StatusActive || a.NextRunAt == nil || !a.NextRunAt.Equal(want) || a.AuthorizedBy == nil || *a.AuthorizedBy != h.ana {
		t.Fatalf("confirmed: %+v next=%v want %v", a, a.NextRunAt, want)
	}
	v := h.svc.ViewOf(ctx, a)
	if !strings.Contains(v.Authorship, "con Asistente") || !strings.HasPrefix(v.TriggerLabel, "Cada día a las 07:00") {
		t.Fatalf("view: %q / %q", v.Authorship, v.TriggerLabel)
	}
	if err := h.svc.CardUI(v, nil).Validate(); err != nil {
		t.Fatalf("card ui: %v", err)
	}
}

func TestScheduledRunDedupesItemsAndSkipsWithoutCost(t *testing.T) {
	scheduledRun(t, sqliteDB(t))
}

func TestScheduledRunPostgres(t *testing.T) {
	scheduledRun(t, pgDB(t))
}

func scheduledRun(t *testing.T, db *gorm.DB) {
	h := newHarness(t, db, nil)
	ctx := context.Background()
	a, err := h.svc.Create(ctx, h.id(), CreateInput{Spec: newsSpec(false), Origin: OriginUI})
	if err != nil {
		t.Fatal(err)
	}
	h.cat.set(item("f1", "Robo en el centro"), item("f2", "Operativo en el puerto"))

	h.clk.Set(time.Date(2026, 10, 13, 6, 59, 0, 0, mx))
	if n, _ := h.svc.Tick(ctx); n != 0 {
		t.Fatalf("06:59 enqueued %d", n)
	}
	h.clk.Set(time.Date(2026, 10, 13, 7, 0, 10, 0, mx))
	if n, _ := h.svc.Tick(ctx); n != 1 {
		t.Fatalf("07:00 enqueued %d", n)
	}
	if n, _ := h.svc.Tick(ctx); n != 0 {
		t.Fatalf("second tick enqueued %d (double fire)", n)
	}
	h.drain()
	if h.del.count() != 1 {
		t.Fatalf("deliveries %d", h.del.count())
	}
	d := h.del.last()
	if len(d.Items) != 2 || d.UserID != h.ana || !strings.Contains(d.Body, "2 hallazgos nuevos") {
		t.Fatalf("delivery: %+v", d)
	}
	got := h.get(a.ID)
	if got.LastStatus != RunCompleted || got.NextRunAt == nil || !got.NextRunAt.Equal(time.Date(2026, 10, 14, 7, 0, 0, 0, mx)) {
		t.Fatalf("after run: %+v", got)
	}
	// Re-processing the same slot (outbox redelivery) never runs twice.
	if _, err := h.svc.execute(ctx, runJob{AutomationID: a.ID, Slot: time.Date(2026, 10, 13, 7, 0, 0, 0, mx).UTC().Format(time.RFC3339), Trigger: TriggerCron}, 2, false); err != nil {
		t.Fatal(err)
	}
	if h.del.count() != 1 {
		t.Fatalf("slot ran twice: %d deliveries", h.del.count())
	}

	// Next day: one new item, the old ones are not repeated.
	h.cat.set(item("f1", "Robo en el centro"), item("f2", "Operativo en el puerto"), item("f3", "Detenidos en Santiago"))
	h.clk.Set(time.Date(2026, 10, 14, 7, 0, 5, 0, mx))
	h.svc.Tick(ctx)
	h.drain()
	if h.del.count() != 2 || len(h.del.last().Items) != 1 || h.del.last().Items[0].Key != "f3" {
		t.Fatalf("day 2 delivery: %+v", h.del.last())
	}
	// Day 3: nothing new → skipped, nothing sent.
	h.clk.Set(time.Date(2026, 10, 15, 7, 0, 5, 0, mx))
	h.svc.Tick(ctx)
	h.drain()
	rs := h.runs(a.ID)
	if h.del.count() != 2 || rs[len(rs)-1].Status != RunSkipped {
		t.Fatalf("day 3: deliveries=%d last=%s", h.del.count(), rs[len(rs)-1].Status)
	}
}

func TestMissedSlotsCollapseAfterDowntime(t *testing.T) {
	h := newHarness(t, sqliteDB(t), nil)
	ctx := context.Background()
	a, _ := h.svc.Create(ctx, h.id(), CreateInput{Spec: newsSpec(false), Origin: OriginUI})
	h.cat.set(item("f1", "x"))
	h.clk.Set(time.Date(2026, 10, 16, 10, 0, 0, 0, mx)) // down for three days
	if n, _ := h.svc.Tick(ctx); n != 1 {
		t.Fatalf("enqueued %d, want 1", n)
	}
	if n, _ := h.svc.Tick(ctx); n != 0 {
		t.Fatalf("catch-up storm: %d", n)
	}
	if next := h.get(a.ID).NextRunAt; next == nil || !next.Equal(time.Date(2026, 10, 17, 7, 0, 0, 0, mx)) {
		t.Fatalf("next %v", next)
	}
}

func TestPersonalReminderRunsOnceAndCompletes(t *testing.T) {
	h := newHarness(t, sqliteDB(t), nil)
	ctx := context.Background()
	spec := `{"schema_version":"automation/v1","name":"Llamar al alcalde","trigger":{"type":"once","at":"2026-10-14T09:00","timezone":"America/Mexico_City"},
	  "steps":[{"id":"aviso","type":"deliver","channels":["in_app","push"],"title":"Recordatorio","body":"Llamar al alcalde de Zona Norte"}]}`
	a, err := h.svc.Create(ctx, h.id(), CreateInput{Spec: json.RawMessage(spec), Origin: OriginChat, Assisted: true})
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != StatusActive || a.AuthorizedBy == nil || a.AssistedBy != "Asistente" {
		t.Fatalf("trivial personal reminder should start active (persona con Asistente): %+v", a)
	}
	h.clk.Set(time.Date(2026, 10, 14, 9, 0, 30, 0, mx))
	h.svc.Tick(ctx)
	h.drain()
	h.svc.Tick(ctx)
	h.drain()
	if h.del.count() != 1 || h.del.last().Body != "Llamar al alcalde de Zona Norte" {
		t.Fatalf("reminder deliveries=%d", h.del.count())
	}
	if got := h.get(a.ID); got.Status != StatusCompleted || got.NextRunAt != nil {
		t.Fatalf("one-shot should complete: %+v", got)
	}

	// With the policy off the same request is a proposal.
	h2 := newHarness(t, sqliteDB(t), func(p *Policy) { p.AutoApprovePersonalReminders = false })
	b, err := h2.svc.Create(ctx, h2.id(), CreateInput{Spec: json.RawMessage(spec), Origin: OriginChat, Assisted: true})
	if err != nil || b.Status != StatusProposed {
		t.Fatalf("policy off: %v %+v", err, b)
	}
}

func TestFailuresAutoPauseAndPermissionLossIsPermanent(t *testing.T) {
	h := newHarness(t, sqliteDB(t), nil)
	ctx := context.Background()
	spec := strings.Replace(string(newsSpec(false)), `"cron": "0 7 * * *"`, `"cron": "0 */2 * * *"`, 1)
	a, err := h.svc.Create(ctx, h.id(), CreateInput{Spec: json.RawMessage(spec), Origin: OriginUI})
	if err != nil {
		t.Fatal(err)
	}
	h.cat.fail = errors.New("timeout de base de datos")
	for i := 0; i < 3; i++ {
		h.clk.Set(h.get(a.ID).NextRunAt.Add(time.Second))
		h.svc.Tick(ctx)
		h.drain() // three attempts each (outbox retries), then final failure
	}
	got := h.get(a.ID)
	if got.Status != StatusError || !strings.Contains(got.StatusReason, "3 fallos seguidos") {
		t.Fatalf("auto-pause: %+v", got)
	}
	// Resume resets the streak; losing the permission stops it immediately.
	h.cat.fail = nil
	if _, err := h.svc.Resume(ctx, h.id(), a.ID); err != nil {
		t.Fatal(err)
	}
	h.cat.forbidden = true
	h.clk.Set(h.get(a.ID).NextRunAt.Add(time.Second))
	h.svc.Tick(ctx)
	h.drain()
	if got := h.get(a.ID); got.Status != StatusError || !strings.Contains(got.StatusReason, "ya no está permitida") {
		t.Fatalf("permission loss: %+v", got)
	}
}

func TestEventTriggerFiltersDedupesAndCoolsDown(t *testing.T) {
	h := newHarness(t, sqliteDB(t), nil)
	ctx := context.Background()
	spec := `{"schema_version":"automation/v1","name":"Tema que se dispara en Zona Norte",
	  "trigger":{"type":"event","event":"support.ticket.opened","where":[{"field":"territorio","op":"eq","value":"Zona Norte"},{"field":"severidad","op":"in","value":["high","critical"]}],"cooldown":"30m"},
	  "steps":[{"id":"aviso","type":"deliver","channels":["in_app","push"],"title":"Se disparó un tema en Zona Norte","body":"{{event.titulo}}","link":"/m/alerts"}]}`
	a, err := h.svc.Create(ctx, h.id(), CreateInput{Spec: json.RawMessage(spec), Origin: OriginUI})
	if err != nil {
		t.Fatal(err)
	}
	ev := func(id, terr, sev string) Event {
		return Event{ID: id, OrganizationID: h.org, Name: "support.ticket.opened", Payload: map[string]any{"territorio": terr, "severidad": sev, "titulo": "Bloqueo en la carretera " + id}}
	}
	// Through the transactional outbox.
	if err := h.db.Transaction(func(tx *gorm.DB) error { return h.svc.EnqueueEventTx(tx, ev("e1", "Zona Norte", "high")) }); err != nil {
		t.Fatal(err)
	}
	h.drain()
	if h.del.count() != 1 || h.del.last().Body != "Bloqueo en la carretera e1" {
		t.Fatalf("event delivery: %d", h.del.count())
	}
	if n, _ := h.svc.Ingest(ctx, ev("e1", "Zona Norte", "high")); n != 0 {
		t.Fatalf("same event fired twice")
	}
	if n, _ := h.svc.Ingest(ctx, ev("e2", "Zona Sur", "critical")); n != 0 {
		t.Fatalf("other territory matched")
	}
	if n, _ := h.svc.Ingest(ctx, ev("e3", "Zona Norte", "low")); n != 0 {
		t.Fatalf("low severity matched")
	}
	if n, _ := h.svc.Ingest(ctx, ev("e4", "Zona Norte", "critical")); n != 0 {
		t.Fatalf("cooldown ignored")
	}
	h.clk.Set(h.clk.Now().Add(31 * time.Minute))
	if n, _ := h.svc.Ingest(ctx, ev("e5", "Zona Norte", "critical")); n != 1 {
		t.Fatalf("after cooldown: %d", n)
	}
	h.drain()
	if h.del.count() != 2 {
		t.Fatalf("deliveries %d", h.del.count())
	}
	_ = a
}

func TestLLMSummaryIsCachedBudgetedAndSkippedInDryRun(t *testing.T) {
	h := newHarness(t, sqliteDB(t), func(p *Policy) { p.DailyBudgetUSD = 1; p.DefaultModel = "barato" })
	ctx := context.Background()
	a, err := h.svc.Create(ctx, h.id(), CreateInput{Spec: newsSpec(true), Origin: OriginUI})
	if err != nil {
		t.Fatal(err)
	}
	h.cat.set(item("f1", "a"), item("f2", "b"))
	// Dry-run: no tokens, no delivery, no dedupe marks.
	run, out, err := h.svc.RunNow(ctx, h.id(), a.ID, true)
	if err != nil || !run.DryRun || h.llm.calls != 0 || h.del.count() != 0 || out == nil || len(out.Items) != 2 {
		t.Fatalf("dry run: err=%v run=%+v llm=%d del=%d out=%+v", err, run, h.llm.calls, h.del.count(), out)
	}
	var marks int64
	h.db.Model(&DeliveryRecord{}).Count(&marks)
	if marks != 0 {
		t.Fatalf("dry run marked %d items", marks)
	}
	h.clk.Set(time.Date(2026, 10, 13, 7, 0, 1, 0, mx))
	h.svc.Tick(ctx)
	h.drain()
	if h.llm.calls != 1 || h.del.last().Body != "Resumen IA de 2 hallazgos" {
		t.Fatalf("llm run: calls=%d body=%q", h.llm.calls, h.del.last().Body)
	}
	rs := h.runs(a.ID)
	last := rs[len(rs)-1]
	if !last.LLMUsed || last.CostUSD != 0.001 || last.InputTokens != 100 {
		t.Fatalf("usage not recorded: %+v", last)
	}
	// Same input on a manual real run → cache hit, no new model call.
	h.db.Where("automation_id = ?", a.ID).Delete(&DeliveryRecord{})
	run2, _, err := h.svc.RunNow(ctx, h.id(), a.ID, false)
	if err != nil || !run2.CacheHit || h.llm.calls != 1 {
		t.Fatalf("cache: err=%v hit=%v calls=%d", err, run2.CacheHit, h.llm.calls)
	}

	// No budget → deterministic template with a note.
	h3 := newHarness(t, sqliteDB(t), func(p *Policy) { p.DailyBudgetUSD = 0 })
	b, _ := h3.svc.Create(ctx, h3.id(), CreateInput{Spec: newsSpec(true), Origin: OriginUI})
	h3.cat.set(item("f9", "z"))
	_, out3, err := h3.svc.RunNow(ctx, h3.id(), b.ID, false)
	if err != nil || h3.llm.calls != 0 || !strings.Contains(out3.Text, "1 hallazgos nuevos") || len(out3.Notes) == 0 {
		t.Fatalf("no budget: err=%v calls=%d out=%+v", err, h3.llm.calls, out3)
	}
}

func TestLimitsPerUser(t *testing.T) {
	h := newHarness(t, sqliteDB(t), func(p *Policy) { p.MaxPerUser = 2 })
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := h.svc.Create(ctx, h.id(), CreateInput{Spec: newsSpec(false), Origin: OriginUI}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.svc.Create(ctx, h.id(), CreateInput{Spec: newsSpec(false), Origin: OriginUI}); !errors.Is(err, ErrLimit) {
		t.Fatalf("want ErrLimit, got %v", err)
	}
}

func TestDynamicToolAndAgentTools(t *testing.T) {
	h := newHarness(t, sqliteDB(t), nil)
	ctx := context.Background()
	h.cat.set(item("f1", "Robo"), map[string]any{"id": "f2", "titulo": "Bache", "territorio": "Zona Sur", "evidencia": map[string]any{}})
	toolSpec := `{"schema_version":"automation/v1","name":"Hallazgos por municipio","description":"Hallazgos recientes de un municipio",
	  "inputs":[{"name":"municipio","type":"string","required":true}],
	  "steps":[{"id":"buscar","type":"tool","tool":"buscar_hallazgos","args":{"territorio":"{{inputs.municipio}}","limite":5}},
	           {"id":"lista","type":"filter","from":"buscar.items","limit":5}]}`
	agentCtx := &contextmgr.ConversationContext{OrganizationID: h.org, UserID: h.ana, ConversationID: uuid.New()}
	var create tools.BaseTool
	for _, tl := range h.svc.AgentTools(AgentToolsConfig{ToolsHint: "buscar_hallazgos"}) {
		if tl.Name() == "crear_automatizacion" {
			create = tl
		}
	}
	var specObj map[string]any
	_ = json.Unmarshal([]byte(toolSpec), &specObj)
	res, err := create.Execute(ctx, map[string]any{"spec": specObj, "tipo": "herramienta"}, agentCtx)
	if err != nil {
		t.Fatal(err)
	}
	m := res.(map[string]any)
	if m["ok"] != true {
		t.Fatalf("create tool: %+v", m)
	}
	spec, err := genui.FromAny(m["ui"])
	if err != nil || spec.Blocks[0].Type != genui.BlockConfirm {
		t.Fatalf("proposal ui: %v %+v", err, spec)
	}
	formatted := tools.ToolResult{ToolName: "crear_automatizacion", Success: true, Result: res}.FormatForLLM()
	if strings.Contains(formatted, "automations.confirm") {
		t.Fatalf("ui spec leaked to the model: %s", formatted)
	}
	// Not callable until a person confirms it.
	if len(h.svc.DynamicTools(ctx, h.org)) != 0 {
		t.Fatal("proposed tool exposed")
	}
	auto := m["automatizacion"].(map[string]any)
	autoID := uuid.MustParse(auto["id"].(string))
	if _, err := h.svc.Confirm(ctx, h.id(), autoID); err != nil {
		t.Fatal(err)
	}
	dyn := h.svc.DynamicTools(ctx, h.org)
	if len(dyn) != 1 || dyn[0].Name() != "dyn_hallazgos_por_municipio" {
		t.Fatalf("dynamic tools: %v", dyn)
	}
	if _, req := dyn[0].Parameters()["municipio"].(map[string]any)["required"]; !req {
		t.Fatal("input schema not exposed")
	}
	out, err := dyn[0].Execute(ctx, map[string]any{"municipio": "Zona Norte"}, agentCtx)
	if err != nil {
		t.Fatal(err)
	}
	r := out.(map[string]any)["resultado"].(map[string]any)
	if r["count"] != 1 {
		t.Fatalf("dynamic tool result: %+v", r)
	}
	// Missing required input.
	out, _ = dyn[0].Execute(ctx, map[string]any{}, agentCtx)
	if out.(map[string]any)["ok"] != false {
		t.Fatalf("missing input accepted: %+v", out)
	}
	// A dynamic tool cannot deliver notifications.
	bad := strings.Replace(toolSpec, `{"id":"lista","type":"filter","from":"buscar.items","limit":5}`, `{"id":"enviar","type":"deliver","channels":["in_app"],"title":"x"}`, 1)
	if _, err := h.svc.Create(ctx, h.id(), CreateInput{Kind: KindTool, Spec: json.RawMessage(bad), Origin: OriginUI}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("tool with deliver: %v", err)
	}
}

func TestAgentEditReturnsToProposal(t *testing.T) {
	h := newHarness(t, sqliteDB(t), nil)
	ctx := context.Background()
	a, _ := h.svc.Create(ctx, h.id(), CreateInput{Spec: newsSpec(false), Origin: OriginUI})
	edited := strings.Replace(string(newsSpec(false)), "0 7 * * *", "30 8 * * 1-5", 1)
	b, err := h.svc.Update(ctx, h.id(), a.ID, UpdateInput{Spec: json.RawMessage(edited), Assisted: true})
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != StatusProposed || b.NextRunAt != nil || b.AuthorizedBy != nil || b.Version != 2 {
		t.Fatalf("agent edit must require confirmation: %+v", b)
	}
	if _, err := h.svc.Update(ctx, h.id(), a.ID, UpdateInput{Spec: json.RawMessage(edited), ExpectedVersion: 1}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale version: %v", err)
	}
	c, err := h.svc.Confirm(ctx, h.id(), a.ID)
	if err != nil || c.NextRunAt == nil || c.NextRunAt.In(mx).Hour() != 8 {
		t.Fatalf("confirm edited: %v %+v", err, c)
	}
}

// TestConcurrentSchedulersPostgres runs two scheduler instances against the
// same PostgreSQL database: each due slot must be enqueued exactly once.
func TestConcurrentSchedulersPostgres(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set")
	}
	open := pgOpener(t, dsn)
	h1 := newHarness(t, open(), nil)
	h2 := newHarness(t, open(), nil)
	h2.org, h2.ana = h1.org, h1.ana
	ctx := context.Background()
	const autos = 20
	for i := 0; i < autos; i++ {
		if _, err := h1.svc.Create(ctx, h1.id(), CreateInput{Spec: newsSpec(false), Origin: OriginUI}); err != nil {
			t.Fatal(err)
		}
	}
	at := time.Date(2026, 10, 13, 7, 0, 1, 0, mx)
	h1.clk.Set(at)
	h2.clk.Set(at)
	var wg sync.WaitGroup
	var mu sync.Mutex
	total := 0
	for _, h := range []*harness{h1, h2, h1, h2} {
		wg.Add(1)
		go func(h *harness) {
			defer wg.Done()
			n, err := h.svc.Tick(ctx)
			if err != nil {
				t.Error(err)
			}
			mu.Lock()
			total += n
			mu.Unlock()
		}(h)
	}
	wg.Wait()
	var jobs int64
	h1.db.Raw("SELECT COUNT(*) FROM metacore_outbox_jobs WHERE kind = ?", JobRun).Scan(&jobs)
	if total != autos || jobs != autos {
		t.Fatalf("enqueued total=%d jobs=%d, want %d each", total, jobs, autos)
	}
}

func pgDB(t *testing.T) *gorm.DB {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set")
	}
	return pgOpener(t, dsn)()
}

// pgOpener creates a throwaway schema and returns a function that opens pools
// bound to it.
func pgOpener(t *testing.T, dsn string) func() *gorm.DB {
	cfg := &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)}
	admin, err := gorm.Open(postgres.Open(dsn), cfg)
	if err != nil {
		t.Fatal(err)
	}
	schema := "automation_test_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	if err := admin.Exec("CREATE SCHEMA " + schema).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec("DROP SCHEMA " + schema + " CASCADE") })
	return func() *gorm.DB {
		db, err := gorm.Open(postgres.Open(dsn+" search_path="+schema), cfg)
		if err != nil {
			t.Fatal(err)
		}
		return db
	}
}
