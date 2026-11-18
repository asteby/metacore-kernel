package inbound

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/metadata"
	"github.com/asteby/metacore-kernel/modelbase"
)

// omitRow has a guard-candidate column with omitempty: at value 0 it vanishes
// from the row as JSON and would be read as NULL (never stale).
type omitRow struct {
	modelbase.BaseUUIDModel
	Sku  string `json:"sku" gorm:"size:64"`
	Ver  int    `json:"ver,omitempty"`
	Seen int    `json:"seen"`
}

func (omitRow) TableName() string { return "inbound_guard_omit" }
func (omitRow) DefineTable() modelbase.TableMetadata {
	return modelbase.TableMetadata{
		Title:   "Omit",
		Columns: []modelbase.ColumnDef{{Key: "sku", Label: "SKU"}, {Key: "ver", Label: "Ver"}, {Key: "seen", Label: "Seen"}},
	}
}
func (omitRow) DefineModal() modelbase.ModalMetadata { return modelbase.ModalMetadata{Title: "Omit"} }

// A guard on a column the row hides at its zero value is refused when the
// contract is registered (guard_field_not_comparable), not on the first event.
func TestGuardContractRejectsOmitemptyColumn(t *testing.T) {
	db := openSQLite(t)
	modelbase.Register("inbound_guard_omit", func() modelbase.ModelDefiner { return &omitRow{} })
	svc := dynamic.New(dynamic.Config{AllowUnconfiguredPermissions: true, DB: db, Metadata: metadata.New(metadata.Config{CacheTTL: -1})})
	d := &DynamicExecutor{Service: svc}
	ctx := context.Background()
	mk := func(field string) Action {
		return Action{Do: ActionRecordDelete, Model: "inbound_guard_omit",
			Guard: &Guard{Field: field, Path: "payload.v", Op: GuardGTE}}
	}

	err := d.checkGuard(ctx, "k.actions[0]", mk("ver"))
	if !errors.Is(err, ErrInvalidContract) || !strings.Contains(err.Error(), "guard_field_not_comparable") {
		t.Fatalf("omitempty guard column must be refused at registration: %v", err)
	}
	if err := d.checkGuard(ctx, "k.actions[0]", mk("seen")); err != nil {
		t.Fatalf("a plain column must pass: %v", err)
	}
}
