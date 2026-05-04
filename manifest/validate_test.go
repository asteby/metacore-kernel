package manifest_test

import (
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
)

func TestValidate_OK(t *testing.T) {
	m := manifest.Manifest{
		Key:     "tickets",
		Name:    "Tickets",
		Version: "1.0.0",
		Kernel:  ">=2.0.0 <3.0.0",
		ModelDefinitions: []manifest.ModelDefinition{{
			TableName: "tickets",
			ModelKey:  "tickets",
			Columns:   []manifest.ColumnDef{{Name: "title", Type: "string"}},
		}},
		Capabilities: []manifest.Capability{
			{Kind: "db:read", Target: "users"},
		},
	}
	if err := m.Validate("2.0.0"); err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
}

func TestValidate_KernelRange(t *testing.T) {
	m := manifest.Manifest{
		Key:     "aa",
		Name:    "A",
		Version: "1.0.0",
		Kernel:  ">=3.0.0",
	}
	err := m.Validate("2.0.0")
	if err == nil || !strings.Contains(err.Error(), "does not satisfy") {
		t.Fatalf("expected kernel mismatch, got %v", err)
	}
}

func TestValidate_BadKey(t *testing.T) {
	m := manifest.Manifest{Key: "Bad-Key!", Name: "x", Version: "1.0.0"}
	if err := m.Validate("2.0.0"); err == nil {
		t.Fatal("expected invalid key")
	}
}

func TestValidate_BackendWasmRequiresEntry(t *testing.T) {
	m := manifest.Manifest{
		Key: "aa", Name: "A", Version: "1.0.0",
		Backend: &manifest.BackendSpec{Runtime: "wasm"},
	}
	if err := m.Validate("2.0.0"); err == nil || !strings.Contains(err.Error(), "entry") {
		t.Fatalf("expected entry-required error, got %v", err)
	}
}

func TestValidate_BackendWasmHookNotExported(t *testing.T) {
	m := manifest.Manifest{
		Key: "aa", Name: "A", Version: "1.0.0",
		Hooks: map[string]string{"fiscal_documents::stamp_fiscal": "foo"},
		Backend: &manifest.BackendSpec{
			Runtime: "wasm",
			Entry:   "backend/b.wasm",
			Exports: []string{"cancel_fiscal"},
		},
	}
	if err := m.Validate("2.0.0"); err == nil || !strings.Contains(err.Error(), "stamp_fiscal") {
		t.Fatalf("expected export-mismatch error, got %v", err)
	}
}

func TestValidate_BackendUnknownRuntime(t *testing.T) {
	m := manifest.Manifest{
		Key: "aa", Name: "A", Version: "1.0.0",
		Backend: &manifest.BackendSpec{Runtime: "magic"},
	}
	if err := m.Validate("2.0.0"); err == nil {
		t.Fatal("expected unknown runtime error")
	}
}

func TestValidate_CapabilityKind(t *testing.T) {
	m := manifest.Manifest{
		Key:          "aa",
		Name:         "A",
		Version:      "1.0.0",
		Capabilities: []manifest.Capability{{Kind: "weird", Target: "x"}},
	}
	if err := m.Validate("2.0.0"); err == nil {
		t.Fatal("expected capability kind error")
	}
}

// withRelations returns a minimal valid manifest carrying the supplied
// relations on its only model — keeps the relation tests focused on the
// field under test instead of the surrounding boilerplate.
func withRelations(rels ...manifest.RelationDef) manifest.Manifest {
	return manifest.Manifest{
		Key:     "rel",
		Name:    "Rel",
		Version: "1.0.0",
		ModelDefinitions: []manifest.ModelDefinition{{
			TableName: "items",
			ModelKey:  "items",
			Columns:   []manifest.ColumnDef{{Name: "title", Type: "string"}},
			Relations: rels,
		}},
	}
}

