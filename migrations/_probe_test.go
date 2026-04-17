package migrations

import (
	"database/sql"

	"github.com/pressly/goose/v3"
)

func probe(db *sql.DB) {
	p, _ := goose.NewProvider(goose.DialectSQLite3, db, SQLFiles)
	_ = p
}
