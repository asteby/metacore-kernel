// Package migrations provides a versioned SQL migration runner for
// metacore-kernel. It wraps pressly/goose with an embedded-FS source so
// migration files are compiled into the binary and do not require a mounted
// filesystem at runtime.
package migrations

// Migration is the metadata descriptor for a single versioned migration.
// Goose itself tracks execution state in the schema_migrations table; this
// struct is used only for listing / introspection by callers.
type Migration struct {
	// Version is the integer prefix of the file name (e.g. 1 for 0001_*).
	Version int64
	// Name is the human-readable label extracted from the file name
	// (e.g. "init_users" for 0001_init_users.up.sql).
	Name string
	// Applied reports whether the migration has already been applied to the
	// current database.
	Applied bool
}
