package payments

import (
	"errors"
	"testing"

	"github.com/asteby/metacore-kernel/capability"
)

// The Go statuses are exactly the wire vocabulary, in its order.
func TestStatusesMatchCapabilityVocabulary(t *testing.T) {
	if len(Statuses) != len(capability.PaymentStatuses) {
		t.Fatalf("Statuses %v vs capability %v", Statuses, capability.PaymentStatuses)
	}
	for i, s := range Statuses {
		if string(s) != capability.PaymentStatuses[i] {
			t.Fatalf("status %d: %s vs %s", i, s, capability.PaymentStatuses[i])
		}
		if _, ok := transitions[s]; !ok {
			t.Fatalf("status %s missing from the transition table", s)
		}
	}
	// The original nine keep their order (retrocompatibility of the enum).
	orig := []string{"pending", "requires_action", "authorized", "paid", "failed", "cancelled", "expired", "refunded", "partially_refunded"}
	for i, s := range orig {
		if capability.PaymentStatuses[i] != s {
			t.Fatalf("original status %d moved: %v", i, capability.PaymentStatuses)
		}
	}
}

func TestStatusClasses(t *testing.T) {
	for _, s := range Statuses {
		classes := 0
		for _, in := range []bool{s.Open(), s.Captured(), s == StatusFailed || s == StatusCancelled || s == StatusExpired} {
			if in {
				classes++
			}
		}
		if classes != 1 {
			t.Errorf("%s must be exactly one of open / captured / gave up", s)
		}
	}
	for _, s := range []Status{StatusFailed, StatusCancelled, StatusExpired, StatusRefunded} {
		if !s.Final() {
			t.Errorf("%s must be final", s)
		}
	}
	for _, s := range []Status{StatusPaid, StatusPartiallyRefunded, StatusCancelling, StatusAtDevice} {
		if s.Final() {
			t.Errorf("%s must not be final", s)
		}
	}
	if StatusAuthorized.Counts() || !StatusPartiallyRefunded.Counts() || StatusRefunded.Counts() {
		t.Error("only paid / partially_refunded count toward the paid total")
	}
}

func TestTransitionTable(t *testing.T) {
	allowed := [][2]Status{
		{StatusPending, StatusAtDevice},
		{StatusAtDevice, StatusPaid},
		{StatusAtDevice, StatusCancelling},
		// MP Point: cancel answered 202 (queued) and the buyer paid anyway.
		{StatusCancelling, StatusPaid},
		// The cancellation was refused: the order is still on the device.
		{StatusCancelling, StatusAtDevice},
		{StatusCancelling, StatusCancelled},
		{StatusAuthorized, StatusPaid},
		{StatusPaid, StatusPartiallyRefunded},
		{StatusPartiallyRefunded, StatusPartiallyRefunded},
		{StatusPartiallyRefunded, StatusRefunded},
	}
	for _, p := range allowed {
		if err := Transition(p[0], p[1]); err != nil {
			t.Errorf("%s -> %s: %v", p[0], p[1], err)
		}
	}
	denied := [][2]Status{
		{StatusPaid, StatusCancelled},
		{StatusPaid, StatusPending},
		{StatusCancelled, StatusPaid},
		{StatusExpired, StatusPaid},
		{StatusRefunded, StatusPaid},
		{StatusPending, StatusRefunded}, // reachable, but not one command
		{StatusAtDevice, StatusPending},
		{StatusPaid, StatusPaid},
		{"approved", StatusPaid},
	}
	for _, p := range denied {
		if err := Transition(p[0], p[1]); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("%s -> %s accepted", p[0], p[1])
		}
	}
	if !Reachable(StatusPending, StatusRefunded) || Reachable(StatusRefunded, StatusPaid) || Reachable(StatusFailed, StatusPaid) {
		t.Fatal("reachability")
	}
	if n := Next(StatusPaid); len(n) != 2 {
		t.Fatalf("Next(paid) = %v", n)
	}
}

