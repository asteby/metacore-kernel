package database_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/config"
	"github.com/asteby/metacore-kernel/database"
)

// modelWithCurrency mirrors the addon shape that motivated the hook:
// a tenant-scoped row with a CurrencyCode string column. The GORM tags
// are intentionally minimal — the whole point of the hook is to remove
// the `default:'XXX'` tag from addon models.
type modelWithCurrency struct {
	ID             uuid.UUID `gorm:"type:text;primary_key"`
	OrganizationID uuid.UUID `gorm:"type:text;index"`
	CurrencyCode   string    `gorm:"size:10"`
}

func (modelWithCurrency) TableName() string { return "models_with_currency" }

// modelWithMoneda exercises the alternate field name used by the
// waybill-cartaporte CFDI model (SAT dictates Spanish naming there).
type modelWithMoneda struct {
	ID             uuid.UUID `gorm:"type:text;primary_key"`
	OrganizationID uuid.UUID `gorm:"type:text;index"`
	Moneda         string    `gorm:"size:3"`
}

func (modelWithMoneda) TableName() string { return "models_with_moneda" }

// modelNoCurrency is the negative control: a model with no currency
// field. The hook must be a no-op for it.
type modelNoCurrency struct {
	ID             uuid.UUID `gorm:"type:text;primary_key"`
	OrganizationID uuid.UUID `gorm:"type:text;index"`
	Name           string    `gorm:"size:50"`
}

func (modelNoCurrency) TableName() string { return "models_no_currency" }

