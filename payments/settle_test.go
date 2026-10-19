package payments

import (
	"errors"
	"testing"
)

func TestCheckTendersAndLines(t *testing.T) {
	if err := CheckTenders("MXN", "1500.00", "1000", "500.00"); err != nil {
		t.Fatalf("split tender: %v", err)
	}
	// A row with an amount but no method was dropped by the client: the
	// declared total no longer matches (7leguas 649f6197f).
	if err := CheckTenders("MXN", "1500.00", "1000"); !errors.Is(err, ErrTenderMismatch) {
		t.Fatalf("mismatch: %v", err)
	}
	if err := CheckTenders("MXN", "0", "0"); !errors.Is(err, ErrTenderMismatch) {
		t.Fatal("zero tender accepted")
	}
	if err := CheckTenders("MXN", "10.005", "10.005"); !errors.Is(err, ErrNotQuantized) {
		t.Fatalf("sub-cent tender: %v", err)
	}
	if err := CheckTenders("JPY", "100.50", "100.50"); !errors.Is(err, ErrNotQuantized) {
		t.Fatalf("yen with decimals: %v", err)
	}
	// What is charged must be the sum of the stored lines (7leguas order 71621).
	if err := CheckLinesTotal("2349.80", "1174.90", "1174.90"); err != nil {
		t.Fatal(err)
	}
	if err := CheckLinesTotal("2349.80", "1174.90"); !errors.Is(err, ErrLinesMismatch) {
		t.Fatalf("lines: %v", err)
	}
}

