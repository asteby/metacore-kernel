package ledger_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/nativekit"
	"github.com/asteby/metacore-kernel/nativekit/dec"
	"github.com/asteby/metacore-kernel/nativekit/ledger"
	"github.com/asteby/metacore-kernel/nativekit/nktest"
)

func d(s string) dec.D { return dec.MustParse(s) }

func setup(t *testing.T, db *gorm.DB) {
	t.Helper()
	if err := ledger.Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
}

func line(acct, delta string) ledger.Line { return ledger.Line{Account: acct, Delta: d(delta)} }

func post(t *testing.T, l *ledger.Ledger, db *gorm.DB, p ledger.NewPosting) (res ledger.Result, err error) {
	t.Helper()
	err = l.Run(context.Background(), db, func(s *ledger.Session) error {
		res, err = s.Post(p)
		return err
	})
	return
}

func TestPostUpdatesBalanceAndRecordsResultingBalance(t *testing.T) {
	nktest.Each(t, func(t *testing.T, db *gorm.DB) {
		setup(t, db)
		tick := time.Unix(1_700_000_000, 0)
		l, org := ledger.New("wallet", ledger.Config{Now: func() time.Time { tick = tick.Add(time.Second); return tick }}), uuid.New()
		if _, err := post(t, l, db, ledger.NewPosting{Org: org, Kind: "topup", Lines: []ledger.Line{line("c1", "10")}}); err != nil {
			t.Fatal(err)
		}
		res, err := post(t, l, db, ledger.NewPosting{Org: org, Kind: "spend", Lines: []ledger.Line{line("c1", "-3.5")}})
		if err != nil {
			t.Fatal(err)
		}
		if got := res.Entries[0].BalanceAfter; got != d("6.5") {
			t.Fatalf("balance after = %s", got)
		}
		if b, _ := l.Balance(context.Background(), db, org, "c1", ""); b != d("6.5") {
			t.Fatalf("balance = %s", b)
		}
		h, _ := l.History(context.Background(), db, org, ledger.HistoryFilter{Account: "c1"})
		if len(h) != 2 || h[0].BalanceAfter != d("6.5") || h[1].BalanceAfter != d("10") {
			t.Fatalf("history %+v", h)
		}
	})
}

func TestFloorBlocksOverdraftAndWritesNothing(t *testing.T) {
	nktest.Each(t, func(t *testing.T, db *gorm.DB) {
		setup(t, db)
		l, org := ledger.New("stock", ledger.Config{}), uuid.New()
		post(t, l, db, ledger.NewPosting{Org: org, Lines: []ledger.Line{line("a", "5")}})
		_, err := post(t, l, db, ledger.NewPosting{Org: org, Lines: []ledger.Line{line("b", "2"), line("a", "-6")}})
		if !nativekit.HasCode(err, ledger.CodeInsufficient) {
			t.Fatalf("want insufficient, got %v", err)
		}
		if meta := nativekit.MetaOf(err); meta["account"] != "a" || meta["balance"] != "5.00" {
			t.Fatalf("meta %v", meta)
		}
		if b, _ := l.Balance(context.Background(), db, org, "b", ""); b != 0 {
			t.Fatalf("a failed posting must not move other lines: b=%s", b)
		}
		// A credit limit is a negative floor.
		floor := d("-100")
		if _, err := post(t, l, db, ledger.NewPosting{Org: org, Lines: []ledger.Line{{Account: "cust", Delta: d("-80"), Floor: &floor}}}); err != nil {
			t.Fatal(err)
		}
		if _, err := post(t, l, db, ledger.NewPosting{Org: org, Lines: []ledger.Line{{Account: "cust", Delta: d("-30"), Floor: &floor}}}); !nativekit.HasCode(err, ledger.CodeInsufficient) {
			t.Fatalf("credit limit not enforced: %v", err)
		}
	})
}

func TestAllowNegativeLedger(t *testing.T) {
	nktest.Each(t, func(t *testing.T, db *gorm.DB) {
		setup(t, db)
		l, org := ledger.New("overdraft", ledger.Config{AllowNegative: true}), uuid.New()
		if _, err := post(t, l, db, ledger.NewPosting{Org: org, Lines: []ledger.Line{line("a", "-4")}}); err != nil {
			t.Fatal(err)
		}
	})
}

