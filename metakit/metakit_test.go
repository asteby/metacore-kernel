package metakit

import (
	"errors"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/modelbase"
)

type ticket struct {
	ID       uint `gorm:"primaryKey"`
	Title    string
	Status   string
	ClosedAt *time.Time
}

func (ticket) TableName() string { return "tickets" }

func (ticket) DefineModal() modelbase.ModalMetadata {
	return modelbase.ModalMetadata{Fields: []modelbase.FieldDef{Text("title", "Title", true)}}
}

func newDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&ticket{}); err != nil {
		t.Fatal(err)
	}
	if err := Install(db); err != nil {
		t.Fatal(err)
	}
	Guard[ticket]("tickets", func(before, after *ticket) error {
		if after.Title == "" {
			return errors.New("title is required")
		}
		if before != nil && before.Status == "closed" && after.Status != "closed" {
			return errors.New("reopen with its action")
		}
		return nil
	})
	return db
}

func fieldCode(t *testing.T, err error, field string) string {
	t.Helper()
	var ve *dynamic.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want *dynamic.ValidationError, got %v", err)
	}
	if fe := ve.Fields[field]; len(fe) > 0 {
		return fe[0].Code
	}
	t.Fatalf("no error for field %q in %v", field, ve.Fields)
	return ""
}

func TestGuardEditableAndTrusted(t *testing.T) {
	db := newDB(t)
	tk := ticket{Title: "a", Status: "open"}
	if err := db.Create(&tk).Error; err != nil {
		t.Fatal(err)
	}

	// Form column: allowed.
	if err := db.Model(&ticket{}).Where("id = ?", tk.ID).Updates(map[string]any{"title": "b"}).Error; err != nil {
		t.Fatalf("form column must be editable: %v", err)
	}
	// Column outside the form: refused with protected_field.
	err := db.Model(&ticket{}).Where("id = ?", tk.ID).Updates(map[string]any{"status": "closed"}).Error
	if code := fieldCode(t, err, "status"); code != "protected_field" {
		t.Fatalf("code = %q", code)
	}
	// The same change from a trusted action: allowed.
	if err := Trusted(db).Model(&tk).Updates(map[string]any{"status": "closed"}).Error; err != nil {
		t.Fatalf("trusted action: %v", err)
	}
	// The rule still runs for trusted writes.
	err = Trusted(db).Model(&tk).Updates(map[string]any{"status": "open"}).Error
	if code := fieldCode(t, err, "_"); code != dynamic.CodeInvalid {
		t.Fatalf("rule error code = %q", code)
	}
	var got ticket
	db.First(&got, tk.ID)
	if got.Title != "b" || got.Status != "closed" {
		t.Fatalf("row = %+v", got)
	}
}

func TestGuardRuleOnMapCreate(t *testing.T) {
	db := newDB(t)
	err := db.Table("tickets").Create(map[string]any{"title": "", "status": "open"}).Error
	if code := fieldCode(t, err, "_"); code != dynamic.CodeInvalid {
		t.Fatalf("code = %q", code)
	}
	if err := db.Table("tickets").Create(map[string]any{"title": "ok", "status": "open"}).Error; err != nil {
		t.Fatalf("valid map insert: %v", err)
	}
}

func TestGuardEditableOptionAndStructWrites(t *testing.T) {
	db := newDB(t)
	Guard[ticket]("tickets", nil, Editable("status"))
	tk := ticket{Title: "a", Status: "open"}
	db.Create(&tk)
	if err := db.Model(&ticket{}).Where("id = ?", tk.ID).Updates(map[string]any{"status": "pending"}).Error; err != nil {
		t.Fatalf("Editable option: %v", err)
	}
	err := db.Model(&ticket{}).Where("id = ?", tk.ID).Updates(map[string]any{"closed_at": time.Now()}).Error
	if code := fieldCode(t, err, "closed_at"); code != "protected_field" {
		t.Fatalf("code = %q", code)
	}
	// Struct saves are left to the model's own hooks.
	tk.Status = "whatever"
	if err := db.Save(&tk).Error; err != nil {
		t.Fatalf("struct save: %v", err)
	}
}

func TestInstallIsIdempotent(t *testing.T) {
	db := newDB(t)
	if err := Install(db); err != nil {
		t.Fatal(err)
	}
	if db.Callback().Update().Get(CallbackUpdate) == nil || db.Callback().Create().Get(CallbackCreate) == nil {
		t.Fatal("callbacks must be registered")
	}
}

func TestMapUpdateAndValidation(t *testing.T) {
	if MapUpdate(nil) {
		t.Error("nil is not a partial update")
	}
	tx := &gorm.DB{Statement: &gorm.Statement{Dest: map[string]interface{}{"status": "x"}}}
	if !MapUpdate(tx) {
		t.Error("Updates(map) must be detected")
	}
	if Validation(nil) != nil {
		t.Error("nil stays nil")
	}
	ve := dynamic.NewValidationError().Add("x", "required", nil).Err()
	if Validation(ve) != ve {
		t.Error("a ValidationError passes through")
	}
	if !errors.Is(Validation(errors.New("boom")), dynamic.ErrValidation) {
		t.Error("plain errors become validation errors")
	}
}

func TestSameValue(t *testing.T) {
	now := time.Date(2026, 10, 13, 10, 0, 0, 123456000, time.UTC)
	local := now.Truncate(time.Second).In(time.FixedZone("CST", -6*3600))
	var nilTime *time.Time
	cases := []struct {
		a, b any
		want bool
	}{
		{now, local, true},
		{&now, &local, true},
		{nilTime, nilTime, true},
		{nilTime, &now, false},
		{now, now.Add(time.Hour), false},
		{"a", "a", true},
		{"a", "b", false},
		{[]string{"x"}, []string{"x"}, true},
	}
	for i, c := range cases {
		if got := sameValue(c.a, c.b); got != c.want {
			t.Errorf("case %d: sameValue(%v, %v) = %v", i, c.a, c.b, got)
		}
	}
}

func TestHelpers(t *testing.T) {
	o := Opts("a", "A", "b", "B", "dangling")
	if len(o) != 2 || o[1].Value != "b" || o[1].Label != "B" {
		t.Fatalf("Opts = %+v", o)
	}
	if c := StatusCol("status", "Status", o); c.CellStyle != "status" || !c.Filterable || !c.Sortable {
		t.Fatalf("StatusCol = %+v", c)
	}
	if f := Select("s", "S", true, o); f.Type != "select" || !f.Required || len(f.Options) != 2 {
		t.Fatalf("Select = %+v", f)
	}
}
