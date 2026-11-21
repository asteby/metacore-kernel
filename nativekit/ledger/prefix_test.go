package ledger_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/nativekit"
	"github.com/asteby/metacore-kernel/nativekit/ledger"
	"github.com/asteby/metacore-kernel/nativekit/nktest"
)

// A ledger with TablePrefix lives in its own tables and behaves exactly like
// the shared one: post, replay, floor, reverse, reconcile.
func TestTablePrefixModeFullCycle(t *testing.T) {
	nktest.Each(t, func(t *testing.T, db *gorm.DB) {
		l := ledger.New("stock", ledger.Config{TablePrefix: "mod_stock_"})
		if err := l.Migrate(context.Background(), db); err != nil {
			t.Fatal(err)
		}
		if err := l.Migrate(context.Background(), db); err != nil {
			t.Fatalf("migrate must be idempotent: %v", err)
		}
		org := uuid.New()
		first, err := post(t, l, db, ledger.NewPosting{Org: org, Key: "k1", Lines: []ledger.Line{line("a", "5"), line("b", "2")}})
		if err != nil {
			t.Fatal(err)
		}
		if again, _ := post(t, l, db, ledger.NewPosting{Org: org, Key: "k1", Lines: []ledger.Line{line("a", "5"), line("b", "2")}}); !again.Replayed {
			t.Fatal("replay must work in prefixed tables")
		}
		if _, err := post(t, l, db, ledger.NewPosting{Org: org, Lines: []ledger.Line{line("a", "-9")}}); !nativekit.HasCode(err, ledger.CodeInsufficient) {
			t.Fatalf("floor: %v", err)
		}
		err = l.Run(context.Background(), db, func(s *ledger.Session) error {
			_, e := s.Reverse(ledger.ReverseInput{Org: org, PostingID: first.Posting.ID})
			return e
		})
		if err != nil {
			t.Fatal(err)
		}
		if b, _ := l.Balance(context.Background(), db, org, "a", ""); b != 0 {
			t.Fatalf("balance %s", b)
		}
		if dr, err := l.Reconcile(context.Background(), db, org); err != nil || len(dr) != 0 {
			t.Fatalf("drift %v %v", dr, err)
		}
		var n int64
		db.Table("mod_stock_postings").Count(&n)
		if n != 2 {
			t.Fatalf("postings in module tables = %d", n)
		}
		if db.Migrator().HasTable("metacore_ledger_postings") {
			t.Fatal("prefixed mode must not touch the shared tables")
		}
	})
}

func TestInvalidPrefixPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("a prefix with SQL metacharacters must be refused")
		}
	}()
	ledger.New("x", ledger.Config{TablePrefix: "a; drop table x;--"})
}