func TestIdempotentReplay(t *testing.T) {
	nktest.Each(t, func(t *testing.T, db *gorm.DB) {
		setup(t, db)
		l, org := ledger.New("stock", ledger.Config{}), uuid.New()
		p := ledger.NewPosting{Org: org, Key: "sale:1:line-1", Lines: []ledger.Line{line("a", "5")}}
		first, err := post(t, l, db, p)
		if err != nil || first.Replayed {
			t.Fatal(err, first.Replayed)
		}
		again, err := post(t, l, db, p)
		if err != nil || !again.Replayed || again.Posting.ID != first.Posting.ID {
			t.Fatalf("replay = %+v err=%v", again.Replayed, err)
		}
		if b, _ := l.Balance(context.Background(), db, org, "a", ""); b != d("5") {
			t.Fatalf("replay double counted: %s", b)
		}
		p.Fingerprint = "x"
		p.Key = "other"
		post(t, l, db, p)
		p.Fingerprint = "y"
		if _, err := post(t, l, db, p); !nativekit.HasCode(err, nativekit.CodeIdempotencyConflict) {
			t.Fatalf("key reuse with other payload: %v", err)
		}
	})
}

func TestReverseAndDouble(t *testing.T) {
	nktest.Each(t, func(t *testing.T, db *gorm.DB) {
		setup(t, db)
		l, org := ledger.New("stock", ledger.Config{}), uuid.New()
		sale, _ := post(t, l, db, ledger.NewPosting{Org: org, Kind: "income", Lines: []ledger.Line{line("a", "8"), {Account: "a", Bucket: "reserved", Delta: d("2")}}})
		rev := func() (r ledger.Result, err error) {
			err = l.Run(context.Background(), db, func(s *ledger.Session) error {
				r, err = s.Reverse(ledger.ReverseInput{Org: org, PostingID: sale.Posting.ID})
				return err
			})
			return
		}
		r, err := rev()
		if err != nil {
			t.Fatal(err)
		}
		if r.Posting.ReversesID == nil || *r.Posting.ReversesID != sale.Posting.ID {
			t.Fatal("reversal must link to the original")
		}
		bs, _ := l.Balances(context.Background(), db, org, "a")
		if bs["balance"] != 0 || bs["reserved"] != 0 {
			t.Fatalf("balances after reversal %v", bs)
		}
		again, err := rev()
		if err != nil || !again.Replayed {
			t.Fatalf("same reversal request must replay: %+v %v", again.Replayed, err)
		}
		err = l.Run(context.Background(), db, func(s *ledger.Session) error {
			_, e := s.Reverse(ledger.ReverseInput{Org: org, PostingID: sale.Posting.ID, Key: "different"})
			return e
		})
		if !nativekit.HasCode(err, ledger.CodeAlreadyReversed) {
			t.Fatalf("second reversal: %v", err)
		}
		err = l.Run(context.Background(), db, func(s *ledger.Session) error {
			_, e := s.Reverse(ledger.ReverseInput{Org: org, PostingID: r.Posting.ID})
			return e
		})
		if !nativekit.HasCode(err, ledger.CodeCannotReverse) {
			t.Fatalf("reversing a reversal: %v", err)
		}
	})
}

func TestReverseRespectsFloorUnlessAllowed(t *testing.T) {
	nktest.Each(t, func(t *testing.T, db *gorm.DB) {
		setup(t, db)
		l, org := ledger.New("stock", ledger.Config{}), uuid.New()
		in, _ := post(t, l, db, ledger.NewPosting{Org: org, Lines: []ledger.Line{line("a", "5")}})
		post(t, l, db, ledger.NewPosting{Org: org, Lines: []ledger.Line{line("a", "-5")}}) // consumed
		err := l.Run(context.Background(), db, func(s *ledger.Session) error {
			_, e := s.Reverse(ledger.ReverseInput{Org: org, PostingID: in.Posting.ID})
			return e
		})
		if !nativekit.HasCode(err, ledger.CodeInsufficient) {
			t.Fatalf("reversal below floor: %v", err)
		}
		err = l.Run(context.Background(), db, func(s *ledger.Session) error {
			_, e := s.Reverse(ledger.ReverseInput{Org: org, PostingID: in.Posting.ID, AllowNegative: true})
			return e
		})
		if err != nil {
			t.Fatal(err)
		}
		if b, _ := l.Balance(context.Background(), db, org, "a", ""); b != d("-5") {
			t.Fatalf("balance %s", b)
		}
	})
}

func TestReconcileDetectsDrift(t *testing.T) {
	nktest.Each(t, func(t *testing.T, db *gorm.DB) {
		setup(t, db)
		l, org := ledger.New("stock", ledger.Config{}), uuid.New()
		post(t, l, db, ledger.NewPosting{Org: org, Lines: []ledger.Line{line("a", "5"), line("b", "1.25")}})
		if dr, err := l.Reconcile(context.Background(), db, org); err != nil || len(dr) != 0 {
			t.Fatalf("clean books drifted: %v %v", dr, err)
		}
		db.Model(&ledger.Cell{}).Where("account = ?", "a").Update("balance", d("9"))
		dr, err := l.Reconcile(context.Background(), db, org)
		if err != nil || len(dr) != 1 || dr[0].Account != "a" || dr[0].Stored != d("9") || dr[0].FromEntries != d("5") {
			t.Fatalf("drift = %+v err=%v", dr, err)
		}
	})
}

