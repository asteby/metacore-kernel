package payments

import (
	"strings"
	"testing"
)

func validPayment() Payment {
	return Payment{
		ID: "p1", DocumentType: "sales_order", DocumentID: "SO-1", OrderRef: "SO-1",
		Channel: ChannelTerminal, Provider: "mercadopago", ProviderRef: "ORD01", DeviceID: "PAX_A910",
		IdempotencyKey: "SO-1:charge:1", Status: StatusPaid, Amount: "1499.00", Currency: "MXN",
		Fee: "52.47", Net: "1446.53",
		Card: &Card{Type: "debit", Brand: "debmaster", Last4: "0042"},
	}
}

func TestAmounts(t *testing.T) {
	for _, s := range []string{"0", "10", "10.5", "1499.00", "-3.20", "0.0825"} {
		if _, err := ParseAmount(s); err != nil {
			t.Errorf("%q: %v", s, err)
		}
	}
	for _, s := range []string{"", "1e3", "1,000.00", "10.", ".5", "NaN", " 1"} {
		if _, err := ParseAmount(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
	// 0.1 + 0.2 is exactly 0.3: no float drift.
	sum, err := Sum("0.1", "0.2")
	if err != nil || sum.Cmp(MustAmount("0.3")) != 0 || sum.String() != "0.30" {
		t.Fatalf("Sum = %s, %v", sum, err)
	}
	for in, want := range map[string]string{"10": "10.00", "10.5": "10.50", "0.0825": "0.0825", "-0": "0.00", "1499.000": "1499.00"} {
		if got := MustAmount(in).String(); got != want {
			t.Errorf("String(%s) = %s, want %s", in, got, want)
		}
	}
	if MustAmount("5").Sub(MustAmount("5.01")).Sign() != -1 {
		t.Fatal("Sub sign")
	}
}

func TestPaymentValidate(t *testing.T) {
	if errs := validPayment().Validate(); len(errs) != 0 {
		t.Fatalf("valid payment rejected: %v", errs)
	}
	cases := map[string]struct {
		mut  func(*Payment)
		want string
	}{
		"float amount":      {func(p *Payment) { p.Amount = "1.5e3" }, "amount"},
		"zero":              {func(p *Payment) { p.Amount = "0" }, "positive"},
		"no document":       {func(p *Payment) { p.DocumentID = "" }, "document_id"},
		"bad channel":       {func(p *Payment) { p.Channel = "card" }, "channel"},
		"no key":            {func(p *Payment) { p.IdempotencyKey = " " }, "idempotency_key"},
		"currency":          {func(p *Payment) { p.Currency = "mxn" }, "currency"},
		"paid without ref":  {func(p *Payment) { p.ProviderRef = "" }, "provider_ref"},
		"over refunded":     {func(p *Payment) { p.AmountRefunded = "1500" }, "exceeds"},
		"status vs refunds": {func(p *Payment) { p.AmountRefunded = "100" }, "does not match"},
		"refund on pending": {func(p *Payment) { p.Status = StatusPending; p.AmountRefunded = "1" }, "cannot have refunds"},
		"net":               {func(p *Payment) { p.Net = "1446.52" }, "net"},
		"last4":             {func(p *Payment) { p.Card.Last4 = "42" }, "last4"},
		"card type":         {func(p *Payment) { p.Card.Type = "amex" }, "card.type"},
		"drawer not cash":   {func(p *Payment) { p.Channel = ChannelCash }, "provider"},
		"cash as online":    {func(p *Payment) { p.Provider = ProviderCash }, "provider"},
		"seller fee":        {func(p *Payment) { p.Seller = &Seller{AccountRef: "acc", ApplicationFee: "2000"} }, "application_fee"},
		"seller account":    {func(p *Payment) { p.Seller = &Seller{} }, "account_ref"},
	}
	for name, c := range cases {
		p := validPayment()
		c.mut(&p)
		errs := p.Validate()
		if !strings.Contains(strings.Join(errs, "; "), c.want) {
			t.Errorf("%s: want %q in %v", name, c.want, errs)
		}
	}
	p := validPayment()
	p.AmountRefunded, p.Status = "1499.00", StatusRefunded
	if errs := p.Validate(); len(errs) != 0 {
		t.Fatalf("fully refunded rejected: %v", errs)
	}
	cash := Payment{DocumentType: "sales_order", DocumentID: "SO-2", Channel: ChannelCash, Provider: ProviderCash,
		ProviderRef: "mov-1", IdempotencyKey: "k", Status: StatusPaid, Amount: "100", Currency: "MXN",
		Cash: &CashTender{Tendered: "200", Change: "100"}}
	if errs := cash.Validate(); len(errs) != 0 {
		t.Fatalf("drawer cash rejected: %v", errs)
	}
}

func TestNetCaptured(t *testing.T) {
	p := validPayment()
	p.AmountRefunded, p.Status = "499.00", StatusPartiallyRefunded
	n, err := p.NetCaptured()
	if err != nil || n.String() != "1000.00" {
		t.Fatalf("NetCaptured = %s, %v", n, err)
	}
	p.Status = StatusAuthorized
	if n, _ := p.NetCaptured(); n.Sign() != 0 {
		t.Fatal("authorized does not count")
	}
}

// r4 I4 / r1 punto 4: bounded scale, exact rendering, per-currency quantum.
func TestMoneyPrecision(t *testing.T) {
	if _, err := ParseAmount("0.000000001"); err == nil {
		t.Fatal("more than MaxScale decimals accepted")
	}
	if got := MustAmount("0.000001").String(); got != "0.000001" {
		t.Fatalf("String lost precision: %s", got)
	}
	for _, c := range []struct {
		s, cur string
		ok     bool
	}{
		{"10.00", "MXN", true}, {"10.005", "MXN", false}, {"100", "JPY", true}, {"100.5", "JPY", false},
		{"1.234", "KWD", true}, {"1.2345", "KWD", false}, {"10.000", "MXN", true}, {"10", "mx", false},
	} {
		if _, err := ParseMoney(c.s, c.cur); (err == nil) != c.ok {
			t.Errorf("ParseMoney(%s, %s) = %v", c.s, c.cur, err)
		}
	}
	p := validPayment()
	p.Amount, p.Fee, p.Net = "10.123", "", ""
	if errs := p.Validate(); len(errs) == 0 {
		t.Fatal("sub-cent payment amount accepted")
	}
}