// openCurrencyDB returns an in-memory sqlite session with the schema and
// callback wired. Each test gets its own DB so callback state is fresh.
func openCurrencyDB(t *testing.T, fallback string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&modelWithCurrency{}, &modelWithMoneda{}, &modelNoCurrency{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	if err := database.RegisterCurrencyDefaultCallback(db, fallback); err != nil {
		t.Fatalf("register callback: %v", err)
	}
	return db
}

func TestCurrencyHook_GetterResolves(t *testing.T) {
	orgID := uuid.New()
	db := openCurrencyDB(t, database.CurrencyHookFallback)

	getter := config.OrgCurrencyGetter(func(_ context.Context, id uuid.UUID) (string, bool) {
		if id == orgID {
			return "COP", true
		}
		return "", false
	})
	ctx := config.WithCurrencyGetter(context.Background(), getter)

	m := &modelWithCurrency{ID: uuid.New(), OrganizationID: orgID}
	if err := db.WithContext(ctx).Create(m).Error; err != nil {
		t.Fatalf("create: %v", err)
	}

	var got modelWithCurrency
	if err := db.First(&got, "id = ?", m.ID).Error; err != nil {
		t.Fatalf("requery: %v", err)
	}
	if got.CurrencyCode != "COP" {
		t.Fatalf("currency_code: got %q want COP", got.CurrencyCode)
	}
}

func TestCurrencyHook_FallbackWhenNoGetter(t *testing.T) {
	db := openCurrencyDB(t, database.CurrencyHookFallback)

	m := &modelWithCurrency{ID: uuid.New(), OrganizationID: uuid.New()}
	if err := db.Create(m).Error; err != nil {
		t.Fatalf("create: %v", err)
	}

	var got modelWithCurrency
	_ = db.First(&got, "id = ?", m.ID).Error
	if got.CurrencyCode != "USD" {
		t.Fatalf("currency_code: got %q want USD (fallback)", got.CurrencyCode)
	}
}

func TestCurrencyHook_FallbackWhenGetterReportsMiss(t *testing.T) {
	db := openCurrencyDB(t, "EUR")

	getter := config.OrgCurrencyGetter(func(_ context.Context, _ uuid.UUID) (string, bool) {
		return "", false // org not configured
	})
	ctx := config.WithCurrencyGetter(context.Background(), getter)

	m := &modelWithCurrency{ID: uuid.New(), OrganizationID: uuid.New()}
	if err := db.WithContext(ctx).Create(m).Error; err != nil {
		t.Fatalf("create: %v", err)
	}

	var got modelWithCurrency
	_ = db.First(&got, "id = ?", m.ID).Error
	if got.CurrencyCode != "EUR" {
		t.Fatalf("currency_code: got %q want EUR (custom fallback)", got.CurrencyCode)
	}
}

func TestCurrencyHook_PreservesExplicitValue(t *testing.T) {
	db := openCurrencyDB(t, database.CurrencyHookFallback)

	getter := config.OrgCurrencyGetter(func(_ context.Context, _ uuid.UUID) (string, bool) {
		return "MXN", true // org default would say MXN …
	})
	ctx := config.WithCurrencyGetter(context.Background(), getter)

	// … but the caller explicitly passes COP — that wins.
	m := &modelWithCurrency{ID: uuid.New(), OrganizationID: uuid.New(), CurrencyCode: "COP"}
	if err := db.WithContext(ctx).Create(m).Error; err != nil {
		t.Fatalf("create: %v", err)
	}

	var got modelWithCurrency
	_ = db.First(&got, "id = ?", m.ID).Error
	if got.CurrencyCode != "COP" {
		t.Fatalf("currency_code: got %q want COP (explicit value)", got.CurrencyCode)
	}
}

func TestCurrencyHook_MonedaFieldName(t *testing.T) {
	orgID := uuid.New()
	db := openCurrencyDB(t, database.CurrencyHookFallback)

	getter := config.OrgCurrencyGetter(func(_ context.Context, _ uuid.UUID) (string, bool) {
		return "MXN", true // CFDI carta porte is Mexico-specific anyway
	})
	ctx := config.WithCurrencyGetter(context.Background(), getter)

	m := &modelWithMoneda{ID: uuid.New(), OrganizationID: orgID}
	if err := db.WithContext(ctx).Create(m).Error; err != nil {
		t.Fatalf("create: %v", err)
	}

	var got modelWithMoneda
	_ = db.First(&got, "id = ?", m.ID).Error
	if got.Moneda != "MXN" {
		t.Fatalf("moneda: got %q want MXN", got.Moneda)
	}
}

func TestCurrencyHook_NoCurrencyFieldIsNoop(t *testing.T) {
	db := openCurrencyDB(t, database.CurrencyHookFallback)

	m := &modelNoCurrency{ID: uuid.New(), OrganizationID: uuid.New(), Name: "x"}
	if err := db.Create(m).Error; err != nil {
		t.Fatalf("create: %v", err)
	}
}

func TestCurrencyHook_BatchInsert(t *testing.T) {
	orgA := uuid.New()
	orgB := uuid.New()
	db := openCurrencyDB(t, database.CurrencyHookFallback)

	getter := config.OrgCurrencyGetter(func(_ context.Context, id uuid.UUID) (string, bool) {
		switch id {
		case orgA:
			return "MXN", true
		case orgB:
			return "COP", true
		}
		return "", false
	})
	ctx := config.WithCurrencyGetter(context.Background(), getter)

	rows := []modelWithCurrency{
		{ID: uuid.New(), OrganizationID: orgA},
		{ID: uuid.New(), OrganizationID: orgB},
		{ID: uuid.New(), OrganizationID: orgA, CurrencyCode: "EUR"}, // explicit wins
	}
	if err := db.WithContext(ctx).Create(&rows).Error; err != nil {
		t.Fatalf("batch create: %v", err)
	}

	for i, want := range []string{"MXN", "COP", "EUR"} {
		var got modelWithCurrency
		if err := db.First(&got, "id = ?", rows[i].ID).Error; err != nil {
			t.Fatalf("requery row %d: %v", i, err)
		}
		if got.CurrencyCode != want {
			t.Fatalf("row %d currency_code: got %q want %q", i, got.CurrencyCode, want)
		}
	}
}

func TestRegisterCurrencyDefaultCallback_DefaultFallbackWhenEmpty(t *testing.T) {
	// Passing "" as fallback must fall through to CurrencyHookFallback.
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&modelWithCurrency{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	if err := database.RegisterCurrencyDefaultCallback(db, ""); err != nil {
		t.Fatalf("register: %v", err)
	}

	m := &modelWithCurrency{ID: uuid.New(), OrganizationID: uuid.New()}
	if err := db.Create(m).Error; err != nil {
		t.Fatalf("create: %v", err)
	}

	var got modelWithCurrency
	_ = db.First(&got, "id = ?", m.ID).Error
	if got.CurrencyCode != database.CurrencyHookFallback {
		t.Fatalf("currency_code: got %q want %q", got.CurrencyCode, database.CurrencyHookFallback)
	}
}

func TestRegisterCurrencyDefaultCallback_IdempotentRegistration(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := database.RegisterCurrencyDefaultCallback(db, "USD"); err != nil {
		t.Fatalf("first register: %v", err)
	}
	// Second call must not error (GORM upserts by name).
	if err := database.RegisterCurrencyDefaultCallback(db, "USD"); err != nil {
		t.Fatalf("second register: %v", err)
	}
}
