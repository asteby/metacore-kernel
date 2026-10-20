package staleplan

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want Kind
	}{
		{"nil", nil, KindNone},
		{"0A000 result type", &pgconn.PgError{Code: "0A000", Message: "cached plan must not change result type"}, KindResultTypeChanged},
		{"0A000 wrapped", fmt.Errorf("apply: %w", &pgconn.PgError{Code: "0A000", Message: "cached plan must not change result type"}), KindResultTypeChanged},
		{"0A000 other feature", &pgconn.PgError{Code: "0A000", Message: "cannot use a feature"}, KindNone},
		{"26000 missing", &pgconn.PgError{Code: "26000", Message: `prepared statement "stmtcache_1" does not exist`}, KindStatementMissing},
		{"26000 other", &pgconn.PgError{Code: "26000", Message: `cursor "x" does not exist`}, KindNone},
		{"42P05 exists", &pgconn.PgError{Code: "42P05", Message: `prepared statement "s" already exists`}, KindStatementExists},
		{"serialization", &pgconn.PgError{Code: "40001", Message: "could not serialize"}, KindNone},
		{"flattened text", errors.New("ERROR: cached plan must not change result type (SQLSTATE 0A000)"), KindResultTypeChanged},
		{"flattened missing", errors.New(`ERROR: prepared statement "x" does not exist (SQLSTATE 26000)`), KindStatementMissing},
		{"plain", errors.New("boom"), KindNone},
	}
	for _, c := range cases {
		if got := Classify(c.err); got != c.want {
			t.Errorf("%s: Classify = %q, want %q", c.name, got, c.want)
		}
		if got := IsStalePlan(c.err); got != (c.want != KindNone) {
			t.Errorf("%s: IsStalePlan = %v", c.name, got)
		}
	}
}
