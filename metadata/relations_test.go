package metadata

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/modelbase"
)

// relatedModel is a fakeModel variant that ALSO declares Relations so the
// auto-derivation path in computeTable can be exercised.
type relatedModel struct {
	key   string
	title string
	rels  []modelbase.RelationDef
}

func (r *relatedModel) TableName() string { return r.key }
func (r *relatedModel) DefineTable() modelbase.TableMetadata {
	return modelbase.TableMetadata{
		Title: r.title,
		Columns: []modelbase.ColumnDef{
			{Key: "id", Label: "ID", Type: "text"},
			{Key: "customer_id", Label: "Customer", Type: "text"},
			{Key: "amount", Label: "Amount", Type: "number"},
		},
	}
}
func (r *relatedModel) DefineModal() modelbase.ModalMetadata {
	return modelbase.ModalMetadata{
		Fields: []modelbase.FieldDef{
			{Key: "customer_id", Label: "Customer", Type: "select"},
		},
	}
}
func (r *relatedModel) DefineRelations() []modelbase.RelationDef { return r.rels }

func registerRelated(t *testing.T, title string, rels []modelbase.RelationDef) string {
	t.Helper()
	key := fmt.Sprintf("metadata_relations_test_%s_%d", t.Name(), time.Now().UnixNano())
	titleCopy := title
	relsCopy := rels
	modelbase.Register(key, func() modelbase.ModelDefiner {
		return &relatedModel{key: key, title: titleCopy, rels: relsCopy}
	})
	return key
}

func TestService_AutoDerivesRefFromBelongsTo(t *testing.T) {
	svc := New(Config{CacheTTL: time.Minute})
	key := registerRelated(t, "Invoices", []modelbase.RelationDef{
		{Name: "customer", Kind: "belongs_to", Through: "customers", ForeignKey: "customer_id"},
	})
	meta, err := svc.GetTable(context.Background(), key)
	if err != nil {
		t.Fatalf("GetTable: %v", err)
	}
	var customerCol *modelbase.ColumnDef
	for i := range meta.Columns {
		if meta.Columns[i].Key == "customer_id" {
			customerCol = &meta.Columns[i]
		}
	}
	if customerCol == nil {
		t.Fatal("customer_id column missing")
	}
	if customerCol.Ref != "customers" {
		t.Fatalf("expected auto-derived Ref=customers, got %q", customerCol.Ref)
	}
}

func TestService_AutoDerivedRef_AuthorOverrideRespected(t *testing.T) {
	svc := New(Config{CacheTTL: time.Minute})
	// Register a model whose DefineTable already sets Ref.
	key := fmt.Sprintf("metadata_relations_override_%d", time.Now().UnixNano())
	modelbase.Register(key, func() modelbase.ModelDefiner {
		return &overrideRefModel{key: key}
	})
	meta, err := svc.GetTable(context.Background(), key)
	if err != nil {
		t.Fatalf("GetTable: %v", err)
	}
	if meta.Columns[0].Ref != "alt_customers" {
		t.Fatalf("author-provided Ref should win, got %q", meta.Columns[0].Ref)
	}
}

type overrideRefModel struct{ key string }

func (m *overrideRefModel) TableName() string { return m.key }
func (m *overrideRefModel) DefineTable() modelbase.TableMetadata {
	return modelbase.TableMetadata{
		Columns: []modelbase.ColumnDef{
			{Key: "customer_id", Type: "text", Ref: "alt_customers"},
		},
	}
}
func (m *overrideRefModel) DefineModal() modelbase.ModalMetadata { return modelbase.ModalMetadata{} }
func (m *overrideRefModel) DefineRelations() []modelbase.RelationDef {
	return []modelbase.RelationDef{
		{Name: "customer", Kind: "belongs_to", Through: "customers", ForeignKey: "customer_id"},
	}
}

func TestService_OrgConfigResolver_ResolvesValidatorRef(t *testing.T) {
	svc := New(Config{CacheTTL: time.Minute})
	min := float64(13)
	max := float64(13)
	key := fmt.Sprintf("metadata_orgref_test_%d", time.Now().UnixNano())
	modelbase.Register(key, func() modelbase.ModelDefiner {
		return &orgRefModel{
			key: key,
			rule: &modelbase.ValidationRule{
				Custom: "$org.tax_id_validator",
				Min:    &min,
				Max:    &max,
			},
		}
	})

	svc.WithOrgConfigResolver(func(_ context.Context, k string) (string, bool) {
		if k == "tax_id_validator" {
			return "rfc.tax_id", true
		}
		return "", false
	})

	meta, err := svc.GetTable(context.Background(), key)
	if err != nil {
		t.Fatalf("GetTable: %v", err)
	}
	if meta.Columns[0].Validation == nil {
		t.Fatal("validation missing")
	}
	if got := meta.Columns[0].Validation.Custom; got != "rfc.tax_id" {
		t.Fatalf("expected resolved validator, got %q", got)
	}
}

func TestService_OrgConfigResolver_LeavesUnresolvedReferenceInPlace(t *testing.T) {
	svc := New(Config{CacheTTL: time.Minute})
	key := fmt.Sprintf("metadata_orgref_miss_%d", time.Now().UnixNano())
	modelbase.Register(key, func() modelbase.ModelDefiner {
		return &orgRefModel{
			key: key,
			rule: &modelbase.ValidationRule{
				Custom: "$org.unknown",
			},
		}
	})
	svc.WithOrgConfigResolver(func(_ context.Context, k string) (string, bool) {
		return "", false
	})
	meta, err := svc.GetTable(context.Background(), key)
	if err != nil {
		t.Fatalf("GetTable: %v", err)
	}
	if got := meta.Columns[0].Validation.Custom; got != "$org.unknown" {
		t.Fatalf("unresolved reference should pass through, got %q", got)
	}
}

func TestService_OrgConfigResolver_ResolvesFieldDefValidation(t *testing.T) {
	svc := New(Config{CacheTTL: time.Minute})
	key := fmt.Sprintf("metadata_orgref_modal_%d", time.Now().UnixNano())
	modelbase.Register(key, func() modelbase.ModelDefiner {
		return &orgRefModalModel{key: key}
	})
	svc.WithOrgConfigResolver(func(_ context.Context, k string) (string, bool) {
		if k == "tax_id_validator" {
			return "rfc.tax_id", true
		}
		return "", false
	})
	modal, err := svc.GetModal(context.Background(), key)
	if err != nil {
		t.Fatalf("GetModal: %v", err)
	}
	if got := modal.Fields[0].Validation; got != "rfc.tax_id" {
		t.Fatalf("expected resolved validator, got %q", got)
	}
}

type orgRefModel struct {
	key  string
	rule *modelbase.ValidationRule
}

func (m *orgRefModel) TableName() string { return m.key }
func (m *orgRefModel) DefineTable() modelbase.TableMetadata {
	return modelbase.TableMetadata{
		Columns: []modelbase.ColumnDef{
			{Key: "tax_id", Type: "text", Validation: m.rule},
		},
	}
}
func (m *orgRefModel) DefineModal() modelbase.ModalMetadata { return modelbase.ModalMetadata{} }

type orgRefModalModel struct{ key string }

func (m *orgRefModalModel) TableName() string                  { return m.key }
func (m *orgRefModalModel) DefineTable() modelbase.TableMetadata { return modelbase.TableMetadata{} }
func (m *orgRefModalModel) DefineModal() modelbase.ModalMetadata {
	return modelbase.ModalMetadata{
		Fields: []modelbase.FieldDef{
			{Key: "tax_id", Type: "text", Validation: "$org.tax_id_validator"},
		},
	}
}
