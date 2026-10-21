package dynamic

import (
	"context"
	"database/sql"
	"testing"

	"gorm.io/gorm"
)

// fakeRaw es el pool crudo: registra lo que le llega.
type fakeRaw struct{ execs []string }

func (f *fakeRaw) PrepareContext(context.Context, string) (*sql.Stmt, error) { return nil, nil }
func (f *fakeRaw) ExecContext(_ context.Context, q string, _ ...interface{}) (sql.Result, error) {
	f.execs = append(f.execs, q)
	return nil, nil
}
func (f *fakeRaw) QueryContext(context.Context, string, ...interface{}) (*sql.Rows, error) {
	return nil, nil
}
func (f *fakeRaw) QueryRowContext(context.Context, string, ...interface{}) *sql.Row { return nil }

// fakeWrap imita un envoltorio (como staleplan.pool / txPool).
type fakeWrap struct {
	gorm.ConnPool
	inner gorm.ConnPool
}

func (w *fakeWrap) UnwrapConnPool() gorm.ConnPool { return w.inner }

func TestRawConnPoolUnwrapsNestedWrappers(t *testing.T) {
	raw := &fakeRaw{}
	cases := map[string]gorm.ConnPool{
		"sin envoltura":        raw,
		"un wrapper":           &fakeWrap{inner: raw},
		"wrappers anidados":    &fakeWrap{inner: &fakeWrap{inner: &fakeWrap{inner: raw}}},
		"stmt cache en medio":  &fakeWrap{inner: &gorm.PreparedStmtDB{ConnPool: raw}},
		"wrapper sobre tx":     &fakeWrap{inner: &gorm.PreparedStmtTX{Tx: &fakeTx{fakeRaw: raw}, PreparedStmtDB: &gorm.PreparedStmtDB{}}},
		"cache y wrappers mix": &gorm.PreparedStmtDB{ConnPool: &fakeWrap{inner: &gorm.PreparedStmtDB{ConnPool: raw}}},
	}
	for name, pool := range cases {
		t.Run(name, func(t *testing.T) {
			got := rawConnPool(pool)
			if _, isTx := got.(*fakeTx); !isTx && got != gorm.ConnPool(raw) {
				t.Fatalf("no llegó al pool crudo: %T", got)
			}
			if _, ok := got.(ConnPoolUnwrapper); ok {
				t.Fatalf("quedó un wrapper: %T", got)
			}
		})
	}
}

type fakeTx struct{ *fakeRaw }

func (f *fakeTx) Commit() error   { return nil }
func (f *fakeTx) Rollback() error { return nil }
func (f *fakeTx) StmtContext(_ context.Context, s *sql.Stmt) *sql.Stmt { return s }

func TestExecScriptRunsOnRawPoolThroughWrappers(t *testing.T) {
	raw := &fakeRaw{}
	db := &gorm.DB{Config: &gorm.Config{}, Statement: &gorm.Statement{
		ConnPool: &fakeWrap{inner: &gorm.PreparedStmtDB{ConnPool: &fakeWrap{inner: raw}}},
		Context:  context.Background(),
	}}
	if err := execScript(db, "A; B;"); err != nil {
		t.Fatal(err)
	}
	if len(raw.execs) != 1 || raw.execs[0] != "A; B;" {
		t.Fatalf("el script no llegó entero al pool crudo: %v", raw.execs)
	}
}