func TestValidate_Relations_ZeroValueIsBackwardsCompat(t *testing.T) {
	// A model without Relations populated must validate exactly like
	// before — manifests authored before the field landed keep working.
	m := withRelations()
	if err := m.Validate("2.0.0"); err != nil {
		t.Fatalf("nil relations should still validate, got %v", err)
	}
}

func TestValidate_Relations_OneToManyOK(t *testing.T) {
	m := withRelations(manifest.RelationDef{
		Name:       "tickets",
		Kind:       "one_to_many",
		Through:    "tickets",
		ForeignKey: "owner_id",
	})
	if err := m.Validate("2.0.0"); err != nil {
		t.Fatalf("valid one_to_many should pass, got %v", err)
	}
}

func TestValidate_Relations_OneToManyWithExplicitReferences(t *testing.T) {
	m := withRelations(manifest.RelationDef{
		Name:       "tickets",
		Kind:       "one_to_many",
		Through:    "tickets",
		ForeignKey: "owner_uuid",
		References: "uuid",
	})
	if err := m.Validate("2.0.0"); err != nil {
		t.Fatalf("explicit references should pass, got %v", err)
	}
}

func TestValidate_Relations_ManyToManyOK(t *testing.T) {
	m := withRelations(manifest.RelationDef{
		Name:       "tags",
		Kind:       "many_to_many",
		Through:    "tags",
		ForeignKey: "item_id",
		Pivot:      "items_tags",
	})
	if err := m.Validate("2.0.0"); err != nil {
		t.Fatalf("valid many_to_many should pass, got %v", err)
	}
}

func TestValidate_Relations_UnknownKind(t *testing.T) {
	m := withRelations(manifest.RelationDef{
		Name:       "tickets",
		Kind:       "one_to_one",
		Through:    "tickets",
		ForeignKey: "owner_id",
	})
	err := m.Validate("2.0.0")
	if err == nil || !strings.Contains(err.Error(), "kind") {
		t.Fatalf("expected unknown-kind error, got %v", err)
	}
}

func TestValidate_Relations_BadName(t *testing.T) {
	m := withRelations(manifest.RelationDef{
		Name:       "Tickets!",
		Kind:       "one_to_many",
		Through:    "tickets",
		ForeignKey: "owner_id",
	})
	err := m.Validate("2.0.0")
	if err == nil || !strings.Contains(err.Error(), "name") {
		t.Fatalf("expected name error, got %v", err)
	}
}

func TestValidate_Relations_BadThrough(t *testing.T) {
	m := withRelations(manifest.RelationDef{
		Name:       "tickets",
		Kind:       "one_to_many",
		Through:    "Tickets-Bad",
		ForeignKey: "owner_id",
	})
	err := m.Validate("2.0.0")
	if err == nil || !strings.Contains(err.Error(), "through") {
		t.Fatalf("expected through error, got %v", err)
	}
}

func TestValidate_Relations_BadForeignKey(t *testing.T) {
	m := withRelations(manifest.RelationDef{
		Name:       "tickets",
		Kind:       "one_to_many",
		Through:    "tickets",
		ForeignKey: "Owner ID",
	})
	err := m.Validate("2.0.0")
	if err == nil || !strings.Contains(err.Error(), "foreign_key") {
		t.Fatalf("expected foreign_key error, got %v", err)
	}
}

func TestValidate_Relations_BadReferences(t *testing.T) {
	m := withRelations(manifest.RelationDef{
		Name:       "tickets",
		Kind:       "one_to_many",
		Through:    "tickets",
		ForeignKey: "owner_id",
		References: "Bad-Ref",
	})
	err := m.Validate("2.0.0")
	if err == nil || !strings.Contains(err.Error(), "references") {
		t.Fatalf("expected references error, got %v", err)
	}
}

