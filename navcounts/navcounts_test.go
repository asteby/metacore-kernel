package navcounts

import (
	"context"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func testDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	db.Exec(`CREATE TABLE alerts (id integer primary key, organization_id text, status text, severity text, deleted_at datetime)`)
	for _, r := range [][]string{{"o1", "open", "low"}, {"o1", "open", "critical"}, {"o1", "closed", "low"}, {"o2", "open", "low"}} {
		db.Exec(`INSERT INTO alerts (organization_id,status,severity) VALUES (?,?,?)`, r[0], r[1], r[2])
	}
	return db
}

func TestComputeToneEscalationPermissionOrgAndOmitZero(t *testing.T) {
	db := testDB(t)
	r := New()
	if err := r.Register(
		Counter{ID: "alerts.open", Table: "alerts", Where: map[string]any{"status": "open"}, Tone: ToneAttention, Target: "/m/alerts", Filter: map[string]string{"f_status": "EQ:open"},
			Escalate: []Escalation{{Where: map[string]any{"severity": "critical"}, Tone: ToneCritical}}, Permission: "alerts.index"},
		Counter{ID: "alerts.none", Table: "alerts", Where: map[string]any{"status": "nope"}, Tone: ToneInfo},
		Counter{ID: "ghost.table", Table: "does_not_exist", Tone: ToneInfo},
	); err != nil {
		t.Fatal(err)
	}
	got := r.Compute(context.Background(), db, "o1", func(string) bool { return true }, time.Now())
	if len(got) != 1 || got[0].Count != 2 || got[0].Tone != ToneCritical {
		t.Fatalf("got %+v", got)
	}
	if got[0].Href != "/m/alerts?f_status=EQ%3Aopen" {
		t.Fatalf("href %q", got[0].Href)
	}
	if v := r.Compute(context.Background(), db, "o1", func(string) bool { return false }, time.Now()); len(v) != 0 {
		t.Fatalf("permission not enforced: %+v", v)
	}
	if v := r.Compute(context.Background(), db, "o2", nil, time.Now()); len(v) != 1 || v[0].Count != 1 || v[0].Tone != ToneAttention {
		t.Fatalf("org isolation: %+v", v)
	}
}

func TestRegisterRejectsInvalid(t *testing.T) {
	r := New()
	bad := []Counter{
		{ID: "a", Table: "t; drop", Tone: ToneInfo},
		{ID: "a", Table: "t", Tone: "loud"},
		{ID: "a", Table: "t", Tone: ToneInfo, Where: map[string]any{"x": nil}},
		{ID: "", Table: "t", Tone: ToneInfo},
	}
	for _, c := range bad {
		if r.Register(c) == nil {
			t.Fatalf("accepted %+v", c)
		}
	}
	_ = r.Register(Counter{ID: "ok", Table: "t", Tone: ToneInfo})
	if r.Register(Counter{ID: "ok", Table: "t", Tone: ToneInfo}) == nil {
		t.Fatal("duplicate accepted")
	}
}
