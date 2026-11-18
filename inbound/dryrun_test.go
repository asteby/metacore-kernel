package inbound

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/metadata"
	"github.com/asteby/metacore-kernel/modelbase"
)

// Dry-run (shadow) mode against the real dynamic.Service, on SQLite (CGO) and
// on Postgres (TEST_POSTGRES_DSN, the CI job "postgres"): a dry run never
// changes a row nor publishes an event, reports what the event WOULD do, and
// applying the same event for real afterwards still works.

const dryModel = "inbound_dry_orders"

type dryOrder struct {
	ID             uuid.UUID      `json:"id" gorm:"type:uuid;primaryKey"`
	OrganizationID uuid.UUID      `json:"organization_id" gorm:"type:uuid;index"`
	CreatedByID    *uuid.UUID     `json:"created_by_id,omitempty" gorm:"type:uuid"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	DeletedAt      gorm.DeletedAt `json:"deleted_at,omitempty" gorm:"index"`
	Reference      string         `json:"reference" gorm:"size:64"`
	Status         string         `json:"status" gorm:"size:32"`
	Total          int            `json:"total"`
	Email          string         `json:"email" gorm:"size:128"`
	SourceAt       *time.Time     `json:"source_at"`
	Idem           string         `json:"idem" gorm:"size:64"`
}

func (o *dryOrder) BeforeCreate(*gorm.DB) error {
	if o.ID == uuid.Nil {
		o.ID = uuid.New()
	}
	return nil
}

func (dryOrder) TableName() string { return dryModel }
func (dryOrder) DefineTable() modelbase.TableMetadata {
	return modelbase.TableMetadata{
		Title: "Dry orders",
		Columns: []modelbase.ColumnDef{
			{Key: "reference", Label: "Reference", Type: "text"},
			{Key: "status", Label: "Status", Type: "text", Options: []modelbase.OptionDef{
				{Value: "new", Label: "New"}, {Value: "paid", Label: "Paid"}, {Value: "shipped", Label: "Shipped"}}},
			{Key: "total", Label: "Total", Type: "number"},
			{Key: "email", Label: "Email", Type: "text"},
			{Key: "source_at", Label: "Source at", Type: "datetime"},
			{Key: "idem", Label: "Idem", Type: "text"},
		},
		SearchColumns: []string{"reference"},
	}
}
func (dryOrder) DefineModal() modelbase.ModalMetadata {
	return modelbase.ModalMetadata{Title: "Dry orders"}
}

const (
	drySQLite = `CREATE TABLE inbound_dry_orders (id TEXT PRIMARY KEY, organization_id TEXT, created_by_id TEXT,
		created_at DATETIME, updated_at DATETIME, deleted_at DATETIME, reference TEXT, status TEXT, total INTEGER,
		email TEXT, source_at DATETIME, idem TEXT)`
	dryPG = `CREATE TABLE inbound_dry_orders (id uuid PRIMARY KEY, organization_id uuid, created_by_id uuid,
		created_at timestamptz, updated_at timestamptz, deleted_at timestamptz, reference text, status text, total integer,
		email text, source_at timestamptz, idem text)`
)

const drySchema = `{"type":"object","required":["event_id","ref"],"properties":{
 "event_id":{"type":"string"},"ref":{"type":"string"},"total":{"type":["integer","string"]},
 "email":{"type":"string"},"at":{"type":"string"}}}`

// dryEnv is one database, one real service and (after setup) one Handler served
// over HTTP.
type dryEnv struct {
	t     *testing.T
	db    *gorm.DB
	svc   *dynamic.Service
	bus   *atomBus
	ex    *DynamicExecutor
	org   uuid.UUID
	store *MemoryReceiptStore
	em    *fakeEmitter
	nt    *fakeNotifier
	auto  *fakeAuto
	cmds  *CommandRegistry
	h     *Handler
	srv   *httptest.Server
}

func eachDryDB(t *testing.T, fn func(t *testing.T, e *dryEnv)) {
	t.Helper()
	run := func(t *testing.T, db *gorm.DB, ddl string) {
		t.Helper()
		if err := db.Exec(ddl).Error; err != nil {
			t.Fatal(err)
		}
		fn(t, newDryEnv(t, db))
	}
	t.Run("SQLite", func(t *testing.T) {
		db := openSQLite(t)
		if sqlDB, err := db.DB(); err == nil {
			sqlDB.SetMaxOpenConns(4)
		}
		run(t, db, drySQLite)
	})
	t.Run("Postgres", func(t *testing.T) { run(t, postgresDB(t), dryPG) })
}

func newDryEnv(t *testing.T, db *gorm.DB) *dryEnv {
	t.Helper()
	modelbase.Register(dryModel, func() modelbase.ModelDefiner { return &dryOrder{} })
	actions := map[string]*manifest.ActionDef{
		"mark_paid": {Key: "mark_paid", Ingest: true, RequiresState: []string{"new"}, Trigger: &manifest.ActionTrigger{Type: "noop"}},
	}
	sm := &dynamic.StageMachine{
		Field:  "status",
		Stages: []manifest.StageDef{{Key: "new"}, {Key: "paid"}, {Key: "shipped"}},
		Transitions: []manifest.TransitionDef{
			{From: "new", To: "paid"}, {From: "paid", To: "shipped"},
		},
	}
	bus := &atomBus{}
	svc := dynamic.New(dynamic.Config{
		AllowUnconfiguredPermissions: true, DB: db, Metadata: metadata.New(metadata.Config{CacheTTL: -1}),
		Bus: bus, DisableEventOutbox: true,
		ActionResolver: func(_ context.Context, model, key string) (*manifest.ActionDef, bool) {
			def, ok := actions[key]
			return def, ok && model == dryModel
		},
		StageMachineResolver: func(_ context.Context, model string) (*dynamic.StageMachine, bool) {
			return sm, model == dryModel
		},
	})
	tomb := GormTombstones{DB: db, Table: func(string) string { return dryModel }}
	return &dryEnv{t: t, db: db, svc: svc, bus: bus, org: uuid.New(), store: NewMemoryReceiptStore(),
		em: &fakeEmitter{}, nt: &fakeNotifier{}, auto: &fakeAuto{}, cmds: NewCommandRegistry(),
		ex: &DynamicExecutor{Service: svc, Tombstones: tomb}}
}

// setup registers the contracts and serves the Handler (the verifier accepts
// everything: signatures are not what is under test here).
func (e *dryEnv) setup(cs ...Contract) {
	e.t.Helper()
	reg := NewRegistry()
	for _, c := range cs {
		if err := reg.Register(c); err != nil {
			e.t.Fatal(err)
		}
	}
	h, err := New(Config{
		Contracts: reg, Receipts: e.store, Executor: e.ex, Commands: e.cmds,
		Emitter: e.em, Notifier: e.nt, Automation: e.auto, Logger: quietLogger(),
		Verifiers: map[string]Verifier{ProfileHMACCanonicalV1: VerifierFunc(func(context.Context, VerifyRequest) error { return nil })},
	})
	if err != nil {
		e.t.Fatal(err)
	}
	e.h = h
	e.srv = httptest.NewServer(h)
	e.t.Cleanup(e.srv.Close)
}

type dryResp struct {
	code int
	body map[string]any
	raw  string
	hdr  http.Header
}

// send posts an event; version 0 = the highest registered.
func (e *dryEnv) send(key string, version int, body string) dryResp {
	e.t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.srv.URL+"/api/inbound/"+e.org.String()+"/"+key, strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if version > 0 {
		req.Header.Set("X-Contract-Version", strconv.Itoa(version))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return dryResp{code: resp.StatusCode, body: out, raw: string(raw), hdr: resp.Header}
}

func dryBody(id, ref string, total any, at string) string {
	b, _ := json.Marshal(map[string]any{"event_id": id, "ref": ref, "total": total, "email": "ana@example.com", "at": at})
	return string(b)
}

func refBody(id, ref string) string {
	b, _ := json.Marshal(map[string]any{"event_id": id, "ref": ref, "at": atomAt(5)})
	return string(b)
}

func stepsOf(m map[string]any) []map[string]any {
	raw, _ := m["actions"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, x := range raw {
		out = append(out, x.(map[string]any))
	}
	return out
}

func changeOf(t *testing.T, step map[string]any, col string) map[string]any {
	t.Helper()
	ch, _ := step["changes"].(map[string]any)
	c, ok := ch[col].(map[string]any)
	if !ok {
		t.Fatalf("no change reported for %q: %v", col, step["changes"])
	}
	return c
}

func dryContract(key string, version int, dry bool, acts ...Action) Contract {
	return Contract{Key: key, Version: version, JSONSchema: json.RawMessage(drySchema), Idempotency: "{event_id}",
		Auth: ProfileHMACCanonicalV1, Actions: acts, DryRun: dry}
}

func upsertAct() Action {
	return Action{Do: ActionRecordUpsert, Model: dryModel, Match: []string{"reference"},
		Mapping: map[string]string{"reference": "{{payload.ref}}", "total": "{{payload.total}}",
			"email": "{{payload.email}}", "source_at": "{{payload.at}}"},
		Guard: &Guard{Field: "source_at", Path: "payload.at", Op: GuardGT}}
}

func selectAct(do string) Action {
	return Action{Do: do, Model: dryModel, Match: []string{"reference"}, Mapping: map[string]string{"reference": "{{payload.ref}}"}}
}

func (e *dryEnv) seed(ref, status string, total int, at string) uuid.UUID {
	e.t.Helper()
	in := map[string]any{"reference": ref, "status": status, "total": total}
	if at != "" {
		in["source_at"] = at
	}
	row, err := e.svc.Create(context.Background(), dryModel, dynamic.NewSystemCaller(e.org), in)
	if err != nil {
		e.t.Fatal(err)
	}
	id, err := uuid.Parse(row["id"].(string))
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

// rows reads every row of the reference of this organization, deleted ones included.
func (e *dryEnv) rows(ref string) []dryOrder {
	e.t.Helper()
	var out []dryOrder
	if err := e.db.Unscoped().Where("organization_id = ? AND reference = ?", e.org, ref).Find(&out).Error; err != nil {
		e.t.Fatal(err)
	}
	return out
}

func (e *dryEnv) live(ref string) int {
	e.t.Helper()
	var n int64
	if err := e.db.Model(&dryOrder{}).Where("organization_id = ? AND reference = ?", e.org, ref).Count(&n).Error; err != nil {
		e.t.Fatal(err)
	}
	return int(n)
}

func (e *dryEnv) events() int { return e.bus.count("") }

// 1. upsert: create vs update, stale, replay; nothing written, nothing published.
func TestDryRunUpsertReportsAndWritesNothing(t *testing.T) {
	eachDryDB(t, func(t *testing.T, e *dryEnv) {
		e.setup(dryContract("dry.up", 1, true, upsertAct()))
		before := e.events()

		r := e.send("dry.up", 0, dryBody("e1", "A", 100, atomAt(2)))
		if r.code != 200 || r.body["status"] != StatusWouldApply || r.body["shadow"] != true || r.hdr.Get(ShadowHeader) != "true" {
			t.Fatalf("first event: %d %s", r.code, r.raw)
		}
		st := stepsOf(r.body)[0]
		if st["would"] != "upsert-create" || st["created"] != true || st["do"] != ActionRecordUpsert {
			t.Fatalf("step: %v", st)
		}
		if changeOf(t, st, "total")["to"] != float64(100) {
			t.Fatalf("total change: %v", st["changes"])
		}
		// A personal column is named, never valued.
		if em := changeOf(t, st, "email"); em["redacted"] != true || em["to"] != nil || em["from"] != nil {
			t.Fatalf("email must be redacted: %v", em)
		}
		if strings.Contains(r.raw, "ana@example.com") {
			t.Fatalf("a personal value leaked into the report: %s", r.raw)
		}
		if n := len(e.rows("A")); n != 0 {
			t.Fatalf("dry run created %d rows", n)
		}

		id := e.seed("A", "new", 50, atomAt(0))
		before = e.events()
		r = e.send("dry.up", 0, dryBody("e2", "A", 100, atomAt(2)))
		st = stepsOf(r.body)[0]
		if r.code != 200 || st["would"] != "upsert-update" || st["created"] == true || st["record_id"] != id.String() {
			t.Fatalf("update event: %d %s", r.code, r.raw)
		}
		if c := changeOf(t, st, "total"); c["from"] != float64(50) || c["to"] != float64(100) {
			t.Fatalf("total change: %v", c)
		}
		if rows := e.rows("A"); len(rows) != 1 || rows[0].Total != 50 {
			t.Fatalf("dry run changed the row: %+v", rows)
		}

		// The order guard judges the same way: an event not newer than the row is stale.
		r = e.send("dry.up", 0, dryBody("e3", "A", 999, atomAt(0)))
		st = stepsOf(r.body)[0]
		if r.code != 200 || st["status"] != StepSkipped || st["error_code"] != CodeStaleEvent {
			t.Fatalf("stale event: %d %s", r.code, r.raw)
		}

		// Retrying the same event answers replayed with the same report.
		r = e.send("dry.up", 0, dryBody("e2", "A", 100, atomAt(2)))
		st = stepsOf(r.body)[0]
		if r.code != 200 || r.body["status"] != "replayed" || r.body["shadow"] != true || st["would"] != "upsert-update" {
			t.Fatalf("replay: %d %s", r.code, r.raw)
		}
		// The same key with another body is a conflict, as for a real event.
		if r = e.send("dry.up", 0, dryBody("e2", "A", 7, atomAt(2))); r.code != 409 || errCode(r.body) != CodeIdempotencyClash {
			t.Fatalf("same key, other body: %d %s", r.code, r.raw)
		}
		if e.events() != before {
			t.Fatalf("a dry run published %d events", e.events()-before)
		}
		recs, err := e.h.Receipts(context.Background(), e.org, ReceiptFilter{Contract: "dry.up"})
		if err != nil || len(recs) == 0 {
			t.Fatalf("receipts: %v %v", recs, err)
		}
		for _, rc := range recs {
			if !rc.Shadow {
				t.Fatalf("a dry-run receipt must be marked shadow: %+v", rc)
			}
		}
	})
}

// 2. The shadow reservation never stops the real application of the same event.
func TestDryRunDoesNotBlockTheRealApplication(t *testing.T) {
	eachDryDB(t, func(t *testing.T, e *dryEnv) {
		e.setup(dryContract("dry.up", 1, true, upsertAct()), dryContract("dry.up", 2, false, upsertAct()))
		body := dryBody("same", "B", 10, atomAt(1))
		if r := e.send("dry.up", 1, body); r.code != 200 || r.body["status"] != StatusWouldApply {
			t.Fatalf("shadow: %d %s", r.code, r.raw)
		}
		if n := len(e.rows("B")); n != 0 {
			t.Fatalf("shadow wrote %d rows", n)
		}
		r := e.send("dry.up", 2, body)
		if r.code != 200 || r.body["status"] != "applied" || r.body["shadow"] == true || r.hdr.Get(ShadowHeader) != "" {
			t.Fatalf("real run after the shadow one: %d %s", r.code, r.raw)
		}
		if rows := e.rows("B"); len(rows) != 1 || rows[0].Total != 10 {
			t.Fatalf("the event was not applied for real: %+v", rows)
		}
		if r = e.send("dry.up", 2, body); r.body["status"] != "replayed" || r.body["shadow"] == true {
			t.Fatalf("real replay: %s", r.raw)
		}
		yes, no := true, false
		ctx := context.Background()
		sh, err := e.h.Receipts(ctx, e.org, ReceiptFilter{Contract: "dry.up", Shadow: &yes})
		if err != nil || len(sh) != 1 || !sh[0].Shadow {
			t.Fatalf("shadow receipts: %+v %v", sh, err)
		}
		re, err := e.h.Receipts(ctx, e.org, ReceiptFilter{Contract: "dry.up", Shadow: &no})
		if err != nil || len(re) != 1 || re[0].Shadow || re[0].Status != StatusApplied {
			t.Fatalf("real receipts: %+v %v", re, err)
		}
		// The clear event key finds each receipt through its own key space.
		sh, err = e.h.Receipts(ctx, e.org, ReceiptFilter{Contract: "dry.up", EventKey: "same", Shadow: &yes})
		if err != nil || len(sh) != 1 || !sh[0].Shadow {
			t.Fatalf("shadow by event key: %+v %v", sh, err)
		}
		all, err := e.h.Receipts(ctx, e.org, ReceiptFilter{Contract: "dry.up", EventKey: "same"})
		if err != nil || len(all) != 2 {
			t.Fatalf("both by event key: %+v %v", all, err)
		}
	})
}

// 3. record.delete: reports the retirement, keeps the row; the guard and the
// match errors are the real ones.
func TestDryRunDelete(t *testing.T) {
	eachDryDB(t, func(t *testing.T, e *dryEnv) {
		del := selectAct(ActionRecordDelete)
		del.Guard = &Guard{Field: "source_at", Path: "payload.at", Op: GuardGT}
		e.setup(dryContract("dry.del", 1, true, del), dryContract("dry.del", 2, false, del))
		e.seed("D1", "new", 1, atomAt(1))
		before := e.events()

		newer := func(id, ref string, at int) string {
			b, _ := json.Marshal(map[string]any{"event_id": id, "ref": ref, "at": atomAt(at)})
			return string(b)
		}
		r := e.send("dry.del", 1, newer("d1", "D1", 3))
		st := stepsOf(r.body)[0]
		if r.code != 200 || st["would"] != "delete" || st["affected"] != float64(1) || st["rows"] != float64(1) {
			t.Fatalf("delete report: %d %s", r.code, r.raw)
		}
		if e.live("D1") != 1 {
			t.Fatal("a dry-run delete removed the row")
		}
		// An event older than the row is stale; nothing matches is a successful no-op.
		r = e.send("dry.del", 1, newer("d2", "D1", 0))
		if st = stepsOf(r.body)[0]; st["status"] != StepSkipped || st["error_code"] != CodeStaleEvent {
			t.Fatalf("stale delete: %s", r.raw)
		}
		r = e.send("dry.del", 1, newer("d3", "NOPE", 3))
		if st = stepsOf(r.body)[0]; r.code != 200 || st["not_found"] != true || st["would"] != "delete" {
			t.Fatalf("delete without a match: %s", r.raw)
		}
		if e.events() != before {
			t.Fatal("a dry run published events")
		}
		// The very same event, applied for real, retires the row.
		if r = e.send("dry.del", 2, newer("d1", "D1", 3)); r.code != 200 || r.body["status"] != "applied" {
			t.Fatalf("real delete: %s", r.raw)
		}
		if e.live("D1") != 0 {
			t.Fatal("the real delete did not apply")
		}
	})
}

// 4. record.restore.
func TestDryRunRestore(t *testing.T) {
	eachDryDB(t, func(t *testing.T, e *dryEnv) {
		e.setup(dryContract("dry.rest", 1, true, selectAct(ActionRecordRestore)),
			dryContract("dry.rest", 2, false, selectAct(ActionRecordRestore)))
		id := e.seed("R1", "new", 1, atomAt(1))
		if err := e.svc.Delete(context.Background(), dryModel, dynamic.NewSystemCaller(e.org), id); err != nil {
			t.Fatal(err)
		}
		before := e.events()
		r := e.send("dry.rest", 1, refBody("r1", "R1"))
		st := stepsOf(r.body)[0]
		if r.code != 200 || st["would"] != "restore" || st["rows"] != float64(1) {
			t.Fatalf("restore report: %d %s", r.code, r.raw)
		}
		if e.live("R1") != 0 || e.events() != before {
			t.Fatal("a dry-run restore wrote or published")
		}
		if r = e.send("dry.rest", 2, refBody("r1", "R1")); r.code != 200 || r.body["status"] != "applied" {
			t.Fatalf("real restore: %s", r.raw)
		}
		if e.live("R1") != 1 {
			t.Fatal("the real restore did not apply")
		}
	})
}

// 5. record.transition: valid, refused and already-there moves.
func TestDryRunTransition(t *testing.T) {
	eachDryDB(t, func(t *testing.T, e *dryEnv) {
		to := func(state string) Action { a := selectAct(ActionRecordTransition); a.To = state; return a }
		e.setup(dryContract("dry.tr.paid", 1, true, to("paid")), dryContract("dry.tr.ship", 1, true, to("shipped")))
		e.seed("T1", "new", 1, "")
		e.seed("T2", "paid", 1, "")
		before := e.events()

		r := e.send("dry.tr.paid", 0, refBody("t1", "T1"))
		st := stepsOf(r.body)[0]
		if r.code != 200 || st["would"] != "transition" || st["rows"] != float64(1) {
			t.Fatalf("valid transition: %d %s", r.code, r.raw)
		}
		if c := changeOf(t, st, "status"); c["from"] != "new" || c["to"] != "paid" {
			t.Fatalf("status change: %v", c)
		}
		if got := e.rows("T1")[0].Status; got != "new" {
			t.Fatalf("a dry-run transition moved the row to %q", got)
		}
		// new -> shipped is not allowed by the machine: the real 422.
		r = e.send("dry.tr.ship", 0, refBody("t2", "T1"))
		if r.code != 422 || errCode(r.body) != CodeInvalidTransition {
			t.Fatalf("invalid transition: %d %s", r.code, r.raw)
		}
		// Already in the target state: a no-op.
		r = e.send("dry.tr.paid", 0, refBody("t3", "T2"))
		st = stepsOf(r.body)[0]
		if r.code != 200 || st["noop"] != true || st["reason"] != CodeAlreadyInState {
			t.Fatalf("already in state: %d %s", r.code, r.raw)
		}
		if e.events() != before {
			t.Fatal("a dry run published events")
		}
	})
}

// 6. record.action: eligible, ineligible (noop and fail).
func TestDryRunAction(t *testing.T) {
	eachDryDB(t, func(t *testing.T, e *dryEnv) {
		act := selectAct(ActionRecordAction)
		act.ActionKey = "mark_paid"
		strict := act
		strict.OnIneligible = OnIneligibleFail
		e.setup(dryContract("dry.act", 1, true, act), dryContract("dry.act.strict", 1, true, strict))
		e.seed("N1", "new", 1, "")
		e.seed("N2", "paid", 1, "")
		before := e.events()

		r := e.send("dry.act", 0, refBody("a1", "N1"))
		st := stepsOf(r.body)[0]
		if r.code != 200 || st["would"] != "action" || st["target"] != "mark_paid" || st["state"] != "new" || st["noop"] == true {
			t.Fatalf("eligible action: %d %s", r.code, r.raw)
		}
		r = e.send("dry.act", 0, refBody("a2", "N2"))
		st = stepsOf(r.body)[0]
		if r.code != 200 || st["noop"] != true || st["reason"] != CodeStateNotEligible || st["state"] != "paid" {
			t.Fatalf("ineligible action: %d %s", r.code, r.raw)
		}
		r = e.send("dry.act.strict", 0, refBody("a3", "N2"))
		if r.code != 409 || errCode(r.body) != CodeStateNotEligible {
			t.Fatalf("ineligible action with on_ineligible=fail: %d %s", r.code, r.raw)
		}
		if e.events() != before || e.rows("N1")[0].Status != "new" {
			t.Fatal("a dry-run action wrote or published")
		}
	})
}

// 7. The side effects are reported and never run; a later step still resolves
// {{result.id}} of a row that would be created.
func TestDryRunSuppressesSideEffects(t *testing.T) {
	eachDryDB(t, func(t *testing.T, e *dryEnv) {
		e.cmds.Register("audit", func(context.Context, Operation) (Outcome, error) {
			t.Error("a dry run executed a command")
			return Outcome{}, nil
		})
		e.setup(dryContract("dry.fx", 1, true,
			upsertAct(),
			Action{Do: ActionEventEmit, Name: "orders.imported", Payload: map[string]any{"id": "{{result.id}}"}},
			Action{Do: ActionNotify, Template: "order_new", Payload: map[string]any{"ref": "{{payload.ref}}"}},
			Action{Do: ActionAutomationEnqueue, Name: "orders.flow", Payload: map[string]any{"ref": "{{payload.ref}}"}},
			Action{Do: ActionCommandRun, Command: "audit", Mapping: map[string]string{"ref": "{{payload.ref}}"}},
		))
		before := e.events()
		r := e.send("dry.fx", 0, dryBody("f1", "F1", 5, atomAt(1)))
		if r.code != 200 || r.body["status"] != StatusWouldApply {
			t.Fatalf("effects: %d %s", r.code, r.raw)
		}
		var got []string
		for _, s := range stepsOf(r.body) {
			w, _ := s["would"].(string)
			if tg, _ := s["target"].(string); tg != "" {
				w += ":" + tg
			}
			got = append(got, w)
		}
		want := "upsert-create,would_emit:orders.imported,would_notify:order_new,would_enqueue:orders.flow,would_command:audit"
		if strings.Join(got, ",") != want {
			t.Fatalf("steps = %v, want %s", got, want)
		}
		if len(e.em.reqs) != 0 || len(e.nt.reqs) != 0 || len(e.auto.evs) != 0 {
			t.Fatalf("a dry run ran a side effect: %d %d %d", len(e.em.reqs), len(e.nt.reqs), len(e.auto.evs))
		}
		if len(e.rows("F1")) != 0 || e.events() != before {
			t.Fatal("a dry run wrote or published")
		}
	})
}

// 8. The match is validated like a real event: ambiguous and empty are refused.
func TestDryRunMatchErrorsAreTheRealOnes(t *testing.T) {
	eachDryDB(t, func(t *testing.T, e *dryEnv) {
		e.setup(dryContract("dry.up", 1, true, upsertAct()))
		e.seed("DUP", "new", 1, atomAt(0))
		e.seed("DUP", "new", 2, atomAt(0))
		r := e.send("dry.up", 0, dryBody("m1", "DUP", 3, atomAt(2)))
		if r.code != 409 || errCode(r.body) != "ambiguous_match" {
			t.Fatalf("ambiguous: %d %s", r.code, r.raw)
		}
		r = e.send("dry.up", 0, dryBody("m2", "", 3, atomAt(2)))
		if r.code != 422 || errCode(r.body) != "match_value_empty" {
			t.Fatalf("empty match: %d %s", r.code, r.raw)
		}
		if r.hdr.Get(ShadowHeader) != "true" {
			t.Fatal("the shadow header must also mark error answers")
		}
		// A value that cannot take the type of its column is refused.
		r = e.send("dry.up", 0, dryBody("m3", "TY", "abc", atomAt(2)))
		if r.code != 422 || errCode(r.body) != "rejected_by_model" {
			t.Fatalf("bad type: %d %s", r.code, r.raw)
		}
		// A numeric string takes the column type, as the match values do (#625).
		r = e.send("dry.up", 0, dryBody("m4", "TY", "7", atomAt(2)))
		if r.code != 200 || changeOf(t, stepsOf(r.body)[0], "total")["to"] != float64(7) {
			t.Fatalf("numeric string: %d %s", r.code, r.raw)
		}
		if len(e.rows("TY")) != 0 {
			t.Fatal("a dry run wrote a row")
		}
	})
}

// 9. The host can hide more columns; nothing relaxes the gates.
func TestDryRunKeepsTheGatesAndTheRedaction(t *testing.T) {
	eachDryDB(t, func(t *testing.T, e *dryEnv) {
		e.ex.DryRunRedact = func(_, col string) bool { return col == "total" }
		e.setup(dryContract("dry.up", 1, false, upsertAct()))
		ctx := context.Background()
		res, err := e.h.DryRun(ctx, DryRunRequest{Org: e.org, Contract: "dry.up", Body: []byte(dryBody("p1", "P1", 5, atomAt(1)))})
		if err != nil || res.Status != StatusWouldApply || !res.Shadow {
			t.Fatalf("panel dry run: %+v %v", res, err)
		}
		if c := res.Actions[0].Changes["total"]; !c.Redacted || c.To != nil {
			t.Fatalf("a host-redacted column leaked: %+v", c)
		}
		if c := res.Actions[0].Changes["source_at"]; c.Redacted || c.To == nil {
			t.Fatalf("a plain column must be shown: %+v", c)
		}
		// Authorize still decides.
		e.ex.Authorize = func(context.Context, uuid.UUID, string, string) bool { return false }
		res, err = e.h.DryRun(ctx, DryRunRequest{Org: e.org, Contract: "dry.up", Body: []byte(dryBody("p2", "P1", 5, atomAt(1)))})
		if err != nil || res.Status != StatusWouldFail || res.ErrorCode != "forbidden" || res.HTTPStatus != http.StatusForbidden {
			t.Fatalf("a dry run must be gated by Authorize: %+v %v", res, err)
		}
		e.ex.Authorize = nil
		// A row of another organization is invisible: the dry run says create.
		other := dynamic.NewSystemCaller(uuid.New())
		if _, err := e.svc.Create(ctx, dryModel, other, map[string]any{"reference": "FOREIGN", "status": "new", "total": 1}); err != nil {
			t.Fatal(err)
		}
		res, err = e.h.DryRun(ctx, DryRunRequest{Org: e.org, Contract: "dry.up", Body: []byte(dryBody("p3", "FOREIGN", 5, atomAt(1)))})
		if err != nil || res.Actions[0].Would != "upsert-create" || res.Actions[0].RecordID != "" {
			t.Fatalf("a dry run must not see rows of other organizations: %+v %v", res, err)
		}
	})
}

// 10. Handler.DryRun (the panel "try with a sample event"): no receipt, no
// reservation, no signature; the rejections before the evaluation are typed.
func TestHandlerDryRunIsEphemeral(t *testing.T) {
	eachDryDB(t, func(t *testing.T, e *dryEnv) {
		e.setup(dryContract("dry.up", 1, false, upsertAct()))
		ctx := context.Background()
		req := DryRunRequest{Org: e.org, Contract: "dry.up", Body: []byte(dryBody("h1", "H1", 5, atomAt(1)))}
		for i := 0; i < 2; i++ { // the same sample can be tried again and again
			res, err := e.h.DryRun(ctx, req)
			if err != nil || res.Status != StatusWouldApply || res.Actions[0].Would != "upsert-create" {
				t.Fatalf("try %d: %+v %v", i, res, err)
			}
		}
		if recs, _ := e.h.Receipts(ctx, e.org, ReceiptFilter{}); len(recs) != 0 {
			t.Fatalf("Handler.DryRun left %d receipts", len(recs))
		}
		if len(e.rows("H1")) != 0 {
			t.Fatal("Handler.DryRun wrote a row")
		}
		bad := DryRunRequest{Org: e.org, Contract: "dry.up", Body: []byte(`{"ref":"x"}`)}
		_, err := e.h.DryRun(ctx, bad)
		var de *DryRunError
		if !errors.As(err, &de) || de.Status != 422 || de.Code != CodeInvalidSchema || len(de.Details) == 0 {
			t.Fatalf("schema rejection: %v", err)
		}
		_, err = e.h.DryRun(ctx, DryRunRequest{Org: e.org, Contract: "nope", Body: req.Body})
		if !errors.As(err, &de) || de.Status != 404 {
			t.Fatalf("unknown contract: %v", err)
		}
		_, err = e.h.DryRun(ctx, DryRunRequest{Org: e.org, Contract: "dry.up", Body: []byte(`{`)})
		if !errors.As(err, &de) || de.Status != 400 || de.Code != CodeMalformedJSON {
			t.Fatalf("malformed body: %v", err)
		}
	})
}

// 11. Route contracts (no chain) keep their report in the receipt too; a command
// route is not run.
func TestDryRunRouteContracts(t *testing.T) {
	eachDryDB(t, func(t *testing.T, e *dryEnv) {
		var ran int32
		e.cmds.Register("cmd", func(context.Context, Operation) (Outcome, error) {
			atomic.AddInt32(&ran, 1)
			return Outcome{}, nil
		})
		route := Contract{Key: "dry.route", Version: 1, JSONSchema: json.RawMessage(drySchema), Idempotency: "{event_id}",
			Auth: ProfileHMACCanonicalV1, DryRun: true,
			Route: Route{Model: dryModel, Op: OpUpsert, Match: []string{"reference"},
				Mapping: map[string]string{"reference": "ref", "total": "total"}}}
		cmd := Contract{Key: "dry.cmd", Version: 1, JSONSchema: json.RawMessage(drySchema), Idempotency: "{event_id}",
			Auth: ProfileHMACCanonicalV1, DryRun: true,
			Route: Route{Command: "cmd", Op: OpCreate, Mapping: map[string]string{"x": "ref"}}}
		e.setup(route, cmd)
		r := e.send("dry.route", 0, dryBody("o1", "O1", 4, atomAt(1)))
		st := stepsOf(r.body)[0]
		if r.code != 200 || r.body["status"] != StatusWouldApply || st["do"] != "record.upsert" || st["would"] != "upsert-create" {
			t.Fatalf("route: %d %s", r.code, r.raw)
		}
		r = e.send("dry.route", 0, dryBody("o1", "O1", 4, atomAt(1)))
		st = stepsOf(r.body)[0]
		if r.body["status"] != "replayed" || r.body["shadow"] != true || st["would"] != "upsert-create" {
			t.Fatalf("route replay: %s", r.raw)
		}
		if len(e.rows("O1")) != 0 {
			t.Fatal("a dry-run route wrote a row")
		}
		r = e.send("dry.cmd", 0, dryBody("c1", "C1", 4, atomAt(1)))
		st = stepsOf(r.body)[0]
		if r.code != 200 || st["would"] != "would_command" || st["target"] != "cmd" || atomic.LoadInt32(&ran) != 0 {
			t.Fatalf("command route: %d %s (ran %d)", r.code, r.raw, ran)
		}
	})
}

// A custom Executor that does not declare dry-run support can never be handed a
// dry-run contract: registration fails (fail closed), and Handler.DryRun refuses.
func TestDryRunNeedsAnExecutorThatSupportsIt(t *testing.T) {
	verifiers := map[string]Verifier{ProfileHMACCanonicalV1: VerifierFunc(func(context.Context, VerifyRequest) error { return nil })}
	build := func(ex Executor, c Contract) (*Handler, error) {
		reg := NewRegistry()
		if err := reg.Register(c); err != nil {
			t.Fatal(err)
		}
		return New(Config{Contracts: reg, Receipts: NewMemoryReceiptStore(), Executor: ex, Verifiers: verifiers, Logger: quietLogger()})
	}
	route := Contract{Key: "stock.changed", Version: 1, JSONSchema: json.RawMessage(stockSchema), Idempotency: "{event_id}",
		Auth: ProfileHMACCanonicalV1, DryRun: true,
		Route: Route{Model: "inventory", Op: OpUpsert, Match: []string{"sku"}, Mapping: map[string]string{"sku": "sku", "quantity": "quantity"}}}
	fake := newFakeExec()
	if _, err := build(fake, route); !errors.Is(err, ErrDryRunUnsupported) {
		t.Fatalf("a dry-run contract must fail to register on an executor without support: %v", err)
	}
	chain := dryContract("dry.up", 1, true, upsertAct())
	if _, err := build(fake, chain); !errors.Is(err, ErrDryRunUnsupported) {
		t.Fatalf("chain: %v", err)
	}
	// A wrapper that hides the DynamicExecutor methods hides DryRunSupporter too.
	type opaque struct{ Executor }
	if _, err := build(opaque{&DynamicExecutor{store: guardFake()}}, route); !errors.Is(err, ErrDryRunUnsupported) {
		t.Fatalf("wrapped executor: %v", err)
	}
	// The same contract without dry_run registers fine, but the panel dry run is refused.
	route.DryRun = false
	h, err := build(fake, route)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.DryRun(context.Background(), DryRunRequest{Org: uuid.New(), Contract: "stock.changed",
		Body: []byte(stockBody("e1", "S1", 1))}); !errors.Is(err, ErrDryRunUnsupported) {
		t.Fatalf("Handler.DryRun on an executor without support: %v", err)
	}
	if fake.calls != 0 {
		t.Fatalf("the executor was called %d times", fake.calls)
	}
	// A DynamicExecutor does support it.
	if !executorDryRuns(&DynamicExecutor{}) {
		t.Fatal("DynamicExecutor must declare dry-run support")
	}
}

// The contract field is plain data: it serializes, loads and is validated as a bool.
func TestContractDryRunField(t *testing.T) {
	c := stockContract()
	raw, err := json.Marshal(c)
	if err != nil || strings.Contains(string(raw), "dry_run") {
		t.Fatalf("an unset dry_run must not be serialized: %s %v", raw, err)
	}
	c.DryRun = true
	raw, err = json.Marshal(c)
	if err != nil || !strings.Contains(string(raw), `"dry_run":true`) {
		t.Fatalf("dry_run must serialize: %s %v", raw, err)
	}
	reg := NewRegistry()
	if err := reg.LoadJSON(raw); err != nil {
		t.Fatal(err)
	}
	if got, ok := reg.get(c.Key, 0); !ok || !got.DryRun {
		t.Fatal("dry_run was lost on load")
	}
	if err := NewRegistry().LoadJSON([]byte(`{"key":"x.y","version":1,"dry_run":"yes"}`)); err == nil {
		t.Fatal("a non-boolean dry_run must be refused")
	}
}

// record.create runs as an upsert on its idempotency column; the report still
// calls it a create.
func TestDryRunCreateIsReportedAsCreate(t *testing.T) {
	eachDryDB(t, func(t *testing.T, e *dryEnv) {
		e.setup(dryContract("dry.cr", 1, true, Action{Do: ActionRecordCreate, Model: dryModel, IdempotencyField: "idem",
			Mapping: map[string]string{"reference": "{{payload.ref}}", "total": "{{payload.total}}"}}))
		r := e.send("dry.cr", 0, dryBody("k1", "K1", 9, atomAt(1)))
		st := stepsOf(r.body)[0]
		if r.code != 200 || st["would"] != "create" || st["created"] != true || st["do"] != ActionRecordCreate {
			t.Fatalf("create report: %d %s", r.code, r.raw)
		}
		if len(e.rows("K1")) != 0 {
			t.Fatal("a dry-run create wrote a row")
		}
	})
}

// Whatever the operation, a dry run reaches no write of the store (the fake
// counts them) and always reports a Would.
func TestDryRunNeverCallsAWrite(t *testing.T) {
	org := uuid.New()
	f := opsFake()
	id := opsRow(f, org, "S1", "W1", "new")
	d := &DynamicExecutor{store: f}
	vals := func(sku string) map[string]any { return map[string]any{"sku": sku, "warehouse": "W1", "quantity": 5} }
	route := func(kind Op, sku string) Operation {
		return Operation{Org: org, Route: Route{Model: "inv", Op: kind, Match: []string{"sku"}}, Values: vals(sku)}
	}
	cases := []struct {
		name string
		op   Operation
		want string
	}{
		{"create", route(OpCreate, "S9"), "create"},
		{"upsert-create", route(OpUpsert, "S9"), "upsert-create"},
		{"upsert-update", route(OpUpsert, "S1"), "upsert-update"},
		{"update", route(OpUpdate, "S1"), "update"},
		{"delete", delOp(org, "", 1, "S1"), "delete"},
		{"action", actionOp(org, "mark_paid", "S1", map[string]any{"reference": "R"}), "action"},
		{"transition", transitionOp(org, "paid", "S1"), "transition"},
		{"restore", stateOpOf(org, OpRestore, "S1"), "restore"},
	}
	for _, c := range cases {
		op := c.op
		op.DryRun = true
		out, err := d.Execute(context.Background(), op)
		if err != nil || out.Would == nil || out.Would.Would != c.want {
			t.Fatalf("%s: out=%+v err=%v", c.name, out, err)
		}
	}
	if f.creates != 0 || f.updates != 0 || f.deletes != 0 || f.execCount() != 0 || len(f.rows) != 1 {
		t.Fatalf("a dry run wrote: creates=%d updates=%d deletes=%d execs=%d rows=%d", f.creates, f.updates, f.deletes, f.execCount(), len(f.rows))
	}
	if r := rowOf(f, id); r["status"] != "new" || r["deleted_at"] != nil {
		t.Fatalf("the row changed: %v", r)
	}
	// update without a row is the real 404, and a missing match is refused.
	op := route(OpUpdate, "NOPE")
	op.DryRun = true
	if _, err := d.Execute(context.Background(), op); execErr(t, err).Code != "record_not_found" {
		t.Fatalf("update of a missing row: %v", err)
	}
	op = route(OpUpsert, "S1")
	op.DryRun, op.Values = true, map[string]any{"sku": "", "warehouse": "W1"}
	if _, err := d.Execute(context.Background(), op); execErr(t, err).Code != "match_value_empty" {
		t.Fatalf("empty match: %v", err)
	}
}
