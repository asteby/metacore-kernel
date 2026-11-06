package dynamic

import (
	"context"
	"testing"

	"github.com/asteby/metacore-kernel/metadata"
	"github.com/google/uuid"
)

func TestUpdate_IgnoresIdentityAndTenantFromBody(t *testing.T) {
	db := setupTestDB(t)
	svc := New(Config{
		DB: db, Metadata: metadata.New(metadata.Config{CacheTTL: -1}),
		AllowUnconfiguredPermissions: true,
		UpdateDenyFields:             map[string][]string{"test_products": {"name"}},
	})
	org := uuid.New()
	user := newUser(org)
	out := createProduct(t, svc, user, "widget", 1.0)
	id := uuid.MustParse(out["id"].(string))

	other := uuid.New().String()
	got, err := svc.Update(context.Background(), "test_products", user, id, map[string]any{
		"organization_id": other,
		"id":              uuid.New().String(),
		"name":            "hacked",
		"price":           9.0,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if got["organization_id"] != org.String() {
		t.Errorf("organization_id se movio: %v", got["organization_id"])
	}
	if got["id"] != id.String() {
		t.Errorf("id cambio: %v", got["id"])
	}
	if got["name"] != "widget" {
		t.Errorf("campo vedado se modifico: %v", got["name"])
	}
	if got["price"] != 9.0 {
		t.Errorf("campo permitido no se actualizo: %v", got["price"])
	}
}
