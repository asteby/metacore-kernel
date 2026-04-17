package migrations

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"

	"github.com/pressly/goose/v3"
	"gorm.io/gorm"
)

func init() {
	goose.SetLogger(goose.NopLogger())
}

func sqlSubFS() fs.FS {
	sub, err := fs.Sub(SQLFiles, "sqlfiles")
	if err != nil {
		panic("migrations: build sub-fs: " + err.Error())
	}
	return sub
}

// Runner executes versioned SQL migrations using the SQL files embedded in
// SQLFiles. All migration state is tracked by goose in the goose_db_version
// table and is idempotent.
type Runner struct {
	// TableName overrides the default goose version table (goose_db_version).
	// Leave empty to use the default.
	TableName string

	// Dialect selects the SQL dialect passed to goose. Supported values are
	// the goose.Dialect constants: goose.DialectSQLite3, goose.DialectPostgres,
	// goose.DialectMySQL, etc. Defaults to goose.DialectSQLite3 when zero so
	// that existing tests and SQLite-based apps require no changes.
	Dialect goose.Dialect
}

func (r Runner) sqlDB(db *gorm.DB) (*sql.DB, error) {
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("migrations: get sql.DB: %w", err)
	}
	return sqlDB, nil
}

func (r Runner) provider(db *gorm.DB) (*goose.Provider, error) {
	sqlDB, err := r.sqlDB(db)
	if err != nil {
		return nil, err
	}
	dialect := r.Dialect
	if dialect == "" {
		dialect = goose.DialectSQLite3
	}
	opts := []goose.ProviderOption{}
	if r.TableName != "" {
		opts = append(opts, goose.WithTableName(r.TableName))
	}
	return goose.NewProvider(dialect, sqlDB, sqlSubFS(), opts...)
}

// Up runs all pending migrations in ascending version order.
func (r Runner) Up(ctx context.Context, db *gorm.DB) error {
	p, err := r.provider(db)
	if err != nil {
		return err
	}
	results, err := p.Up(ctx)
	if err != nil {
		return fmt.Errorf("migrations: up: %w", err)
	}
	for _, res := range results {
		if res.Error != nil {
			return fmt.Errorf("migrations: version %d: %w", res.Source.Version, res.Error)
		}
	}
	return nil
}

// UpTo runs migrations up to and including the given target version.
func (r Runner) UpTo(ctx context.Context, db *gorm.DB, version int64) error {
	p, err := r.provider(db)
	if err != nil {
		return err
	}
	results, err := p.UpTo(ctx, version)
	if err != nil {
		return fmt.Errorf("migrations: up-to %d: %w", version, err)
	}
	for _, res := range results {
		if res.Error != nil {
			return fmt.Errorf("migrations: version %d: %w", res.Source.Version, res.Error)
		}
	}
	return nil
}

// Down rolls back exactly `steps` migrations. Pass steps=0 to roll back all.
func (r Runner) Down(ctx context.Context, db *gorm.DB, steps int) error {
	p, err := r.provider(db)
	if err != nil {
		return err
	}
	if steps == 0 {
		results, err := p.DownTo(ctx, 0)
		if err != nil {
			return fmt.Errorf("migrations: down-all: %w", err)
		}
		for _, res := range results {
			if res.Error != nil {
				return fmt.Errorf("migrations: version %d: %w", res.Source.Version, res.Error)
			}
		}
		return nil
	}
	for i := 0; i < steps; i++ {
		res, err := p.Down(ctx)
		if err != nil {
			return fmt.Errorf("migrations: down step %d: %w", i+1, err)
		}
		if res != nil && res.Error != nil {
			return fmt.Errorf("migrations: version %d: %w", res.Source.Version, res.Error)
		}
	}
	return nil
}

// Status returns the list of known migrations and whether each has been applied.
func (r Runner) Status(ctx context.Context, db *gorm.DB) ([]Migration, error) {
	p, err := r.provider(db)
	if err != nil {
		return nil, err
	}
	dbVersion, err := p.GetDBVersion(ctx)
	if err != nil {
		return nil, fmt.Errorf("migrations: get db version: %w", err)
	}
	sources := p.ListSources()
	out := make([]Migration, 0, len(sources))
	for _, s := range sources {
		out = append(out, Migration{
			Version: s.Version,
			Name:    s.Path,
			Applied: s.Version <= dbVersion,
		})
	}
	return out, nil
}
