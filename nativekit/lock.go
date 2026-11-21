package nativekit

import (
	"sort"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// SortedIDs returns ids deduplicated and in the canonical lock order
// (ascending by textual form, which equals Postgres uuid ordering). Acquire
// locks for several rows in this order and two operations can never wait on
// each other in a cycle.
func SortedIDs(ids ...uuid.UUID) []uuid.UUID {
	seen := make(map[uuid.UUID]struct{}, len(ids))
	out := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// SortedKeys is SortedIDs for opaque string keys.
func SortedKeys(keys ...string) []string {
	seen := make(map[string]struct{}, len(keys))
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// LockRows selects the rows of T with the given primary keys FOR UPDATE, in
// canonical order, inside tx. The result is ordered by id; rows that do not
// exist are simply absent (compare lengths to detect them). On dialects
// without row locks (SQLite) it degrades to a plain ordered read, which is
// safe there because writers are serialized.
func LockRows[T any](tx *gorm.DB, ids ...uuid.UUID) ([]T, error) {
	var rows []T
	if len(ids) == 0 {
		return rows, nil
	}
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id IN ?", SortedIDs(ids...)).
		Order("id").
		Find(&rows).Error
	return rows, err
}
