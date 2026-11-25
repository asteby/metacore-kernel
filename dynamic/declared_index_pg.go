package dynamic

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// pgDeclaredIndexDB is the Postgres declaredIndexDB. In a real run db is ONE
// dedicated connection (gorm Connection), so the session settings and the
// advisory locks stay on it; in a dry-run it is the pool (read-only queries).
type pgDeclaredIndexDB struct{ db *gorm.DB }

func (p pgDeclaredIndexDB) relKind(ctx context.Context, schema, rel string) (string, error) {
	var rows []struct{ Kind string }
	err := p.db.WithContext(ctx).Raw(`SELECT c.relkind::text AS kind FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = ? AND c.relname = ?`, schema, rel).Scan(&rows).Error
	if err != nil || len(rows) == 0 {
		return "", err
	}
	return rows[0].Kind, nil
}

func (p pgDeclaredIndexDB) columns(ctx context.Context, schema, table string) (map[string]bool, error) {
	var names []string
	err := p.db.WithContext(ctx).Raw(`SELECT a.attname FROM pg_attribute a
JOIN pg_class c ON c.oid = a.attrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = ? AND c.relname = ? AND a.attnum > 0 AND NOT a.attisdropped`, schema, table).Scan(&names).Error
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out, nil
}

func (p pgDeclaredIndexDB) index(ctx context.Context, schema, name string) (declaredIndexInfo, error) {
	var rows []struct {
		Kind  string
		Valid *bool
		Tbl   *string
	}
	err := p.db.WithContext(ctx).Raw(`SELECT c.relkind::text AS kind, i.indisvalid AS valid, t.relname AS tbl
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_index i ON i.indexrelid = c.oid
LEFT JOIN pg_class t ON t.oid = i.indrelid
WHERE n.nspname = ? AND c.relname = ?`, schema, name).Scan(&rows).Error
	if err != nil || len(rows) == 0 {
		return declaredIndexInfo{}, err
	}
	info := declaredIndexInfo{exists: true, kind: rows[0].Kind}
	if rows[0].Valid != nil {
		info.valid = *rows[0].Valid
	}
	if rows[0].Tbl != nil {
		info.table = *rows[0].Tbl
	}
	return info, nil
}

func (p pgDeclaredIndexDB) tryLock(ctx context.Context, key string) (bool, error) {
	var got bool
	err := p.db.WithContext(ctx).Raw(`SELECT pg_try_advisory_lock(hashtextextended(?, 0))`, key).Scan(&got).Error
	return got, err
}

// unlock releases the session lock even when the run's ctx is already done
// (the connection goes back to the pool; a dead connection drops it anyway).
func (p pgDeclaredIndexDB) unlock(key string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var ok bool
	_ = p.db.WithContext(ctx).Raw(`SELECT pg_advisory_unlock(hashtextextended(?, 0))`, key).Scan(&ok).Error
}

// exec runs ONE statement (a multi-statement string would be an implicit
// transaction block, where CONCURRENTLY is refused).
func (p pgDeclaredIndexDB) exec(ctx context.Context, stmt string) error {
	return p.db.WithContext(ctx).Exec(stmt).Error
}

func (p pgDeclaredIndexDB) setTimeouts(ctx context.Context, lock, stmt time.Duration) error {
	if err := p.exec(ctx, fmt.Sprintf(`SET lock_timeout = '%dms'`, lock.Milliseconds())); err != nil {
		return err
	}
	return p.exec(ctx, fmt.Sprintf(`SET statement_timeout = '%dms'`, stmt.Milliseconds()))
}

// resetTimeouts restores the session defaults before the connection returns
// to the pool.
func (p pgDeclaredIndexDB) resetTimeouts() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = p.exec(ctx, `RESET lock_timeout`)
	_ = p.exec(ctx, `RESET statement_timeout`)
}
