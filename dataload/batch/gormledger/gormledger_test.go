package gormledger

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/asteby/metacore-kernel/dataload"
	"github.com/asteby/metacore-kernel/dataload/batch"
	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func testDB(t *testing.T) *gorm.DB {
	t.Helper()
	name := strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", name)), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatalf("Migrate is not idempotent: %v", err)
	}
	return db
}

func TestLedgerRoundTrip(t *testing.T) {
	ctx := context.Background()
	l := New(testDB(t))
	s := batch.Scope{OrgID: uuid.New(), Kind: "export"}
	other := batch.Scope{OrgID: uuid.New(), Kind: "export"}

	if done, err := l.Done(ctx, s, "m1"); err != nil || done {
		t.Fatalf("Done on empty: %v %v", done, err)
	}
	if p, err := l.LastContentPrint(ctx, s); err != nil || p != "" {
		t.Fatalf("Last on empty: %q %v", p, err)
	}
	gen := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	rec := batch.Record{ManifestSHA: "m1", ContentPrint: "p1", GeneratedAt: &gen, Files: []string{"a.json"}, Counts: map[string]int{"rows": 3},
		Warnings: []dataload.Message{dataload.NewMessage(dataload.CodeBatchUnknownFile, "file", "x.json")}}
	if err := l.Record(ctx, s, rec); err != nil {
		t.Fatal(err)
	}
	if err := l.Record(ctx, s, rec); err != nil { // idempotent
		t.Fatalf("second Record: %v", err)
	}
	if done, _ := l.Done(ctx, s, "m1"); !done {
		t.Fatal("Done false after Record")
	}
	if done, _ := l.Done(ctx, other, "m1"); done {
		t.Fatal("scopes leak into each other")
	}
	time.Sleep(2 * time.Millisecond)
	if err := l.Record(ctx, s, batch.Record{ManifestSHA: "m2", ContentPrint: "p2", SameContent: true}); err != nil {
		t.Fatal(err)
	}
	if p, _ := l.LastContentPrint(ctx, s); p != "p2" {
		t.Fatalf("last print = %q", p)
	}

	var rows []Row
	if err := l.db.Order("id").Find(&rows).Error; err != nil || len(rows) != 2 {
		t.Fatalf("rows = %d err=%v", len(rows), err)
	}
	r := rows[0]
	if r.OrganizationID != s.OrgID || r.Counts["rows"] != 3 || len(r.Warnings) != 1 || r.Warnings[0].Params["file"] != "x.json" || r.GeneratedAt == nil || !r.GeneratedAt.Equal(gen) {
		t.Fatalf("row = %+v", r)
	}
}

// End to end: Process over the SQL ledger.
func TestProcessWithGormLedger(t *testing.T) {
	ctx := context.Background()
	l := New(testDB(t))
	s := batch.Scope{OrgID: uuid.New(), Kind: "export"}
	body := []byte(`{"a":1}`)
	mk := func(gen string) fstest.MapFS {
		m, _ := json.Marshal(batch.Manifest{GeneratedAt: gen, Files: map[string]string{"a.json": sum(body)}})
		return fstest.MapFS{"a.json": {Data: body}, "manifest.json": {Data: m}}
	}
	imports := 0
	ct := batch.Contract{Files: []batch.FileSpec{{Name: "a.json", Import: func(context.Context, batch.File) (batch.FileReport, error) {
		imports++
		return batch.FileReport{Counts: map[string]int{"docs": 1}}, nil
	}}}}
	res, err := batch.Process(ctx, mk("2026-10-01T00:00:00Z"), ct, l, s)
	if err != nil || !res.Imported {
		t.Fatalf("%+v %v", res, err)
	}
	res, err = batch.Process(ctx, mk("2026-10-01T00:00:00Z"), ct, l, s)
	if err != nil || !res.Skipped || res.SameContent {
		t.Fatalf("same manifest: %+v %v", res, err)
	}
	res, err = batch.Process(ctx, mk("2026-10-02T00:00:00Z"), ct, l, s)
	if err != nil || !res.SameContent || imports != 1 {
		t.Fatalf("same content: %+v %v imports=%d", res, err, imports)
	}
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
