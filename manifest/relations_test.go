package manifest

import "testing"

func TestAutoDeriveColumnRefs_BelongsToStampsRef(t *testing.T) {
	def := &ModelDefinition{
		TableName: "invoices",
		ModelKey:  "invoices",
		Columns: []ColumnDef{
			{Name: "customer_id", Type: "uuid"},
			{Name: "amount", Type: "decimal"},
		},
		Relations: []RelationDef{
			{Name: "customer", Kind: "belongs_to", Through: "customers", ForeignKey: "customer_id"},
		},
	}
	AutoDeriveColumnRefs(def)
	if got := def.Columns[0].Ref; got != "customers" {
		t.Fatalf("expected customer_id.Ref=customers, got %q", got)
	}
	if got := def.Columns[1].Ref; got != "" {
		t.Fatalf("amount column should not have Ref, got %q", got)
	}
}

func TestAutoDeriveColumnRefs_AuthorOverrideWins(t *testing.T) {
	def := &ModelDefinition{
		Columns: []ColumnDef{
			{Name: "customer_id", Type: "uuid", Ref: "alt_customers"},
		},
		Relations: []RelationDef{
			{Name: "customer", Kind: "belongs_to", Through: "customers", ForeignKey: "customer_id"},
		},
	}
	AutoDeriveColumnRefs(def)
	if got := def.Columns[0].Ref; got != "alt_customers" {
		t.Fatalf("author-provided Ref should win, got %q", got)
	}
}

func TestAutoDeriveColumnRefs_OneToManyDoesNotStampOwnerColumn(t *testing.T) {
	// one_to_many's FK is on the OTHER model, so the owner's column should
	// not gain a Ref from it.
	def := &ModelDefinition{
		Columns: []ColumnDef{{Name: "name", Type: "string"}},
		Relations: []RelationDef{
			{Name: "items", Kind: "one_to_many", Through: "invoice_items", ForeignKey: "invoice_id"},
		},
	}
	AutoDeriveColumnRefs(def)
	if def.Columns[0].Ref != "" {
		t.Fatalf("one_to_many should not stamp owner columns, got %q", def.Columns[0].Ref)
	}
}

func TestAutoDeriveColumnRefs_NilOrEmpty_NoOp(t *testing.T) {
	if AutoDeriveColumnRefs(nil) != nil {
		t.Fatalf("nil input must return nil")
	}
	def := &ModelDefinition{Columns: []ColumnDef{{Name: "x"}}}
	AutoDeriveColumnRefs(def)
	if def.Columns[0].Ref != "" {
		t.Fatalf("no relations declared, expected no Ref")
	}
}

func TestValidate_BelongsToRelationKindAccepted(t *testing.T) {
	m := &Manifest{
		Key: "test_addon", Name: "T", Version: "1.0.0",
		ModelDefinitions: []ModelDefinition{{
			TableName: "invoices", ModelKey: "invoices",
			Columns: []ColumnDef{{Name: "id", Type: "uuid"}},
			Relations: []RelationDef{
				{Name: "customer", Kind: "belongs_to", Through: "customers", ForeignKey: "customer_id"},
			},
		}},
	}
	if err := m.Validate("0.9.0"); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestValidate_BelongsToRejectsPivot(t *testing.T) {
	m := &Manifest{
		Key: "test_addon", Name: "T", Version: "1.0.0",
		ModelDefinitions: []ModelDefinition{{
			TableName: "invoices", ModelKey: "invoices",
			Columns: []ColumnDef{{Name: "id", Type: "uuid"}},
			Relations: []RelationDef{
				{Name: "customer", Kind: "belongs_to", Through: "customers", ForeignKey: "customer_id", Pivot: "should_not_be_here"},
			},
		}},
	}
	if err := m.Validate("0.9.0"); err == nil {
		t.Fatal("expected error for belongs_to with pivot")
	}
}

func TestValidationCustomAcceptsOrgRef(t *testing.T) {
	min := float64(0)
	rule := &ValidationRule{Custom: "$org.tax_id_validator", Min: &min}
	if err := rule.validate(); err != nil {
		t.Fatalf("$org.<key> reference must validate, got %v", err)
	}
}

func TestValidationCustomRejectsBadRef(t *testing.T) {
	rule := &ValidationRule{Custom: "$org."}
	if err := rule.validate(); err == nil {
		t.Fatal("empty $org key must error")
	}
	rule = &ValidationRule{Custom: "$other.key"}
	if err := rule.validate(); err == nil {
		t.Fatal("non-$org prefix without dotted identifier must error")
	}
}
