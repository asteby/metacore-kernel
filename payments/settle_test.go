package payments

import (
	"errors"
	"testing"
)

func TestCheckTendersAndLines(t *testing.T) {
	if err := CheckTenders("1500.00", "1000", "500.00"); err != nil {
		t.Fatalf("split tender: %v", err)
	}
	// A row with an amount but no method was dropped by the client: the
	// declared total no longer matches (7leguas 649f6197f).
	if err := CheckTenders("1500.00", "1000"); !errors.Is(err, ErrTenderMismatch) {
		t.Fatalf("mismatch: %v", err)
	}
	if err := CheckTenders("0", "0"); !errors.Is(err, ErrTenderMismatch) {
		t.Fatal("zero tender accepted")
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
		name                       string
		total, paid, collected     string
		cash                       bool
		tol                        string
		settled                    bool
		applied, change, round, re string
		err                        error
	}{
		{"exact card", "100.00", "", "100", false, "", true, "100.00", "0.00", "0.00", "0.00", nil},
		{"cash change", "100.00", "", "200", true, "0.50", true, "100.00", "100.00", "0.00", "0.00", nil},
		{"card over", "100.00", "", "100.01", false, "", false, "", "", "", "", ErrExceedsTotal},
		// The recycler cannot return coins below its smallest one and closes
		// $0.49 short (7leguas a8f3e6df6).
		{"cash rounding", "100.49", "", "100", true, "0.50", true, "100.00", "0.00", "0.49", "0.00", nil},
		{"cash rounding at limit", "100.50", "", "100", true, "0.50", true, "100.00", "0.00", "0.50", "0.00", nil},
		{"cash short", "100.51", "", "100", true, "0.50", false, "100.00", "0.00", "0.00", "0.51", nil},
		{"card never rounds", "100.49", "", "100", false, "0.50", false, "100.00", "0.00", "0.00", "0.49", nil},
		{"tolerance off", "100.49", "", "100", true, "0", false, "100.00", "0.00", "0.00", "0.49", nil},
		{"second tender", "300", "200", "100", false, "", true, "100.00", "0.00", "0.00", "0.00", nil},
		{"nothing due", "300", "300", "1", true, "", false, "", "", "", "", ErrNothingDue},
	}
	for _, c := range cases {
		s, err := Settle(c.total, c.paid, c.collected, c.cash, c.tol)
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
		if s.Settled != c.settled || s.Applied.String() != c.applied || s.Change.String() != c.change || s.Rounding.String() != c.round || s.Remaining.String() != c.re {
			t.Errorf("%s: %+v (applied %s change %s rounding %s remaining %s)", c.name, s.Settled, s.Applied, s.Change, s.Rounding, s.Remaining)
		}
	}
	if _, err := Settle("100", "", "99.5e0", true, ""); err == nil {
		t.Fatal("float collected accepted")
	}
}

func TestCheckReplay(t *testing.T) {
	paid := Payment{ID: "a", IdempotencyKey: "req-1", Status: StatusPaid}
	open := Payment{ID: "b", IdempotencyKey: "req-2", Status: StatusAtDevice}
	failed := Payment{ID: "c", IdempotencyKey: "req-3", Status: StatusFailed}

	// Timeout retry of a cash charge that did get recorded: replay, no
	// second payment (7leguas 1eb71a72e).
	if v, p, _ := CheckReplay([]Payment{paid}, "req-1", "100", "100"); v != VerdictReplay || p.ID != "a" {
		t.Fatalf("replay: %s %v", v, p)
	}
	// Another point of sale charging a paid document: stop, nothing charged.
	if v, _, _ := CheckReplay([]Payment{paid}, "req-9", "100", "100"); v != VerdictAlreadyPaid {
		t.Fatalf("already paid: %s", v)
	}
	// The same request while its terminal order is live: resume it, never
	// recreate it (MP Point resume).
	if v, p, _ := CheckReplay([]Payment{open}, "req-2", "0", "100"); v != VerdictInProgress || p.ID != "b" {
		t.Fatalf("resume: %s %v", v, p)
	}
	// A different request while another payment of the document is open.
	if v, p, _ := CheckReplay([]Payment{failed, open}, "req-9", "0", "100"); v != VerdictInProgress || p.ID != "b" {
		t.Fatalf("in progress elsewhere: %s %v", v, p)
	}
	if v, _, _ := CheckReplay([]Payment{failed}, "req-3", "0", "100"); v != VerdictRetryNewKey {
		t.Fatalf("failed key reuse: %s", v)
	}
	if v, _, _ := CheckReplay([]Payment{failed}, "req-4", "0", "100"); v != VerdictNew {
		t.Fatalf("new attempt: %s", v)
	}
	// Partly paid (layaway / split tender): a new tender may proceed.
	if v, _, _ := CheckReplay([]Payment{paid}, "req-5", "100", "300"); v != VerdictNew {
		t.Fatalf("partial: %s", v)
	}
	if _, _, err := CheckReplay(nil, "k", "x", "100"); err == nil {
		t.Fatal("invalid amount accepted")
	}
}
