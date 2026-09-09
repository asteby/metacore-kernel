package dispatch

import (
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func forgetTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := Migrate(db, ""); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func TestForgetDeliveries_DropsOnlyThatAddonEvent(t *testing.T) {
	db := forgetTestDB(t)
	keep := Delivery{
		ID: uuid.New(), DeliveryID: "keep", AddonKey: "workshop",
		Event: "warehouse.stock_picked", Status: StatusDelivered,
	}
	drop := Delivery{
		ID: uuid.New(), DeliveryID: "drop", AddonKey: "workshop",
		Event: "customers.SalesOrder.updated", Status: StatusDelivered,
	}
	other := Delivery{
		ID: uuid.New(), DeliveryID: "other", AddonKey: "pos",
		Event: "customers.SalesOrder.updated", Status: StatusDelivered,
	}
	if err := db.Table(DefaultTableName).Create(&[]Delivery{keep, drop, other}).Error; err != nil {
		t.Fatal(err)
	}

	n, err := ForgetDeliveries(db, "", "workshop", "customers.SalesOrder.updated")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("deleted %d, want 1", n)
	}
	var left []Delivery
	if err := db.Table(DefaultTableName).Find(&left).Error; err != nil {
		t.Fatal(err)
	}
	if len(left) != 2 {
		t.Fatalf("left %d rows, want 2", len(left))
	}
}
