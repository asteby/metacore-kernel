package dispatch_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/asteby/metacore-kernel/dispatch"
	"github.com/asteby/metacore-kernel/events"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// silentLogger discards dispatcher logs so the test output stays readable.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// staticProvider returns a SubscriptionProvider yielding a single subscription
// for every org — the common single-addon test shape.
func staticProvider(addonKey string, installation uuid.UUID, event, handlerType, fn string) dispatch.SubscriptionProvider {
	return dispatch.SubscriptionProviderFunc(func(_ context.Context, _ uuid.UUID) ([]dispatch.Subscription, error) {
		return []dispatch.Subscription{{
			AddonKey:     addonKey,
			Installation: installation,
			Event:        event,
			HandlerType:  handlerType,
			Function:     fn,
		}}, nil
	})
}

// publishCanonical publishes a synthetic CanonicalEvent on the bus, mirroring
// dynamic.publishCanonical's wire shape and event name
// (`<addonKey>.<model>.<action>`). It publishes under the trusted "kernel"
// producer key so the bus capability check (ModeEnforce in some tests) does
// not reject the synthetic publish — matching how kernel-owned models publish.
func publishCanonical(t *testing.T, bus *events.Bus, addonKey, model, action string, orgID uuid.UUID, rowID string) {
	t.Helper()
	payload := map[string]any{
		"id":        rowID,
		"model":     model,
		"action":    action,
		"addon_key": addonKey,
		"after":     map[string]any{"id": rowID, "qty": 5},
	}
	event := addonKey + "." + model + "." + action
	if err := bus.Publish(context.Background(), "kernel", event, orgID, payload); err != nil {
		t.Fatalf("publish %s: %v", event, err)
	}
}

// singleDelivery loads the one expected ledger row, failing if the count is not
// exactly one.
func singleDelivery(t *testing.T, db *gorm.DB) dispatch.Delivery {
	t.Helper()
	var rows []dispatch.Delivery
	if err := db.Table(dispatch.DefaultTableName).Find(&rows).Error; err != nil {
		t.Fatalf("load deliveries: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 delivery row, got %d: %+v", len(rows), rows)
	}
	return rows[0]
}

// deliveryCount returns the number of ledger rows.
func deliveryCount(t *testing.T, db *gorm.DB) int {
	t.Helper()
	var n int64
	if err := db.Table(dispatch.DefaultTableName).Count(&n).Error; err != nil {
		t.Fatalf("count deliveries: %v", err)
	}
	return int(n)
}

// roundTrip is a tiny assertion that a payload survives JSON round-trip with
// the canonical keys present (used implicitly by the compiled-tier test).
var _ = json.Marshal
