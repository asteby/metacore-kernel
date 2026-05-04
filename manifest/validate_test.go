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

// withColumn returns a minimal valid manifest containing a single column —
// helper to keep the column-extension tests focused on the field under test.
func withColumn(col manifest.ColumnDef) manifest.Manifest {
	return manifest.Manifest{
		Key:     "ext",
		Name:    "Ext",
		Version: "1.0.0",
		ModelDefinitions: []manifest.ModelDefinition{{
			TableName: "items",
			ModelKey:  "items",
			Columns:   []manifest.ColumnDef{col},
		}},
	}
}

func ptrF(f float64) *float64 { return &f }

func TestValidate_ColumnExtensions_ZeroValueIsBackwardsCompat(t *testing.T) {
	// A column without any of the new fields populated must validate
	// exactly like before — this is the contract the task hinges on.
	m := withColumn(manifest.ColumnDef{Name: "title", Type: "string"})
	if err := m.Validate("2.0.0"); err != nil {
		t.Fatalf("zero-value extension fields should still validate, got %v", err)
	}
}

func TestValidate_ColumnVisibility(t *testing.T) {
	for _, v := range []string{"all", "table", "modal", "list"} {
		m := withColumn(manifest.ColumnDef{Name: "title", Type: "string", Visibility: v})
		if err := m.Validate("2.0.0"); err != nil {
			t.Errorf("visibility=%q should be accepted, got %v", v, err)
		}
	}
}

func TestValidate_ColumnVisibilityRejected(t *testing.T) {
	m := withColumn(manifest.ColumnDef{Name: "title", Type: "string", Visibility: "everywhere"})
	err := m.Validate("2.0.0")
	if err == nil || !strings.Contains(err.Error(), "visibility") {
		t.Fatalf("expected visibility error, got %v", err)
	}
}

func TestValidate_ColumnWidgetAccepted(t *testing.T) {
	m := withColumn(manifest.ColumnDef{Name: "title", Type: "string", Widget: "textarea"})
	if err := m.Validate("2.0.0"); err != nil {
		t.Fatalf("widget=textarea should be accepted, got %v", err)
	}
}

func TestValidate_ColumnWidgetRejected(t *testing.T) {
	m := withColumn(manifest.ColumnDef{Name: "title", Type: "string", Widget: "neural-blob"})
	err := m.Validate("2.0.0")
	if err == nil || !strings.Contains(err.Error(), "widget") {
		t.Fatalf("expected widget error, got %v", err)
	}
}

func TestValidate_ColumnSearchable(t *testing.T) {
	// Searchable is a plain bool — there is no invalid state, just
	// confirm it survives a round trip through Validate without
	// upsetting the existing checks.
	m := withColumn(manifest.ColumnDef{Name: "title", Type: "string", Searchable: true})
	if err := m.Validate("2.0.0"); err != nil {
		t.Fatalf("searchable=true should validate, got %v", err)
	}
}

func TestValidate_ValidationRegexCompiles(t *testing.T) {
	m := withColumn(manifest.ColumnDef{
		Name:       "tax_id",
		Type:       "string",
		Validation: &manifest.ValidationRule{Regex: `^[A-Z0-9]{12,13}$`},
	})
	if err := m.Validate("2.0.0"); err != nil {
		t.Fatalf("valid regex should pass, got %v", err)
	}
}

func TestValidate_ValidationRegexBroken(t *testing.T) {
	m := withColumn(manifest.ColumnDef{
		Name:       "tax_id",
		Type:       "string",
		Validation: &manifest.ValidationRule{Regex: `(unclosed`},
	})
	err := m.Validate("2.0.0")
	if err == nil || !strings.Contains(err.Error(), "regex") {
		t.Fatalf("expected regex compile error, got %v", err)
	}
}

func TestValidate_ValidationMinMaxOK(t *testing.T) {
	m := withColumn(manifest.ColumnDef{
		Name:       "age",
		Type:       "int",
		Validation: &manifest.ValidationRule{Min: ptrF(0), Max: ptrF(150)},
	})
	if err := m.Validate("2.0.0"); err != nil {
		t.Fatalf("min<=max should pass, got %v", err)
	}
}

