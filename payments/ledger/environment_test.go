package ledger

import (
	"testing"

	"github.com/asteby/metacore-kernel/capability"
	"github.com/asteby/metacore-kernel/payments"
	"gorm.io/gorm"
)

// The provider's x_environment (extension of the payment outputs) must reach
// the subscribers of commerce.payments.confirmed when the ledger republishes
// the update, and only as the exact words "sandbox" / "production".
func TestConfirmedKeepsProviderEnvironment(t *testing.T) {
	eachDialect(t, func(t *testing.T, db *gorm.DB) {
		cases := []struct {
			name     string
			env      string
			testMode bool
			want     any
		}{
			{"production", "production", false, "production"},
			{"sandbox", "sandbox", true, "sandbox"},
			{"unknown word is dropped", "live", false, nil},
			{"production never rides next to test_mode", "production", true, nil},
			{"absent", "", false, nil},
		}
		for i, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				h := newHarness(t, db)
				h.fake.on("mercadopago", capability.CommercePaymentsTerminalCreate, func(map[string]any) (any, error) {
					return map[string]any{"provider_ref": "ORDENV", "status": "at_device"}, nil
				})
				if _, err := h.l.StartCharge(ctx, h.org, terminalCharge("env-"+string(rune('a'+i)))); err != nil {
					t.Fatal(err)
				}
				o := payments.Outcome{OrderRef: "SO-1", Status: payments.StatusPaid, Amount: "100.00", Currency: "MXN",
					ProviderRef: "ORDENV", EventID: "evt-env", TestMode: c.testMode, XEnvironment: c.env}
				if _, err := h.l.Observe(ctx, h.org, "mercadopago", "", o, payments.SourceWebhook); err != nil {
					t.Fatal(err)
				}
				ev := h.confirmed()
				if len(ev) == 0 {
					t.Fatal("no commerce.payments.confirmed published")
				}
				result, _ := ev[len(ev)-1].payload["result"].(map[string]any)
				if got := result["x_environment"]; got != c.want {
					t.Fatalf("x_environment = %v, want %v (result %v)", got, c.want, result)
				}
			})
		}
	})
}
