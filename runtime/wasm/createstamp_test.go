package wasm

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

func TestStampCreate(t *testing.T) {
	req := &dataMutateRequest{Op: "create", Model: "SalesOrderItem"}
	row := map[string]any{"unit_price": 40.0}

	// Unset, wrong op or no model: untouched.
	if code, err := stampCreate(context.Background(), &invocation{}, nil, uuid.New(), req, row); code != "" || err != nil {
		t.Fatalf("sin stamper: %q %v", code, err)
	}
	calls := 0
	inv := &invocation{createStamp: func(_ context.Context, _ *gorm.DB, _ uuid.UUID, model string, r map[string]any) error {
		calls++
		r["min_price_at_sale"] = 50.0
		return nil
	}}
	if _, err := stampCreate(context.Background(), inv, nil, uuid.New(), &dataMutateRequest{Op: "update", Model: "X"}, row); err != nil || calls != 0 {
		t.Fatalf("update no se estampa: calls=%d err=%v", calls, err)
	}
	if _, err := stampCreate(context.Background(), inv, nil, uuid.New(), &dataMutateRequest{Op: "create"}, row); err != nil || calls != 0 {
		t.Fatalf("sin model no se estampa: calls=%d", calls)
	}
	if code, err := stampCreate(context.Background(), inv, nil, uuid.New(), req, row); code != "" || err != nil || row["min_price_at_sale"] != 50.0 {
		t.Fatalf("estampa: %q %v %v", code, err, row)
	}

	// Error mapping.
	refused := &invocation{createStamp: func(context.Context, *gorm.DB, uuid.UUID, string, map[string]any) error {
		return fmt.Errorf("%w: already used", dynamic.ErrApprovalConsume)
	}}
	if code, _ := stampCreate(context.Background(), refused, nil, uuid.New(), req, row); code != "approval_invalid" {
		t.Fatalf("code = %q", code)
	}
	broken := &invocation{createStamp: func(context.Context, *gorm.DB, uuid.UUID, string, map[string]any) error {
		return errors.New("db down")
	}}
	if code, _ := stampCreate(context.Background(), broken, nil, uuid.New(), req, row); code != "db_error" {
		t.Fatalf("code = %q", code)
	}
}

func TestUpdateStampFor(t *testing.T) {
	req := &dataMutateRequest{Op: "update", Model: "SalesOrderItem", ID: "line1"}
	if f := updateStampFor(context.Background(), &invocation{}, nil, uuid.New(), req); f != nil {
		t.Fatal("sin stamper no hay cierre")
	}
	var gotID string
	inv := &invocation{updateStamp: func(_ context.Context, _ *gorm.DB, _ uuid.UUID, model, id string, before, data, inc map[string]any) error {
		gotID = id
		data["price_authorized"] = 0.0
		return nil
	}}
	if f := updateStampFor(context.Background(), inv, nil, uuid.New(), &dataMutateRequest{Op: "create", Model: "X"}); f != nil {
		t.Fatal("un create no usa el stamp de update")
	}
	data := map[string]any{"unit_price": 1.0}
	f := updateStampFor(context.Background(), inv, nil, uuid.New(), req)
	if code, err := f(map[string]any{}, data, nil); code != "" || err != nil || gotID != "line1" || data["price_authorized"] != 0.0 {
		t.Fatalf("%q %v %v %v", code, err, gotID, data)
	}

	for want, e := range map[string]error{
		"approval_invalid": fmt.Errorf("%w: used", dynamic.ErrApprovalConsume),
		"validation_error": fmt.Errorf("%w: gone", dynamic.ErrSnapshotSource),
		"db_error":         errors.New("down"),
	} {
		e := e
		bad := &invocation{updateStamp: func(context.Context, *gorm.DB, uuid.UUID, string, string, map[string]any, map[string]any, map[string]any) error { return e }}
		if code, _ := updateStampFor(context.Background(), bad, nil, uuid.New(), req)(nil, nil, nil); code != want {
			t.Errorf("%v: code=%q want %q", e, code, want)
		}
	}
	// The create path maps a missing snapshot source the same way.
	missing := &invocation{createStamp: func(context.Context, *gorm.DB, uuid.UUID, string, map[string]any) error {
		return fmt.Errorf("%w: p-other", dynamic.ErrSnapshotSource)
	}}
	if code, _ := stampCreate(context.Background(), missing, nil, uuid.New(), &dataMutateRequest{Op: "create", Model: "X"}, map[string]any{}); code != "validation_error" {
		t.Fatalf("code = %q", code)
	}
}