func TestValidate_ValidationMinGreaterThanMax(t *testing.T) {
	m := withColumn(manifest.ColumnDef{
		Name:       "age",
		Type:       "int",
		Validation: &manifest.ValidationRule{Min: ptrF(10), Max: ptrF(5)},
	})
	err := m.Validate("2.0.0")
	if err == nil || !strings.Contains(err.Error(), "min") {
		t.Fatalf("expected min>max error, got %v", err)
	}
}

func TestValidate_ValidationMinOnlyIsOK(t *testing.T) {
	// Only Min set — there is no Max to compare against, must pass.
	m := withColumn(manifest.ColumnDef{
		Name:       "age",
		Type:       "int",
		Validation: &manifest.ValidationRule{Min: ptrF(18)},
	})
	if err := m.Validate("2.0.0"); err != nil {
		t.Fatalf("min-only should pass, got %v", err)
	}
}

func TestValidate_ValidationCustomShape(t *testing.T) {
	good := withColumn(manifest.ColumnDef{
		Name:       "rfc",
		Type:       "string",
		Validation: &manifest.ValidationRule{Custom: "rfc.tax_id"},
	})
	if err := good.Validate("2.0.0"); err != nil {
		t.Fatalf("dotted custom should pass, got %v", err)
	}

	bad := withColumn(manifest.ColumnDef{
		Name:       "rfc",
		Type:       "string",
		Validation: &manifest.ValidationRule{Custom: "Has Space!"},
	})
	if err := bad.Validate("2.0.0"); err == nil || !strings.Contains(err.Error(), "custom") {
		t.Fatalf("expected custom shape error, got %v", err)
	}
}

