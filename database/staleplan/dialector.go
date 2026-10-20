package staleplan

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

const connGenKey = "metacore.staleplan.generation"

// resetSession is the database/sql per-connection hook: a connection that
// predates the last [Reset] still holds the server-side prepared statements
// (with their old result descriptions) the reset wanted gone, so it DEALLOCATEs
// them before its next use. A connection first seen now is stamped, not wiped.
func resetSession(ctx context.Context, c *pgx.Conn) error {
	data := c.PgConn().CustomData()
	cur := generation.Load()
	if prev, ok := data[connGenKey].(uint64); ok && prev != cur {
		if err := c.DeallocateAll(ctx); err != nil {
			slog.Warn("staleplan.deallocate_all_failed", "err", err.Error())
			return fmt.Errorf("staleplan: deallocate all: %w", err) // database/sql discards it
		}
		counters.conns.Add(1)
	}
	data[connGenKey] = cur
	return nil
}

// OpenSQLDB opens a database/sql pool over pgx with the per-connection
// generation check. dsn is a libpq/pgx DSN or URL.
func OpenSQLDB(dsn string) (*sql.DB, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	return stdlib.OpenDB(*cfg, stdlib.OptionResetSession(resetSession)), nil
}

// Dialector is gorm's postgres.Open(dsn) plus the per-connection generation
// check: use it instead of postgres.Open so that, after an addon changes the
// schema, every pooled connection drops its server-side prepared statements
// the next time it is reused, not just the ones GORM happens to hold handles for.
func Dialector(dsn string) (gorm.Dialector, error) {
	db, err := OpenSQLDB(dsn)
	if err != nil {
		return nil, err
	}
	return postgres.New(postgres.Config{Conn: db}), nil
}

// scrubIdle runs DEALLOCATE ALL on the connections that are idle right now.
// It is the host-agnostic half of the reset: it works for a pool opened with
// plain postgres.Open. Busy connections are not touched (we cannot take them);
// with [Dialector] they are scrubbed when next reused, which closes that gap.
func scrubIdle(ctx context.Context, db *sql.DB) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
	defer cancel()
	n := db.Stats().Idle
	held := make([]*sql.Conn, 0, n)
	for i := 0; i < n; i++ {
		c, err := db.Conn(ctx)
		if err != nil {
			break
		}
		held = append(held, c)
		_ = c.Raw(func(dc any) error {
			if sc, ok := dc.(*stdlib.Conn); ok {
				if err := sc.Conn().DeallocateAll(ctx); err == nil {
					counters.conns.Add(1)
					sc.Conn().PgConn().CustomData()[connGenKey] = generation.Load()
				}
			}
			return nil
		})
	}
	for _, c := range held {
		_ = c.Close()
	}
}
