package query

import (
	"database/sql/driver"
	"fmt"
	"strings"

	"gorm.io/gorm"
)

// pgArray is one bound Postgres array. GORM must not expand it into N
// placeholders: the prepared statement is `col = ANY($1::uuid[])` regardless
// of how many ids the list carries.
type pgArray []string

func (a pgArray) Value() (driver.Value, error) {
	var b strings.Builder
	b.Grow(len(a) * 8)
	b.WriteByte('{')
	for i, s := range a {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('"')
		for _, r := range s {
			if r == '"' || r == '\\' {
				b.WriteByte('\\')
			}
			b.WriteRune(r)
		}
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String(), nil
}

// bindInList returns the WHERE fragment and its single bind value.
// SQLite keeps `IN ?` (the live tests run there). Postgres binds one array
// so PrepareStmt can reuse the plan when only the ids change.
func bindInList(db *gorm.DB, col string, vals []string, not, uuidCol bool) (string, any) {
	if db == nil || db.Dialector == nil || db.Dialector.Name() != "postgres" {
		op := "IN"
		if not {
			op = "NOT IN"
		}
		return fmt.Sprintf("%s %s ?", col, op), vals
	}
	cast := "text"
	if uuidCol {
		cast = "uuid"
	}
	op, fn := "=", "ANY"
	if not {
		op, fn = "<>", "ALL"
	}
	return fmt.Sprintf("%s %s %s(?::%s[])", col, op, fn, cast), pgArray(vals)
}