func TestValidate_AllColumnExtensionsTogether(t *testing.T) {
	m := withColumn(manifest.ColumnDef{
		Name:       "email",
		Type:       "string",
		Visibility: "modal",
		Searchable: true,
		Widget:     "email",
		Validation: &manifest.ValidationRule{
			Regex:  `^[^@]+@[^@]+\.[^@]+$`,
			Min:    ptrF(3),
			Max:    ptrF(254),
			Custom: "email.deliverable",
		},
	})
	if err := m.Validate("2.0.0"); err != nil {
		t.Fatalf("rich column should validate, got %v", err)
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
	// before — same backwards-compat contract as the column extension.
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

// withActions returns a minimal valid manifest that hangs the supplied
// ActionDef under model key "tickets". Mirrors the helpers above so each
// trigger test stays focused on the field under check.
func withActions(actions ...manifest.ActionDef) manifest.Manifest {
	return manifest.Manifest{
		Key:     "act",
		Name:    "Act",
		Version: "1.0.0",
		Actions: map[string][]manifest.ActionDef{
			"tickets": actions,
		},
	}
}

// withWasmActions is identical to withActions but also wires a Backend
// declaration so wasm triggers can resolve their Export. The exports
// argument is variadic so tests pick whichever symbols they expect to be
// present (or absent) in the backend declaration.
func withWasmActions(exports []string, actions ...manifest.ActionDef) manifest.Manifest {
	m := withActions(actions...)
	m.Backend = &manifest.BackendSpec{
		Runtime: "wasm",
		Entry:   "backend/b.wasm",
		Exports: exports,
	}
	return m
}

func TestValidate_ActionTrigger_NilIsBackwardsCompat(t *testing.T) {
	// An ActionDef without Trigger must validate just like before — the
	// addon ecosystem has months of manifests with no trigger field set.
	m := withActions(manifest.ActionDef{Key: "escalate", Name: "Escalate", Label: "Escalate"})
	if err := m.Validate("2.0.0"); err != nil {
		t.Fatalf("nil trigger should still validate, got %v", err)
	}
}

func TestValidate_ActionTrigger_WasmOK(t *testing.T) {
	m := withWasmActions([]string{"escalateTicket"}, manifest.ActionDef{
		Key:     "escalate",
		Name:    "Escalate",
		Label:   "Escalate",
		Trigger: &manifest.ActionTrigger{Type: "wasm", Export: "escalateTicket", RunInTx: true},
	})
	if err := m.Validate("2.0.0"); err != nil {
		t.Fatalf("valid wasm trigger should pass, got %v", err)
	}
}

func TestValidate_ActionTrigger_WasmRequiresExport(t *testing.T) {
	m := withWasmActions([]string{"escalateTicket"}, manifest.ActionDef{
		Key:     "escalate",
		Name:    "Escalate",
		Label:   "Escalate",
		Trigger: &manifest.ActionTrigger{Type: "wasm"},
	})
	err := m.Validate("2.0.0")
	if err == nil || !strings.Contains(err.Error(), "export") {
		t.Fatalf("expected export-required error, got %v", err)
	}
}

func TestValidate_ActionTrigger_WasmExportNotInBackend(t *testing.T) {
	// The export must appear in Backend.Exports so the wasm host can
	// resolve it at dispatch — same contract enforced for hooks.
	m := withWasmActions([]string{"otherSymbol"}, manifest.ActionDef{
		Key:     "escalate",
		Name:    "Escalate",
		Label:   "Escalate",
		Trigger: &manifest.ActionTrigger{Type: "wasm", Export: "escalateTicket"},
	})
	err := m.Validate("2.0.0")
	if err == nil || !strings.Contains(err.Error(), "backend.exports") {
		t.Fatalf("expected export-mismatch error, got %v", err)
	}
}

func TestValidate_ActionTrigger_WasmExportInvalidSymbol(t *testing.T) {
	m := withWasmActions([]string{"with space"}, manifest.ActionDef{
		Key:     "escalate",
		Name:    "Escalate",
		Label:   "Escalate",
		Trigger: &manifest.ActionTrigger{Type: "wasm", Export: "with space"},
	})
	err := m.Validate("2.0.0")
	if err == nil || !strings.Contains(err.Error(), "invalid symbol") {
		t.Fatalf("expected invalid-symbol error, got %v", err)
	}
}

func TestValidate_ActionTrigger_WasmWithoutBackendExports(t *testing.T) {
	// No Backend declared at all — wasm trigger has nothing to point at.
	m := withActions(manifest.ActionDef{
		Key:     "escalate",
		Name:    "Escalate",
		Label:   "Escalate",
		Trigger: &manifest.ActionTrigger{Type: "wasm", Export: "escalateTicket"},
	})
	err := m.Validate("2.0.0")
	if err == nil || !strings.Contains(err.Error(), "backend.exports") {
		t.Fatalf("expected export-mismatch error when Backend is nil, got %v", err)
	}
}

func TestValidate_ActionTrigger_WebhookOK(t *testing.T) {
	m := withActions(manifest.ActionDef{
		Key:     "escalate",
		Name:    "Escalate",
		Label:   "Escalate",
		Trigger: &manifest.ActionTrigger{Type: "webhook"},
	})
	if err := m.Validate("2.0.0"); err != nil {
		t.Fatalf("webhook trigger without export should pass, got %v", err)
	}
}

func TestValidate_ActionTrigger_WebhookRejectsExport(t *testing.T) {
	m := withActions(manifest.ActionDef{
		Key:     "escalate",
		Name:    "Escalate",
		Label:   "Escalate",
		Trigger: &manifest.ActionTrigger{Type: "webhook", Export: "shouldNotBeHere"},
	})
	err := m.Validate("2.0.0")
	if err == nil || !strings.Contains(err.Error(), "export") {
		t.Fatalf("expected export-not-allowed error, got %v", err)
	}
}

func TestValidate_ActionTrigger_WebhookRejectsRunInTx(t *testing.T) {
	// A webhook hop escapes the request transaction, so honouring RunInTx
	// would silently lie. Reject at authoring time.
	m := withActions(manifest.ActionDef{
		Key:     "escalate",
		Name:    "Escalate",
		Label:   "Escalate",
		Trigger: &manifest.ActionTrigger{Type: "webhook", RunInTx: true},
	})
	err := m.Validate("2.0.0")
	if err == nil || !strings.Contains(err.Error(), "run_in_tx") {
		t.Fatalf("expected run_in_tx-not-allowed error, got %v", err)
	}
}

func TestValidate_ActionTrigger_NoopOK(t *testing.T) {
	m := withActions(manifest.ActionDef{
		Key:     "track",
		Name:    "Track",
		Label:   "Track",
		Trigger: &manifest.ActionTrigger{Type: "noop"},
	})
	if err := m.Validate("2.0.0"); err != nil {
		t.Fatalf("noop trigger should pass, got %v", err)
	}
}

func TestValidate_ActionTrigger_NoopRejectsExport(t *testing.T) {
	m := withActions(manifest.ActionDef{
		Key:     "track",
		Name:    "Track",
		Label:   "Track",
		Trigger: &manifest.ActionTrigger{Type: "noop", Export: "irrelevant"},
	})
	err := m.Validate("2.0.0")
	if err == nil || !strings.Contains(err.Error(), "export") {
		t.Fatalf("expected export-not-allowed error, got %v", err)
	}
}

func TestValidate_ActionTrigger_NoopRejectsRunInTx(t *testing.T) {
	m := withActions(manifest.ActionDef{
		Key:     "track",
		Name:    "Track",
		Label:   "Track",
		Trigger: &manifest.ActionTrigger{Type: "noop", RunInTx: true},
	})
	err := m.Validate("2.0.0")
	if err == nil || !strings.Contains(err.Error(), "run_in_tx") {
		t.Fatalf("expected run_in_tx-not-allowed error, got %v", err)
	}
}

func TestValidate_ActionTrigger_UnknownType(t *testing.T) {
	m := withActions(manifest.ActionDef{
		Key:     "track",
		Name:    "Track",
		Label:   "Track",
		Trigger: &manifest.ActionTrigger{Type: "queue"},
	})
	err := m.Validate("2.0.0")
	if err == nil || !strings.Contains(err.Error(), "trigger.type") {
		t.Fatalf("expected unknown-type error, got %v", err)
	}
}

func TestValidate_ActionTrigger_OnExtension(t *testing.T) {
	// Triggers on actions added via ModelExtension follow the same rules.
	m := manifest.Manifest{
		Key:     "act",
		Name:    "Act",
		Version: "1.0.0",
		Backend: &manifest.BackendSpec{
			Runtime: "wasm",
			Entry:   "backend/b.wasm",
			Exports: []string{"escalateTicket"},
		},
		Extensions: []manifest.ModelExtension{{
			Model: "tickets",
			Actions: []manifest.ActionDef{{
				Key:     "escalate",
				Name:    "Escalate",
				Label:   "Escalate",
				Trigger: &manifest.ActionTrigger{Type: "wasm", Export: "escalateTicket"},
			}},
		}},
	}
	if err := m.Validate("2.0.0"); err != nil {
		t.Fatalf("extension trigger should validate, got %v", err)
	}
}

func TestValidate_ActionTrigger_ErrorPathIncludesIndex(t *testing.T) {
	// The path stitched onto an actions-map error should surface both the
	// model key and the slice index so operators can grep it.
	m := manifest.Manifest{
		Key:     "act",
		Name:    "Act",
		Version: "1.0.0",
		Actions: map[string][]manifest.ActionDef{
			"tickets": {
				{Key: "ok", Name: "Ok", Label: "Ok"},
				{
					Key:     "broken",
					Name:    "Broken",
					Label:   "Broken",
					Trigger: &manifest.ActionTrigger{Type: "queue"},
				},
			},
		},
	}
	err := m.Validate("2.0.0")
	if err == nil ||
		!strings.Contains(err.Error(), `actions["tickets"][1]`) ||
		!strings.Contains(err.Error(), "trigger.type") {
		t.Fatalf("expected fully-qualified action path in error, got %v", err)
	}
}

func TestValidate_Relations_ErrorPathIncludesModelIndex(t *testing.T) {
	// The model loop stitches "manifest.model_definitions[i]." onto the
	// relation error so operators can grep the full path. Verify the
	// stitching survives — past refactors lost it once already.
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
