package nativekit_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/nativekit"
	"github.com/asteby/metacore-kernel/nativekit/nktest"
)

func TestErrorCodes(t *testing.T) {
	base := nativekit.E("stock.negative", "not enough", map[string]any{"current": "1.00"})
	wrapped := fmt.Errorf("op failed: %w", base)

	if nativekit.CodeOf(wrapped) != "stock.negative" || !nativekit.HasCode(wrapped, "stock.negative") {
		t.Fatalf("code lost through wrapping: %v", nativekit.CodeOf(wrapped))
	}
	if !errors.Is(wrapped, nativekit.E("stock.negative", "")) {
		t.Fatal("errors.Is must match by code")
	}
	if errors.Is(wrapped, nativekit.E("other", "")) {
		t.Fatal("different code must not match")
	}
	if nativekit.MetaOf(wrapped)["current"] != "1.00" {
		t.Fatal("meta lost")
	}
	if nativekit.CodeOf(errors.New("plain")) != "" {
		t.Fatal("plain errors have no code")
	}
	cause := errors.New("boom")
	if !errors.Is(nativekit.Wrap(nativekit.CodeInternal, "x", cause), cause) {
		t.Fatal("Wrap must keep the cause")
	}
}

func TestHTTPStatus(t *testing.T) {
	cases := map[nativekit.Code]int{
		nativekit.CodeInvalid:             http.StatusBadRequest,
		nativekit.CodeNotFound:            http.StatusNotFound,
		nativekit.CodeConflict:            http.StatusConflict,
		nativekit.CodeIdempotencyConflict: http.StatusConflict,
		nativekit.CodeInternal:            http.StatusInternalServerError,
		"stock.negative":                  http.StatusUnprocessableEntity,
	}
	for c, want := range cases {
		if got := c.HTTPStatus(); got != want {
			t.Errorf("%s: %d want %d", c, got, want)
		}
	}
}

func TestSortedIDsDedupesAndOrders(t *testing.T) {
	a := uuid.MustParse("00000000-0000-0000-0000-00000000000a")
	b := uuid.MustParse("00000000-0000-0000-0000-00000000000b")
	c := uuid.MustParse("ffffffff-0000-0000-0000-000000000000")
	got := nativekit.SortedIDs(c, b, a, b, c)
	if len(got) != 3 || got[0] != a || got[1] != b || got[2] != c {
		t.Fatalf("got %v", got)
	}
	k := nativekit.SortedKeys("z", "a", "z", "m")
	if len(k) != 3 || k[0] != "a" || k[2] != "z" {
		t.Fatalf("keys %v", k)
	}
}

func TestKeyAndFingerprint(t *testing.T) {
	if got := nativekit.Key("sale", " 42 ", "", "out"); got != "sale:42:out" {
		t.Fatal(got)
	}
	if nativekit.Key("", " ") != "" {
		t.Fatal("empty key must stay empty")
	}
	a := nativekit.Fingerprint(map[string]any{"q": 1, "r": 2})
	b := nativekit.Fingerprint(map[string]any{"r": 2, "q": 1})
	if a == "" || a != b {
		t.Fatal("fingerprint must be stable")
	}
	if a == nativekit.Fingerprint(map[string]any{"q": 2, "r": 2}) {
		t.Fatal("different payloads must differ")
	}
}

func TestOpHooks(t *testing.T) {
	var order []string
	op := nativekit.NewOp(context.Background(), nil)
	op.AfterCommit(func(context.Context) { order = append(order, "a") })
	op.AfterCommit(func(context.Context) { panic("hook blew up") })
	op.AfterCommit(func(context.Context) { order = append(order, "b") })
	op.OnRollback(func(context.Context) { order = append(order, "rollback") })
	op.Commit()
	op.Commit()   // idempotent
	op.Rollback() // already finalized: no-op
	if len(order) != 2 || order[0] != "a" || order[1] != "b" {
		t.Fatalf("hooks %v (panic must not stop later hooks)", order)
	}

	order = nil
	op = nativekit.NewOp(context.Background(), nil)
	op.AfterCommit(func(context.Context) { order = append(order, "commit") })
	op.OnRollback(func(context.Context) { order = append(order, "rollback") })
	op.Rollback()
	if len(order) != 1 || order[0] != "rollback" {
		t.Fatalf("rollback hooks %v", order)
	}
}

func TestIsRetryable(t *testing.T) {
	if !nativekit.IsRetryable(fmt.Errorf("w: %w", &pgconn.PgError{Code: "40001"})) {
		t.Fatal("serialization failure is retryable")
	}
	if !nativekit.IsRetryable(&pgconn.PgError{Code: "40P01"}) {
		t.Fatal("deadlock is retryable")
	}
	if nativekit.IsRetryable(&pgconn.PgError{Code: "23505"}) || nativekit.IsRetryable(errors.New("x")) {
		t.Fatal("unique violation / plain errors are not retryable")
	}
}

type receipt struct {
	Total string `json:"total"`
	N     int    `json:"n"`
}

type note struct {
	ID   uuid.UUID `gorm:"type:uuid;primaryKey"`
	Body string
}

func setup(t *testing.T, db *gorm.DB) {
	t.Helper()
	if err := nativekit.Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&note{}); err != nil {
		t.Fatal(err)
	}
}