func TestEventsAfterCommitOnly(t *testing.T) {
	nktest.Each(t, func(t *testing.T, db *gorm.DB) {
		setup(t, db)
		var events []ledger.Event
		l := ledger.New("stock", ledger.Config{Emit: func(_ context.Context, ev ledger.Event) { events = append(events, ev) }})
		org := uuid.New()
		boom := errors.New("later step failed")
		err := l.Run(context.Background(), db, func(s *ledger.Session) error {
			if _, err := s.Post(ledger.NewPosting{Org: org, Lines: []ledger.Line{line("a", "1")}}); err != nil {
				return err
			}
			return boom
		})
		if !errors.Is(err, boom) || len(events) != 0 {
			t.Fatalf("rolled back operation must not emit: err=%v events=%d", err, len(events))
		}
		post(t, l, db, ledger.NewPosting{Org: org, Kind: "k", Lines: []ledger.Line{line("a", "1"), line("b", "2")}})
		if len(events) != 1 || len(events[0].Cells) != 2 || events[0].Kind != "k" {
			t.Fatalf("events %+v", events)
		}
	})
}

func TestOnPostErrorAbortsPosting(t *testing.T) {
	nktest.Each(t, func(t *testing.T, db *gorm.DB) {
		setup(t, db)
		l := ledger.New("stock", ledger.Config{OnPost: func(context.Context, *gorm.DB, ledger.Event) error { return errors.New("outbox down") }})
		org := uuid.New()
		if _, err := post(t, l, db, ledger.NewPosting{Org: org, Lines: []ledger.Line{line("a", "1")}}); err == nil {
			t.Fatal("hook error must abort")
		}
		if b, _ := l.Balance(context.Background(), db, org, "a", ""); b != 0 {
			t.Fatalf("balance leaked: %s", b)
		}
	})
}

func TestValidation(t *testing.T) {
	l := ledger.New("v", ledger.Config{})
	op := nativekit.NewOp(context.Background(), nil)
	s := l.Session(op)
	for name, p := range map[string]ledger.NewPosting{
		"no org":     {Lines: []ledger.Line{line("a", "1")}},
		"no lines":   {Org: uuid.New()},
		"zero":       {Org: uuid.New(), Lines: []ledger.Line{line("a", "0")}},
		"no account": {Org: uuid.New(), Lines: []ledger.Line{{Delta: d("1")}}},
	} {
		if _, err := s.Post(p); !nativekit.HasCode(err, ledger.CodeInvalidPosting) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Opposite-order postings from many goroutines must neither deadlock nor lose
// an update: the canonical lock order is what this guards.
func TestConcurrentOppositeOrderNoDeadlock(t *testing.T) {
	db := nktest.Postgres(t)
	setup(t, db)
	l, org := ledger.New("stock", ledger.Config{}), uuid.New()
	post(t, l, db, ledger.NewPosting{Org: org, Lines: []ledger.Line{line("a", "100"), line("b", "100")}})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			from, to := "a", "b"
			if i%2 == 1 {
				from, to = "b", "a"
			}
			_, err := post(t, l, db, ledger.NewPosting{Org: org, Lines: []ledger.Line{line(from, "-1"), line(to, "1")}})
			if err != nil {
				t.Errorf("transfer %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	a, _ := l.Balance(context.Background(), db, org, "a", "")
	b, _ := l.Balance(context.Background(), db, org, "b", "")
	if a.Add(b) != d("200") {
		t.Fatalf("money created or lost: %s + %s", a, b)
	}
	if dr, _ := l.Reconcile(context.Background(), db, org); len(dr) != 0 {
		t.Fatalf("drift %v", dr)
	}
}

func TestConcurrentSameKeyAppliesOnce(t *testing.T) {
	db := nktest.Postgres(t)
	setup(t, db)
	l, org := ledger.New("stock", ledger.Config{}), uuid.New()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := post(t, l, db, ledger.NewPosting{Org: org, Key: "same", Lines: []ledger.Line{line("a", "1")}}); err != nil {
				t.Errorf("post: %v", err)
			}
		}()
	}
	wg.Wait()
	if b, _ := l.Balance(context.Background(), db, org, "a", ""); b != d("1") {
		t.Fatalf("applied %s times", b)
	}
}