func TestSettle(t *testing.T) {
	cases := []struct {
		name                           string
		total, paid, nonCash, cash     string
		tol                            string
		settled                        bool
		applied, change, round, remain string
		err                            error
	}{
		{"exact card", "100.00", "", "100", "", "", true, "100.00", "0.00", "0.00", "0.00", nil},
		{"cash change", "100.00", "", "", "200", "0.50", true, "100.00", "100.00", "0.00", "0.00", nil},
		{"card over", "100.00", "", "100.01", "", "", false, "", "", "", "", ErrExceedsTotal},
		// r1/r4 B1: an overcharged card is never paid back in cash.
		{"card over with cash", "500.00", "", "600", "10", "", false, "", "", "", "", ErrExceedsTotal},
		{"mixed exact", "500.00", "", "450", "50", "", true, "500.00", "0.00", "0.00", "0.00", nil},
		{"mixed change only from cash", "500.00", "", "450", "60", "", true, "500.00", "10.00", "0.00", "0.00", nil},
		// The recycler cannot return coins below its smallest one and closes
		// $0.49 short (7leguas a8f3e6df6).
		{"cash rounding", "100.49", "", "", "100", "0.50", true, "100.00", "0.00", "0.49", "0.00", nil},
		{"cash rounding at limit", "100.50", "", "", "100", "0.50", true, "100.00", "0.00", "0.50", "0.00", nil},
		{"cash short", "100.51", "", "", "100", "0.50", false, "100.00", "0.00", "0.00", "0.51", nil},
		{"card never rounds", "100.49", "", "100", "", "0.50", false, "100.00", "0.00", "0.00", "0.49", nil},
		{"tolerance off", "100.49", "", "", "100", "0", false, "100.00", "0.00", "0.00", "0.49", nil},
		{"second tender", "300", "200", "100", "", "", true, "100.00", "0.00", "0.00", "0.00", nil},
		{"nothing due", "300", "300", "", "1", "", false, "", "", "", "", ErrNothingDue},
		// r1 B2 / r4 I3: a tolerance cannot forgive debts.
		{"tolerance above cap", "500.00", "", "", "100", "1000", false, "", "", "", "", ErrInvalidTolerance},
		{"tolerance one unit", "100.00", "", "", "99", "1.00", false, "", "", "", "", ErrInvalidTolerance},
		{"negative tolerance", "100.00", "", "", "99", "-0.10", false, "", "", "", "", ErrInvalidTolerance},
		{"negative already paid", "100.00", "-50", "", "100", "", false, "", "", "", "", ErrNegativeAmount},
		{"negative cash", "100.00", "", "110", "-10", "", false, "", "", "", "", ErrNegativeAmount},
		{"sub-cent", "100.00", "", "99.995", "", "", false, "", "", "", "", ErrNotQuantized},
	}
	for _, c := range cases {
		s, err := Settle(Collection{Currency: "MXN", Total: c.total, AlreadyPaid: c.paid, NonCash: c.nonCash, Cash: c.cash, CashTolerance: c.tol})
		if c.err != nil {
			if !errors.Is(err, c.err) {
				t.Errorf("%s: err %v, want %v", c.name, err, c.err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if s.Settled != c.settled || s.Applied.String() != c.applied || s.Change.String() != c.change || s.Rounding.String() != c.round || s.Remaining.String() != c.remain {
			t.Errorf("%s: settled=%v applied %s change %s rounding %s remaining %s", c.name, s.Settled, s.Applied, s.Change, s.Rounding, s.Remaining)
		}
		cash := MustAmount(orZero(c.cash))
		if s.Change.Cmp(cash) > 0 {
			t.Errorf("%s: change %s above the cash tendered %s", c.name, s.Change, cash)
		}
	}
	if _, err := Settle(Collection{Currency: "MXN", Total: "100", Cash: "99.5e0"}); err == nil {
		t.Fatal("float collected accepted")
	}
	if MaxCashTolerance("MXN").String() != "0.99" || MaxCashTolerance("JPY").String() != "0.00" || MaxCashTolerance("KWD").String() != "0.999" {
		t.Fatalf("caps %s %s %s", MaxCashTolerance("MXN"), MaxCashTolerance("JPY"), MaxCashTolerance("KWD"))
	}
}

func TestCheckReplay(t *testing.T) {
	paid := Payment{ID: "a", IdempotencyKey: "req-1", Status: StatusPaid, Amount: "100"}
	open := Payment{ID: "b", IdempotencyKey: "req-2", Status: StatusAtDevice, Amount: "100"}
	failed := Payment{ID: "c", IdempotencyKey: "req-3", Status: StatusFailed, Amount: "100"}

	// Timeout retry of a cash charge that did get recorded: replay, no
	// second payment (7leguas 1eb71a72e).
	if v, p, _ := CheckReplay([]Payment{paid}, nil, "req-1", "100"); v != VerdictReplay || p.ID != "a" {
		t.Fatalf("replay: %s %v", v, p)
	}
	// Another point of sale charging a paid document: stop, nothing charged.
	if v, _, _ := CheckReplay([]Payment{paid}, nil, "req-9", "100"); v != VerdictAlreadyPaid {
		t.Fatalf("already paid: %s", v)
	}
	// The same request while its terminal order is live: resume it, never
	// recreate it (MP Point resume).
	if v, p, _ := CheckReplay([]Payment{open}, nil, "req-2", "100"); v != VerdictInProgress || p.ID != "b" {
		t.Fatalf("resume: %s %v", v, p)
	}
	if v, p, _ := CheckReplay([]Payment{failed, open}, nil, "req-9", "100"); v != VerdictInProgress || p.ID != "b" {
		t.Fatalf("in progress elsewhere: %s %v", v, p)
	}
	if v, _, _ := CheckReplay([]Payment{failed}, nil, "req-3", "100"); v != VerdictRetryNewKey {
		t.Fatalf("failed key reuse: %s", v)
	}
	if v, _, _ := CheckReplay([]Payment{failed}, nil, "req-4", "100"); v != VerdictNew {
		t.Fatalf("new attempt: %s", v)
	}
	// Partly paid (layaway / split tender): a new tender may proceed.
	if v, _, _ := CheckReplay([]Payment{paid}, nil, "req-5", "300"); v != VerdictNew {
		t.Fatalf("partial: %s", v)
	}
	// r4 I2: the key is required; the paid amount is computed, not trusted.
	if _, _, err := CheckReplay(nil, nil, "  ", "100"); !errors.Is(err, ErrKeyRequired) {
		t.Fatalf("blank key: %v", err)
	}
	if _, _, err := CheckReplay(nil, nil, "k", "x"); err == nil {
		t.Fatal("invalid total accepted")
	}
	// r1: a partial refund that did not reduce the total does not reopen the
	// debt (no second charge after a goodwill refund) ...
	part := Payment{ID: "a", IdempotencyKey: "req-1", Status: StatusPartiallyRefunded, Amount: "100", AmountRefunded: "30"}
	goodwill := Refund{PaymentID: "a", Amount: "30", Status: RefundSucceeded}
	if v, _, _ := CheckReplay([]Payment{part}, []Refund{goodwill}, "req-7", "100"); v != VerdictAlreadyPaid {
		t.Fatalf("goodwill refund reopened the debt: %s", v)
	}
	// ... while a return that lowered the total is consistent with it.
	ret := Refund{PaymentID: "a", Amount: "30", Status: RefundSucceeded, AdjustsTotal: true}
	if v, _, _ := CheckReplay([]Payment{part}, []Refund{ret}, "req-7", "70"); v != VerdictAlreadyPaid {
		t.Fatalf("return with adjusted total: %s", v)
	}
	if v, _, _ := CheckReplay([]Payment{part}, []Refund{ret}, "req-7", "100"); v != VerdictNew {
		t.Fatalf("return without adjusting the total leaves it due: %s", v)
	}
}
