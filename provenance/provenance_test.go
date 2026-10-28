package provenance

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type widget struct {
	ID             uuid.UUID  `gorm:"type:uuid;primaryKey"`
	OrganizationID uuid.UUID  `gorm:"type:uuid"`
	Name           string     `gorm:"size:80"`
	CreatedByID    *uuid.UUID `gorm:"type:uuid"`
	UpdatedByID    *uuid.UUID `gorm:"type:uuid"`
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

func (widget) TableName() string { return "widgets" }

type plain struct {
	ID   uuid.UUID `gorm:"type:uuid;primaryKey"`
	Name string
}

func (plain) TableName() string { return "plains" }

func openSQLite(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", uuid.NewString())), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	return setup(t, db)
}

func setup(t *testing.T, db *gorm.DB) *gorm.DB {
	t.Helper()
	if err := db.AutoMigrate(&widget{}, &plain{}); err != nil {
		t.Fatal(err)
	}
	if err := Register(db, WithSkipTables("skipped")); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestNormalizeModes(t *testing.T) {
	u := uuid.New()
	cases := []struct {
		name string
		p    Provenance
		want Mode
	}{
		{"human", Human(u, ViaUI), ModeManual},
		{"agent alone", Agent("Asistente", ViaJob), ModeAutomated},
		{"agent on behalf", Agent("Asistente", ViaAutomation).OnBehalf(u), ModeAutomated},
		{"assisted", Assisted("Asistente", u, ViaChat), ModeAssisted},
		{"proposed", Agent("Asistente", ViaChat).OnBehalf(u).Propose(), ModeAutomated},
		{"system", System(ViaSeed, "Datos semilla"), ModeAutomated},
		{"integration", Integration("data-sync", ViaImport, ""), ModeAutomated},
	}
	for _, c := range cases {
		got := c.p.Normalize()
		if got.Mode != c.want {
			t.Errorf("%s: mode %s, want %s", c.name, got.Mode, c.want)
		}
		if err := got.Validate(); err != nil {
			t.Errorf("%s: validate: %v", c.name, err)
		}
	}
	// Proposed then authorized → assisted, authorized_at set.
	p := Agent("Asistente", ViaChat).OnBehalf(u).Propose().Authorize(u, time.Now())
	if n := p.Normalize(); n.Mode != ModeAssisted || n.Proposed || n.AuthorizedAt == nil {
		t.Fatalf("confirm: %+v", n)
	}
	if err := (Provenance{ActorKind: ActorHuman, Mode: ModeManual}).Validate(); !errors.Is(err, ErrHumanNoActor) {
		t.Fatalf("human without id: %v", err)
	}
	if err := (Provenance{ActorKind: "robot", Mode: ModeManual}).Validate(); !errors.Is(err, ErrBadActorKind) {
		t.Fatalf("bad kind: %v", err)
	}
}

func TestAccountable(t *testing.T) {
	u, v := uuid.New(), uuid.New()
	if a := Human(u, ViaUI).Accountable(); a == nil || *a != u {
		t.Fatal("human accountable")
	}
	if a := Agent("Asistente", ViaChat).OnBehalf(u).Authorize(v, time.Now()).Accountable(); a == nil || *a != v {
		t.Fatal("authorized_by wins over on_behalf_of")
	}
	if a := Agent("Asistente", ViaAutomation).OnBehalf(u).Accountable(); a == nil || *a != u {
		t.Fatal("on_behalf_of")
	}
	if System(ViaSeed, "").Accountable() != nil || Agent("Asistente", ViaJob).Accountable() != nil {
		t.Fatal("unattended work never gets a fake person")
	}
}

func TestContextAndJSON(t *testing.T) {
	ctx := context.Background()
	if _, ok := FromContext(ctx); ok {
		t.Fatal("empty ctx")
	}
	u := uuid.New()
	ctx = WithContext(ctx, Human(u, ViaUI))
	ctx2 := Ensure(ctx, System(ViaSeed, ""))
	if p, _ := FromContext(ctx2); p.ActorKind != ActorHuman {
		t.Fatal("Ensure must keep the existing provenance")
	}
	ctx3 := Update(ctx, func(p Provenance) Provenance { return p.WithRef(RefSession, "s1") })
	if p, _ := FromContext(ctx3); p.RefID != "s1" {
		t.Fatal("Update")
	}
	if _, ok := FromContext(Without(ctx)); ok {
		t.Fatal("Without must clear it")
	}
	js := Assisted("Asistente", u, ViaChat).WithSummary("Propuso crear un hallazgo").JSON()
	back := Parse(js)
	if back.Mode != ModeAssisted || back.AuthorizedBy == nil || *back.AuthorizedBy != u || back.Summary == "" {
		t.Fatalf("roundtrip: %s", js)
	}
	if Parse("{bad").ActorKind != "" {
		t.Fatal("bad json → zero")
	}
}

func TestHookStampsAndRecords(t *testing.T) {
	db := openSQLite(t)
	exerciseHook(t, db)
}

func exerciseHook(t *testing.T, db *gorm.DB) {
	t.Helper()
	ctx := context.Background()
	org, person, approver := uuid.New(), uuid.New(), uuid.New()
	store := NewStore(db)

	// 1. No provenance: untouched (legacy behaviour).
	w0 := widget{ID: uuid.New(), OrganizationID: org, Name: "legacy"}
	if err := db.WithContext(ctx).Create(&w0).Error; err != nil {
		t.Fatal(err)
	}
	if w0.CreatedByID != nil {
		t.Fatal("no provenance must not stamp")
	}
	if got := store.Created(ctx, org, "widgets", []string{w0.ID.String()}); len(got) != 0 {
		t.Fatalf("no provenance must not record: %v", got)
	}

	// 2. Person with agent: created_by_id = approver, lateral row assisted.
	w1 := widget{ID: uuid.New(), OrganizationID: org, Name: "assisted"}
	actx := WithContext(ctx, Agent("Asistente", ViaChat).OnBehalf(person).Authorize(approver, time.Now()).
		WithRef(RefProposal, "p-1").WithSummary("Propuso crear assisted"))
	if err := db.WithContext(actx).Create(&w1).Error; err != nil {
		t.Fatal(err)
	}
	if w1.CreatedByID == nil || *w1.CreatedByID != approver || w1.UpdatedByID == nil {
		t.Fatalf("created_by_id not stamped: %+v", w1)
	}
	c := store.Created(ctx, org, "widgets", []string{w1.ID.String()})[w1.ID.String()]
	if c.Mode != string(ModeAssisted) || c.ActorKind != string(ActorAgent) || c.AuthorizedBy == nil || c.RefID != "p-1" || c.OrganizationID == nil || *c.OrganizationID != org {
		t.Fatalf("created entry: %+v", c)
	}

	// 3. Explicit created_by_id is never overwritten.
	mine := uuid.New()
	w2 := widget{ID: uuid.New(), OrganizationID: org, Name: "explicit", CreatedByID: &mine}
	if err := db.WithContext(WithContext(ctx, Human(person, ViaUI))).Create(&w2).Error; err != nil {
		t.Fatal(err)
	}
	if *w2.CreatedByID != mine {
		t.Fatal("explicit created_by_id overwritten")
	}

	// 4. System seed on a batch: no fake person, one row per record.
	batch := []widget{{ID: uuid.New(), OrganizationID: org, Name: "a"}, {ID: uuid.New(), OrganizationID: org, Name: "b"}}
	if err := DB(db, System(ViaSeed, "Datos semilla")).Create(&batch).Error; err != nil {
		t.Fatal(err)
	}
	if batch[0].CreatedByID != nil {
		t.Fatal("system work must not stamp a person")
	}
	got := store.Created(ctx, org, "widgets", []string{batch[0].ID.String(), batch[1].ID.String()})
	if len(got) != 2 || got[batch[1].ID.String()].Via != ViaSeed {
		t.Fatalf("batch: %v", got)
	}

	// 5. Update (Save + Updates on a model value) and delete → history.
	w1.Name = "renamed"
	if err := db.WithContext(WithContext(ctx, Human(person, ViaUI))).Save(&w1).Error; err != nil {
		t.Fatal(err)
	}
	if w1.UpdatedByID == nil || *w1.UpdatedByID != person {
		t.Fatalf("updated_by_id: %+v", w1.UpdatedByID)
	}
	if err := db.WithContext(WithContext(ctx, Agent("Asistente", ViaAutomation).OnBehalf(person))).
		Model(&w1).Updates(map[string]any{"name": "by automation"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(WithContext(ctx, Human(approver, ViaUI))).Delete(&w1).Error; err != nil {
		t.Fatal(err)
	}
	v, err := store.ForRecord(ctx, org, "widgets", w1.ID.String(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.History) != 4 || v.Created == nil || v.LastChange.Action != ActionDeleted {
		t.Fatalf("history: %+v", v.History)
	}
	if v.History[1].Mode != string(ModeManual) || v.History[2].Mode != string(ModeAutomated) {
		t.Fatalf("modes: %s %s", v.History[1].Mode, v.History[2].Mode)
	}

	// 6. Schema-less map create with id.
	id := uuid.New()
	row := map[string]any{"id": id, "name": "map", "organization_id": org, "created_by_id": nil}
	if err := db.WithContext(WithContext(ctx, Human(person, ViaAPI))).Table("widgets").Create(row).Error; err != nil {
		t.Fatal(err)
	}
	if row["created_by_id"] != person {
		t.Fatalf("map created_by_id: %v", row["created_by_id"])
	}
	if got := store.Created(ctx, org, "widgets", []string{id.String()}); len(got) != 1 {
		t.Fatal("map create not recorded")
	}

	// 7. Rolled-back transaction leaves no provenance.
	rb := widget{ID: uuid.New(), OrganizationID: org, Name: "rollback"}
	_ = db.WithContext(WithContext(ctx, Human(person, ViaUI))).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&rb).Error; err != nil {
			return err
		}
		return errors.New("abort")
	})
	if got := store.Created(ctx, org, "widgets", []string{rb.ID.String()}); len(got) != 0 {
		t.Fatal("rolled back write kept its provenance")
	}

	// 8. Idempotent creation row: a second "created" keeps the first author.
	if err := store.Record(ctx, NewEntry(org, "widgets", w2.ID.String(), ActionCreated, System(ViaMigration, "backfill"), time.Now())); err != nil {
		t.Fatal(err)
	}
	if c := store.Created(ctx, org, "widgets", []string{w2.ID.String()})[w2.ID.String()]; c.ActorKind != string(ActorHuman) {
		t.Fatalf("backfill overwrote the original author: %+v", c)
	}

	// 9. Models without audit columns still get the lateral row.
	p := plain{ID: uuid.New(), Name: "x"}
	if err := db.WithContext(WithContext(ctx, Integration("data-sync", ViaImport, "Plano de datos"))).Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	if got := store.Created(ctx, uuid.Nil, "plains", []string{p.ID.String()}); got[p.ID.String()].ActorName != "data-sync" {
		t.Fatalf("plain: %v", got)
	}
}

func TestRecordFailureKeepsTheWrite(t *testing.T) {
	db := openSQLite(t)
	if err := db.Migrator().DropTable(TableName); err != nil {
		t.Fatal(err)
	}
	w := widget{ID: uuid.New(), Name: "kept"}
	if err := db.WithContext(WithContext(context.Background(), Human(uuid.New(), ViaUI))).Create(&w).Error; err != nil {
		t.Fatalf("business write must survive a provenance failure: %v", err)
	}
	var n int64
	db.Model(&widget{}).Where("id = ?", w.ID).Count(&n)
	if n != 1 {
		t.Fatal("write lost")
	}
}

func TestPresentation(t *testing.T) {
	u, a := uuid.New(), uuid.New()
	e := NewEntry(uuid.New(), "findings", "r1", ActionCreated, Agent("Asistente", ViaChat).OnBehalf(u).Authorize(a, time.Now()).WithSummary("Propuso crear"), time.Now())
	m := e.Presentation(map[uuid.UUID]Person{a: {Name: "Ana Pérez", Avatar: "/a.png"}})
	if m["mode"] != "assisted" || m["authorized_by_name"] != "Ana Pérez" || m["authorized_by_avatar"] != "/a.png" || m["actor_name"] != "Asistente" {
		t.Fatalf("presentation: %v", m)
	}
	if got := People(e); len(got) != 2 {
		t.Fatalf("people: %v", got)
	}
}

// TestConcurrentWritesKeepTheirOwnProvenance runs writes with different
// provenance in parallel (go test -race): each record must carry its own.
func TestConcurrentWritesKeepTheirOwnProvenance(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()
	org := uuid.New()
	const n = 24
	ids := make([]uuid.UUID, n)
	people := make([]uuid.UUID, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		ids[i], people[i] = uuid.New(), uuid.New()
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := widget{ID: ids[i], OrganizationID: org, Name: fmt.Sprint(i)}
			if err := db.WithContext(WithContext(ctx, Human(people[i], ViaUI))).Create(&w).Error; err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	keys := make([]string, n)
	for i := range ids {
		keys[i] = ids[i].String()
	}
	got := NewStore(db).Created(ctx, org, "widgets", keys)
	for i := range ids {
		e := got[ids[i].String()]
		if e.ActorID == nil || *e.ActorID != people[i] {
			t.Fatalf("record %d has another provenance: %+v", i, e)
		}
	}
}

// TestPostgres runs the same scenario on a real PostgreSQL when
// PROVENANCE_TEST_PG_DSN is set (e.g. postgres://user:testpass@127.0.0.1:5440/provenance).
func TestPostgres(t *testing.T) {
	dsn := os.Getenv("PROVENANCE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("PROVENANCE_TEST_PG_DSN not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	for _, tbl := range []string{"widgets", "plains", TableName} {
		db.Exec("DROP TABLE IF EXISTS " + tbl)
	}
	setup(t, db)
	exerciseHook(t, db)
	// Migrate is idempotent.
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	// A failing provenance insert inside the write's transaction (PostgreSQL
	// aborts the whole tx on error) is isolated by the savepoint.
	if err := db.Migrator().DropTable(TableName); err != nil {
		t.Fatal(err)
	}
	w := widget{ID: uuid.New(), Name: "kept"}
	if err := db.WithContext(WithContext(context.Background(), Human(uuid.New(), ViaUI))).Create(&w).Error; err != nil {
		t.Fatalf("business write must survive a provenance failure: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
}