func TestOnceReplaysAndSkipsSideEffects(t *testing.T) {
	nktest.Each(t, func(t *testing.T, db *gorm.DB) {
		setup(t, db)
		org := uuid.New()
		calls := 0
		do := func() error {
			return nativekit.Run(context.Background(), db, func(op *nativekit.Op) error {
				got, replayed, err := nativekit.Once(op.Tx(), org, "sale", "k1", func() (receipt, error) {
					calls++
					if err := op.Tx().Create(&note{ID: uuid.New(), Body: "x"}).Error; err != nil {
						return receipt{}, err
					}
					return receipt{Total: "9.90", N: calls}, nil
				})
				if err != nil {
					return err
				}
				if got.Total != "9.90" || got.N != 1 {
					t.Errorf("result %+v (replay must return the first result)", got)
				}
				_ = replayed
				return nil
			})
		}
		if err := do(); err != nil {
			t.Fatal(err)
		}
		if err := do(); err != nil {
			t.Fatal(err)
		}
		var n int64
		db.Model(&note{}).Count(&n)
		if calls != 1 || n != 1 {
			t.Fatalf("calls=%d notes=%d: the operation must apply exactly once", calls, n)
		}
	})
}

func TestOnceFailureReleasesKey(t *testing.T) {
	nktest.Each(t, func(t *testing.T, db *gorm.DB) {
		setup(t, db)
		org := uuid.New()
		boom := errors.New("boom")
		err := nativekit.Run(context.Background(), db, func(op *nativekit.Op) error {
			_, _, e := nativekit.Once(op.Tx(), org, "s", "k", func() (receipt, error) {
				op.Tx().Create(&note{ID: uuid.New()})
				return receipt{}, boom
			})
			return e
		})
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
		var n int64
		db.Model(&note{}).Count(&n)
		var keys int64
		db.Model(&nativekit.OpKey{}).Count(&keys)
		if n != 0 || keys != 0 {
			t.Fatalf("a failed operation must leave no rows (notes=%d keys=%d)", n, keys)
		}
		// The retry applies normally.
		err = nativekit.Run(context.Background(), db, func(op *nativekit.Op) error {
			_, replayed, e := nativekit.Once(op.Tx(), org, "s", "k", func() (receipt, error) { return receipt{N: 7}, nil })
			if replayed {
				t.Error("retry after failure must not replay")
			}
			return e
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}

func TestOnceRejectsReusedKeyWithOtherPayload(t *testing.T) {
	nktest.Each(t, func(t *testing.T, db *gorm.DB) {
		setup(t, db)
		org := uuid.New()
		run := func(fp string) error {
			return nativekit.Run(context.Background(), db, func(op *nativekit.Op) error {
				_, _, e := nativekit.Once(op.Tx(), org, "s", "k", func() (receipt, error) { return receipt{}, nil }, nativekit.WithFingerprint(fp))
				return e
			})
		}
		if err := run("aaa"); err != nil {
			t.Fatal(err)
		}
		if err := run("aaa"); err != nil {
			t.Fatalf("same payload replays: %v", err)
		}
		if err := run("bbb"); !nativekit.HasCode(err, nativekit.CodeIdempotencyConflict) {
			t.Fatalf("different payload must conflict, got %v", err)
		}
	})
}

func TestOnceEmptyKeyAlwaysRuns(t *testing.T) {
	nktest.Each(t, func(t *testing.T, db *gorm.DB) {
		setup(t, db)
		calls := 0
		for i := 0; i < 2; i++ {
			_ = nativekit.Run(context.Background(), db, func(op *nativekit.Op) error {
				_, _, err := nativekit.Once(op.Tx(), uuid.New(), "s", "", func() (int, error) { calls++; return calls, nil })
				return err
			})
		}
		if calls != 2 {
			t.Fatalf("calls = %d", calls)
		}
	})
}

func TestOnceConcurrentDuplicatesApplyOnce(t *testing.T) {
	db := nktest.Postgres(t)
	setup(t, db)
	org := uuid.New()
	var applied atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := nativekit.Run(context.Background(), db, func(op *nativekit.Op) error {
				_, _, e := nativekit.Once(op.Tx(), org, "pay", "same-key", func() (receipt, error) {
					applied.Add(1)
					return receipt{Total: "1.00"}, nil
				})
				return e
			})
			if err != nil {
				t.Errorf("run: %v", err)
			}
		}()
	}
	wg.Wait()
	if applied.Load() != 1 {
		t.Fatalf("applied %d times; the unique index must admit exactly one", applied.Load())
	}
}

func TestRunFiresHooksAfterCommitOnly(t *testing.T) {
	nktest.Each(t, func(t *testing.T, db *gorm.DB) {
		setup(t, db)
		var committed, rolled int
		_ = nativekit.Run(context.Background(), db, func(op *nativekit.Op) error {
			op.AfterCommit(func(context.Context) { committed++ })
			op.OnRollback(func(context.Context) { rolled++ })
			return nil
		})
		_ = nativekit.Run(context.Background(), db, func(op *nativekit.Op) error {
			op.AfterCommit(func(context.Context) { committed++ })
			op.OnRollback(func(context.Context) { rolled++ })
			return errors.New("fail")
		})
		if committed != 1 || rolled != 1 {
			t.Fatalf("committed=%d rolled=%d", committed, rolled)
		}
	})
}

func TestLockRowsOrderedAndSkipsMissing(t *testing.T) {
	nktest.Each(t, func(t *testing.T, db *gorm.DB) {
		setup(t, db)
		a, b := uuid.New(), uuid.New()
		db.Create(&note{ID: a, Body: "a"})
		db.Create(&note{ID: b, Body: "b"})
		err := nativekit.Run(context.Background(), db, func(op *nativekit.Op) error {
			rows, err := nativekit.LockRows[note](op.Tx(), b, a, uuid.New())
			if err != nil {
				return err
			}
			if len(rows) != 2 || rows[0].ID.String() > rows[1].ID.String() {
				t.Errorf("rows %v", rows)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}