func TestObserveEveryPair(t *testing.T) {
	for _, stored := range Statuses {
		for _, observed := range Statuses {
			o := Observe(stored, observed)
			switch o.Decision {
			case Apply:
				if !Reachable(stored, observed) {
					t.Errorf("%s -> %s applied but unreachable", stored, observed)
				}
			case Ignore, Anomaly:
				if o.Reason == "" {
					t.Errorf("%s -> %s: %s without reason", stored, observed, o.Decision)
				}
				if stored != observed && Reachable(stored, observed) {
					t.Errorf("%s -> %s reachable but %s", stored, observed, o.Decision)
				}
			default:
				t.Errorf("%s -> %s: decision %q", stored, observed, o.Decision)
			}
			// Money captured is never ignored when we had given up on it.
			if (stored == StatusExpired || stored == StatusCancelled || stored == StatusFailed) && observed.Captured() && o.Decision != Anomaly {
				t.Errorf("%s -> %s must be an anomaly", stored, observed)
			}
		}
	}
}

func TestObserveCases(t *testing.T) {
	cases := []struct {
		stored, observed Status
		want             Decision
		reason           string
	}{
		{StatusPending, StatusPaid, Apply, ""},
		{StatusPending, StatusRefunded, Apply, ""}, // missed the paid webhook
		{StatusPaid, StatusPaid, Ignore, ReasonDuplicate},
		{StatusPartiallyRefunded, StatusPartiallyRefunded, Apply, ""},
		{StatusPaid, StatusPending, Ignore, ReasonStale},        // older webhook delivered late
		{StatusPaid, StatusAtDevice, Ignore, ReasonStale},       // older poll answer
		{StatusCancelled, StatusAtDevice, Ignore, ReasonStale},  // poll that crossed the cancel
		{StatusExpired, StatusPaid, Anomaly, ReasonLateCapture}, // storefront late payment
		{StatusCancelled, StatusAuthorized, Anomaly, ReasonLateCapture},
		{StatusPaid, StatusCancelled, Anomaly, ReasonCaptureReversed},
		{StatusRefunded, StatusPaid, Anomaly, ReasonRefundReversed},
		{StatusPartiallyRefunded, StatusPaid, Anomaly, ReasonRefundReversed},
		{StatusFailed, StatusExpired, Ignore, ReasonStale},
		{StatusCancelling, StatusPaid, Apply, ""},
		{StatusPending, "approved", Anomaly, ReasonUnknownStatus},
		// r4 B4: monotonic. A late at_device poll never undoes an
		// authorization (no path through cancelling), and an older open
		// status never replaces a newer one.
		{StatusAuthorized, StatusAtDevice, Ignore, ReasonStale},
		{StatusAuthorized, StatusPending, Ignore, ReasonStale},
		{StatusRequiresAction, StatusPending, Ignore, ReasonStale},
		{StatusAtDevice, StatusPending, Ignore, ReasonStale},
		{StatusPending, StatusCancelling, Apply, ""},
		// From cancelling (the stored status) a refused cancellation applies.
		{StatusCancelling, StatusAtDevice, Apply, ""},
		{StatusCancelling, StatusAuthorized, Apply, ""},
	}
	for _, c := range cases {
		got := Observe(c.stored, c.observed)
		if got.Decision != c.want || got.Reason != c.reason {
			t.Errorf("Observe(%s, %s) = %+v, want %s %q", c.stored, c.observed, got, c.want, c.reason)
		}
	}
}

// Reachable never goes back to an older open status, whatever the path.
func TestReachableMonotonic(t *testing.T) {
	for _, from := range Statuses {
		for _, to := range Statuses {
			if from == StatusCancelling || !from.Open() || !to.Open() || !Reachable(from, to) {
				continue
			}
			if openRank[to] <= openRank[from] {
				t.Errorf("%s -> %s reachable: goes back", from, to)
			}
		}
	}
}

// r1 punto 1: on a channel with a provider the only local command is
// cancelling; the provider decides the rest. Drawer cash is counted by staff.
func TestCommand(t *testing.T) {
	for _, ch := range []Channel{ChannelTerminal, ChannelOnline, ChannelQR, ChannelLink} {
		if err := Command(ch, StatusAtDevice, StatusCancelling); err != nil {
			t.Errorf("%s: cancelling refused: %v", ch, err)
		}
		for _, to := range []Status{StatusCancelled, StatusPaid, StatusFailed} {
			if err := Command(ch, StatusPending, to); !errors.Is(err, ErrInvalidTransition) {
				t.Errorf("%s: local pending -> %s accepted", ch, to)
			}
		}
	}
	if err := Command(ChannelCash, StatusPending, StatusPaid); err != nil {
		t.Fatalf("drawer cash paid: %v", err)
	}
	if err := Command(ChannelCash, StatusPaid, StatusPending); !errors.Is(err, ErrInvalidTransition) {
		t.Fatal("drawer cash goes back")
	}
}
