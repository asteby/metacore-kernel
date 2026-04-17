package migrations

import "embed"

// SQLFiles holds every *.sql file under migrations/sqlfiles/ compiled into the
// binary. The embed directive is intentionally placed in its own file so that
// callers importing the package do not need to be aware of the embed path.
//
//go:embed sqlfiles/*.sql
var SQLFiles embed.FS
