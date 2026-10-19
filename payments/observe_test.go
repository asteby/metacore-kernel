package payments

import "testing"

// r4 B3: a re-read is compared on money, not only on status.
func TestObserveOutcome(t *testing.T) {
	paid := Payment{ID: "p1", Provider: "mercadopago", ProviderRef: "ORD01", Status: StatusPaid, Amount: "100.00", Currency: "MXN"}
	part := paid
	part.Status, part.AmountRefunded = StatusPartiallyRefunded, "30.00"
	pending := paid
	pending.Status = StatusPending

	cases := []struct {
		name   string
		stored Payment
		o      Outcome
		want   Decision
		reason string
	}{
		{"capture applies", pending, Outcome{Status: StatusPaid, Amount: "100", Currency: "MXN", ProviderRef: "ORD01"}, Apply, ""},
		{"capture of another amount", pending, Outcome{Status: StatusPaid, Amount: "10.00", Currency: "MXN", ProviderRef: "ORD01"}, Anomaly, ReasonAmountMismatch},
		{"authorization of another amount", pending, Outcome{Status: StatusAuthorized, Amount: "99.99", Currency: "MXN"}, Anomaly, ReasonAmountMismatch},
		{"another currency", pending, Outcome{Status: StatusPaid, Amount: "100", Currency: "USD"}, Anomaly, ReasonCurrencyMismatch},
		{"another provider payment", pending, Outcome{Status: StatusPaid, Amount: "100", Currency: "MXN", ProviderRef: "ORD99"}, Anomaly, ReasonProviderRefMismatch},
		{"same paid again", paid, Outcome{Status: StatusPaid, Amount: "100", Currency: "MXN"}, Ignore, ReasonDuplicate},
		{"partial refund grows", part, Outcome{Status: StatusPartiallyRefunded, Amount: "100", AmountRefunded: "50", Currency: "MXN"}, Apply, ""},
		{"partial refund same total", part, Outcome{Status: StatusPartiallyRefunded, Amount: "100", AmountRefunded: "30", Currency: "MXN"}, Ignore, ReasonDuplicate},
		{"partial refund shrinks", part, Outcome{Status: StatusPartiallyRefunded, Amount: "100", AmountRefunded: "20", Currency: "MXN"}, Anomaly, ReasonRefundReversed},
		{"refunded above amount", part, Outcome{Status: StatusRefunded, Amount: "100", AmountRefunded: "150", Currency: "MXN"}, Anomaly, ReasonAmountMismatch},
		{"status contradicts total", paid, Outcome{Status: StatusRefunded, Amount: "100", AmountRefunded: "40", Currency: "MXN"}, Anomaly, ReasonAmountMismatch},
		{"panel refund observed", paid, Outcome{Status: StatusPartiallyRefunded, Amount: "100", AmountRefunded: "60", Currency: "MXN"}, Apply, ""},
		{"refund event adds up", part, Outcome{Kind: KindRefund, Status: StatusRefunded, Amount: "70", Currency: "MXN"}, Apply, ""},
		{"paid after partial refund", part, Outcome{Status: StatusPaid, Amount: "100", Currency: "MXN"}, Anomaly, ReasonRefundReversed},
		{"float-like amount", pending, Outcome{Status: StatusPaid, Amount: "1e2", Currency: "MXN"}, Anomaly, ReasonInvalidOutcome},
		{"unknown status", paid, Outcome{Status: "approved"}, Anomaly, ReasonUnknownStatus},
	}
	for _, c := range cases {
		got := ObserveOutcome(c.stored, c.o)
		if got.Decision != c.want || got.Reason != c.reason {
			t.Errorf("%s: %+v, want %s %q", c.name, got, c.want, c.reason)
		}
	}
}
