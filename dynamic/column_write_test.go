package dynamic

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/manifest"
	"github.com/asteby/metacore-kernel/metadata"
	"github.com/asteby/metacore-kernel/modelbase"
)

const wpPerm = "inv.item.set_sku"

type WpItem struct {
	modelbase.BaseUUIDModel
	Name string `json:"name" gorm:"size:255"`
	Sku  string `json:"sku" gorm:"size:255"`
}

func (WpItem) TableName() string { return "val_items" }
func (WpItem) DefineTable() modelbase.TableMetadata {
	return modelbase.TableMetadata{Title: "Items", Columns: []modelbase.ColumnDef{
		{Key: "name", Label: "Name"},
		{Key: "sku", Label: "SKU", WritePermission: wpPerm},
	}}
}
func (WpItem) DefineModal() modelbase.ModalMetadata { return modelbase.ModalMetadata{Title: "Item"} }

func writeGateService(t *testing.T, holders map[uuid.UUID]bool) *Service {
	t.Helper()
	db := setupValidationDB(t)
	modelbase.Register("wp_items", func() modelbase.ModelDefiner { return &WpItem{} })
	return New(Config{
		AllowUnconfiguredPermissions: true,
		DB:                           db,
		Metadata:                     metadata.New(metadata.Config{CacheTTL: -1}),
		ValidationSchemaResolver: func(_ context.Context, model string) ([]manifest.ColumnDef, bool) {
			return []manifest.ColumnDef{
				{Name: "name", Type: "string"},
				{Name: "sku", Type: "string", WritePermission: wpPerm},
			}, model == "wp_items"
		},
		ColumnReadChecker: func(_ context.Context, u modelbase.AuthUser, perm string) bool {
			return perm == wpPerm && holders[u.GetID()]
		},
	})
}

func TestWritePermission_CreateUpdate(t *testing.T) {
	org := uuid.New()
	boss, clerk := newUser(org), newUser(org)
	svc := writeGateService(t, map[uuid.UUID]bool{boss.id: true})
	ctx := context.Background()

	// The holder may set the gated column.
	row, err := svc.Create(ctx, "wp_items", boss, map[string]any{"name": "a", "sku": "S1"})
	if err != nil {
		t.Fatalf("holder create: %v", err)
	}
	id := row["id"].(string)

	// A caller without it is refused when they send a value on create...
	var denied *ColumnWriteDeniedError
	if _, err := svc.Create(ctx, "wp_items", clerk, map[string]any{"name": "b", "sku": "S2"}); !errors.As(err, &denied) || denied.Column != "sku" {
		t.Fatalf("want ColumnWriteDeniedError(sku), got %v", err)
	} else if !errors.Is(err, ErrForbidden) {
		t.Fatal("must answer 403 (wrap ErrForbidden)")
	}
	// ...but may create without touching it.
	if _, err := svc.Create(ctx, "wp_items", clerk, map[string]any{"name": "c"}); err != nil {
		t.Fatalf("clerk create without the gated column: %v", err)
	}

	uid := uuid.MustParse(id)
	// Update: changing it is refused; echoing the persisted value is not.
	if _, err := svc.Update(ctx, "wp_items", clerk, uid, map[string]any{"sku": "HACK"}); !errors.As(err, &denied) {
		t.Fatalf("clerk update must be denied, got %v", err)
	}
	if _, err := svc.Update(ctx, "wp_items", clerk, uid, map[string]any{"name": "renamed", "sku": "S1"}); err != nil {
		t.Fatalf("echoing the persisted value must not fail the save: %v", err)
	}
	got, err := svc.Get(ctx, "wp_items", clerk, uid)
	if err != nil || got["sku"] != "S1" || got["name"] != "renamed" {
		t.Fatalf("row after echo update: %v %v", got, err)
	}
	// The holder can change it.
	if _, err := svc.Update(ctx, "wp_items", boss, uid, map[string]any{"sku": "S9"}); err != nil {
		t.Fatalf("holder update: %v", err)
	}
	// A system caller is never gated.
	if _, err := svc.Update(ctx, "wp_items", NewSystemCaller(org), uid, map[string]any{"sku": "S10"}); err != nil {
		t.Fatalf("system caller: %v", err)
	}
}

func TestWritePermission_ColumnWriteMask(t *testing.T) {
	org := uuid.New()
	boss, clerk := newUser(org), newUser(org)
	svc := writeGateService(t, map[uuid.UUID]bool{boss.id: true})
	ctx := context.Background()
	if cols, err := svc.ColumnWriteMask(ctx, "wp_items", clerk); err != nil || len(cols) != 1 || cols[0] != "sku" {
		t.Fatalf("clerk mask = %v %v", cols, err)
	}
	if cols, _ := svc.ColumnWriteMask(ctx, "wp_items", boss); len(cols) != 0 {
		t.Fatalf("holder mask = %v", cols)
	}
}
