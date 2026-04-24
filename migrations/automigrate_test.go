package migrations

import (
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// --- fixture models --------------------------------------------------------

type testOrg struct {
	ID   uuid.UUID `gorm:"type:text;primaryKey"`
	Name string
}

func (testOrg) TableName() string { return "test_orgs" }

type testUser struct {
	ID    uuid.UUID `gorm:"type:text;primaryKey"`
	OrgID uuid.UUID `gorm:"type:text;index"`
	Org   testOrg   `gorm:"foreignKey:OrgID"`
	Email string
}

func (testUser) TableName() string { return "test_users" }

type testOrder struct {
	ID     uuid.UUID `gorm:"type:text;primaryKey"`
	UserID uuid.UUID `gorm:"type:text;index"`
	User   testUser  `gorm:"foreignKey:UserID"`
	Code   string
}

func (testOrder) TableName() string { return "test_orders" }

func memDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	return db
}

// --- tests -----------------------------------------------------------------

func TestAutoMigrateCreatesTables(t *testing.T) {
	db := memDB(t)
	if err := AutoMigrate(db, []any{&testOrg{}, &testUser{}, &testOrder{}}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	for _, name := range []string{"test_orgs", "test_users", "test_orders"} {
		if !db.Migrator().HasTable(name) {
			t.Errorf("table %s not created", name)
		}
	}
}

func TestAutoMigrateRestoresOriginalFKConfig(t *testing.T) {
	db := memDB(t)
	db.Config.DisableForeignKeyConstraintWhenMigrating = true
	if err := AutoMigrate(db, []any{&testOrg{}}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	if db.Config.DisableForeignKeyConstraintWhenMigrating != true {
		t.Fatal("original FK-disabled flag was not restored")
	}
}

func TestAutoMigrateEmptyIsNoop(t *testing.T) {
	db := memDB(t)
	if err := AutoMigrate(db, nil); err != nil {
		t.Fatalf("empty: %v", err)
	}
	if err := AutoMigrate(db, []any{}); err != nil {
		t.Fatalf("empty slice: %v", err)
	}
}

func TestTopoSortRespectsForeignKeys(t *testing.T) {
	models := map[string]any{
		"test_orders": &testOrder{}, // depends on testUser → testOrg
		"test_orgs":   &testOrg{},
		"test_users":  &testUser{}, // depends on testOrg
	}
	sorted := TopoSort(models)
	if len(sorted) != 3 {
		t.Fatalf("sorted len = %d, want 3", len(sorted))
	}

	// Index where each model landed.
	pos := map[string]int{}
	for i, m := range sorted {
		pos[structName(m)] = i
	}
	if pos["testOrg"] > pos["testUser"] {
		t.Error("testOrg must come before testUser (dependency)")
	}
	if pos["testUser"] > pos["testOrder"] {
		t.Error("testUser must come before testOrder (dependency)")
	}
}

func TestAutoMigrateSortedCreatesTables(t *testing.T) {
	db := memDB(t)
	err := AutoMigrateSorted(db, map[string]any{
		"test_orders": &testOrder{},
		"test_users":  &testUser{},
		"test_orgs":   &testOrg{},
	})
	if err != nil {
		t.Fatalf("AutoMigrateSorted: %v", err)
	}
	for _, name := range []string{"test_orgs", "test_users", "test_orders"} {
		if !db.Migrator().HasTable(name) {
			t.Errorf("table %s not created", name)
		}
	}
}

func TestResetDatabaseSQLiteDropsTables(t *testing.T) {
	db := memDB(t)
	if err := AutoMigrate(db, []any{&testOrg{}, &testUser{}}); err != nil {
		t.Fatalf("pre-migrate: %v", err)
	}
	if !db.Migrator().HasTable("test_orgs") {
		t.Fatal("pre-migrate: table missing")
	}
	if err := ResetDatabase(db); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if db.Migrator().HasTable("test_orgs") {
		t.Error("table still present after reset")
	}
	if db.Migrator().HasTable("test_users") {
		t.Error("table still present after reset")
	}
}

func TestSafeIdent(t *testing.T) {
	good := []string{"users", "my_table", "Table123", "_foo"}
	bad := []string{"", "users; DROP TABLE x;", "users'", "users--", `has space`}
	for _, s := range good {
		if !safeIdent(s) {
			t.Errorf("safeIdent(%q) = false, want true", s)
		}
	}
	for _, s := range bad {
		if safeIdent(s) {
			t.Errorf("safeIdent(%q) = true, want false (injection risk)", s)
		}
	}
}