func TestValidate_Relations_OneToManyRejectsPivot(t *testing.T) {
	// Pivot only makes sense for many_to_many. Setting it on one_to_many
	// is almost always a mis-edit and the relation shape is ambiguous,
	// so the validator refuses rather than silently ignore the field.
	m := withRelations(manifest.RelationDef{
		Name:       "tickets",
		Kind:       "one_to_many",
		Through:    "tickets",
		ForeignKey: "owner_id",
		Pivot:      "items_tickets",
	})
	err := m.Validate("2.0.0")
	if err == nil || !strings.Contains(err.Error(), "pivot") {
		t.Fatalf("expected pivot-not-allowed error, got %v", err)
	}
}

func TestValidate_Relations_ManyToManyRequiresPivot(t *testing.T) {
	m := withRelations(manifest.RelationDef{
		Name:       "tags",
		Kind:       "many_to_many",
		Through:    "tags",
		ForeignKey: "item_id",
		// Pivot intentionally empty.
	})
	err := m.Validate("2.0.0")
	if err == nil || !strings.Contains(err.Error(), "pivot") {
		t.Fatalf("expected pivot-required error, got %v", err)
	}
}

func TestValidate_Relations_ManyToManyBadPivot(t *testing.T) {
	m := withRelations(manifest.RelationDef{
		Name:       "tags",
		Kind:       "many_to_many",
		Through:    "tags",
		ForeignKey: "item_id",
		Pivot:      "Items Tags",
	})
	err := m.Validate("2.0.0")
	if err == nil || !strings.Contains(err.Error(), "pivot") {
		t.Fatalf("expected invalid-pivot error, got %v", err)
	}
}

func TestValidate_Relations_DuplicateName(t *testing.T) {
	m := withRelations(
		manifest.RelationDef{Name: "tickets", Kind: "one_to_many", Through: "tickets", ForeignKey: "owner_id"},
		manifest.RelationDef{Name: "tickets", Kind: "one_to_many", Through: "tickets", ForeignKey: "approver_id"},
	)
	err := m.Validate("2.0.0")
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("expected duplicate-name error, got %v", err)
	}
}

func TestValidate_Relations_MultipleNamedDistinctly(t *testing.T) {
	// A model can carry several relations to the same target as long as
	// each Name is distinct — the SDK addresses them by Name, not by
	// (target, kind), so this is the supported way to model "owner" vs
	// "approver" pointing at the same users table.
	m := withRelations(
		manifest.RelationDef{Name: "owned_tickets", Kind: "one_to_many", Through: "tickets", ForeignKey: "owner_id"},
		manifest.RelationDef{Name: "approved_tickets", Kind: "one_to_many", Through: "tickets", ForeignKey: "approver_id"},
		manifest.RelationDef{Name: "tags", Kind: "many_to_many", Through: "tags", ForeignKey: "user_id", Pivot: "users_tags"},
	)
	if err := m.Validate("2.0.0"); err != nil {
		t.Fatalf("distinct relation names should pass, got %v", err)
	}
}

func TestValidate_Relations_ErrorPathIncludesModelIndex(t *testing.T) {
	// The model loop stitches "manifest.model_definitions[i]." onto the
	// relation error so operators can grep the full path. Verify the
	// stitching survives so a future refactor doesn't quietly lose it.
	m := manifest.Manifest{
		Key:     "rel",
		Name:    "Rel",
		Version: "1.0.0",
		ModelDefinitions: []manifest.ModelDefinition{
			{
				TableName: "first",
				ModelKey:  "first",
				Columns:   []manifest.ColumnDef{{Name: "title", Type: "string"}},
			},
			{
				TableName: "second",
				ModelKey:  "second",
				Columns:   []manifest.ColumnDef{{Name: "title", Type: "string"}},
				Relations: []manifest.RelationDef{{
					Name:       "broken",
					Kind:       "one_to_many",
					Through:    "tickets",
					ForeignKey: "Bad Key",
				}},
			},
		},
	}
	err := m.Validate("2.0.0")
	if err == nil || !strings.Contains(err.Error(), "model_definitions[1]") || !strings.Contains(err.Error(), "relations[0]") {
		t.Fatalf("expected fully-qualified path in error, got %v", err)
	}
}
