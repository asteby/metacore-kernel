package dynamic

import (
	"testing"

	"github.com/asteby/metacore-kernel/cachecoherence"
)

// softDeleteCols / orgScopedTables used to live for the whole process: an
// addon migration that added deleted_at or organization_id to a table was
// ignored until restart. A coherence bump now forgets them (and the live
// column cache generation moves).
func TestCoherenceBumpClearsSchemaIntrospectionCaches(t *testing.T) {
	softDeleteCols.Store("t_coh", false)
	orgScopedTables.Store("t_coh", false)
	gen := columnsGen.Load()

	cachecoherence.Bump("addon.upgrade", cachecoherence.WithAddon("x"))

	if _, ok := softDeleteCols.Load("t_coh"); ok {
		t.Fatal("softDeleteCols survived the bump")
	}
	if _, ok := orgScopedTables.Load("t_coh"); ok {
		t.Fatal("orgScopedTables survived the bump")
	}
	if columnsGen.Load() == gen {
		t.Fatal("live-columns generation did not move")
	}
}
