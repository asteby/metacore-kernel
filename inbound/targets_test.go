package inbound

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/asteby/metacore-kernel/modelbase"
)

func TestSystemFieldsUnion(t *testing.T) {
	want := []string{"id", "organization_id", "tenant_id", "branch_id", "entity_id", "created_by_id",
		"created_by", "updated_by", "created_at", "updated_at", "deleted_at"}
	if len(SystemFields) != len(want) {
		t.Fatalf("SystemFields has %d entries, want %d", len(SystemFields), len(want))
	}
	for _, f := range want {
		if !SystemFields[f] || !forbidden[f] {
			t.Fatalf("%s must be a system field and forbidden", f)
		}
	}
}

func TestWritableColumnsFlags(t *testing.T) {
	cases := []struct {
		name string
		col  modelbase.ColumnDef
		ok   bool
	}{
		{"plain", modelbase.ColumnDef{Key: "sku"}, true},
		{"system", modelbase.ColumnDef{Key: "created_by"}, false},
		{"system tenant", modelbase.ColumnDef{Key: "organization_id"}, false},
		{"readonly", modelbase.ColumnDef{Key: "sku", Readonly: true}, false},
		{"generated", modelbase.ColumnDef{Key: "total", Generated: "a*b"}, false},
		{"protected", modelbase.ColumnDef{Key: "status", Protected: true}, false},
		{"empty key", modelbase.ColumnDef{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := WritableColumns(&modelbase.TableMetadata{Columns: []modelbase.ColumnDef{tc.col}})
			if (len(got) == 1) != tc.ok {
				t.Fatalf("writable=%v want %v (%v)", len(got) == 1, tc.ok, got)
			}
		})
	}
	if got := WritableColumns(nil); got != nil {
		t.Fatalf("nil meta: %v", got)
	}
	got := WritableColumns(&modelbase.TableMetadata{Columns: []modelbase.ColumnDef{
		{Key: "sku", Label: "SKU", Type: "text", Required: true, Unique: true, Default: "x", Ref: "r"},
	}})
	if !reflect.DeepEqual(got, []TargetColumn{{Name: "sku", Label: "SKU", Type: "text", Required: true, Unique: true, Ref: "r", Default: "x"}}) {
		t.Fatalf("column projection: %+v", got)
	}
}

func TestMatchCandidates(t *testing.T) {
	meta := &modelbase.TableMetadata{
		Columns: []modelbase.ColumnDef{
			{Key: "id", Unique: true},
			{Key: "organization_id"},
			{Key: "sku", Unique: true},
			{Key: "warehouse"},
			{Key: "external_id"},
			{Key: "total", Generated: "1"},
		},
		UniqueIndexes: []modelbase.UniqueIndexMeta{
			{Columns: []string{"organization_id", "warehouse", "external_id"}}, // composite, org ignored
			{Columns: []string{"external_id"}, Partial: true},                  // partial WHERE deleted_at IS NULL
			{Columns: []string{"organization_id", "sku"}},                      // dup of the unique column
			{Columns: []string{"organization_id"}},                             // only tenant: nothing to match
			{Columns: []string{"warehouse", "total"}},                          // generated: not suppliable
			{Columns: []string{"warehouse", "ghost"}},                          // undeclared
		},
	}
	want := [][]string{{"external_id"}, {"sku"}, {"warehouse", "external_id"}}
	if got := MatchCandidates(meta); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if got := MatchCandidates(nil); len(got) != 0 {
		t.Fatalf("nil meta: %v", got)
	}
}

func TestTargetFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/targets.json")
	if err != nil {
		t.Fatal(err)
	}
	var targets []Target
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&targets); err != nil {
		t.Fatalf("fixture does not match Target: %v", err)
	}
	if len(targets) != 1 || targets[0].Model != "inventory.items" || len(targets[0].Columns) != 4 ||
		!targets[0].Permissions.Update || targets[0].Permissions.Delete || !targets[0].Scope.Branch {
		t.Fatalf("fixture content: %+v", targets)
	}
	if tg := targets[0]; len(tg.ActionsAvailable) != 1 || tg.ActionsAvailable[0].Key != "reserve" || tg.Stage == nil || tg.Stage.Field != "status" ||
		len(tg.Stage.Transitions) != 2 || !tg.Restorable || tg.IngestCursor != "source_updated_at" {
		t.Fatalf("fixture state-op fields: %+v", tg)
	}
	// Round trip keeps snake_case keys the SDK reads.
	out, _ := json.Marshal(targets[0])
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	for _, k := range []string{"addon", "addon_name", "model", "columns", "match_candidates", "soft_delete", "actions", "permissions", "scope", "guard_candidates", "actions_available", "stage", "restorable", "ingest_cursor"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("missing key %q in %s", k, out)
		}
	}
}

// flagStore serves metadata with flagged columns.
type flagStore struct{ *fakeDyn }

func (flagStore) TableMetadata(context.Context, string) (*modelbase.TableMetadata, error) {
	return &modelbase.TableMetadata{
		Columns: []modelbase.ColumnDef{
			{Key: "sku"}, {Key: "quantity"}, {Key: "created_by"}, {Key: "locked", Readonly: true},
			{Key: "total", Generated: "1"}, {Key: "state", Protected: true},
		},
		Audit: &modelbase.AuditMeta{DeletedAt: "deleted_at"},
	}, nil
}

func TestCheckContractRejectsNonWritableMapping(t *testing.T) {
	d := &DynamicExecutor{store: flagStore{newFakeDyn()}}
	ctx := context.Background()
	mk := func(to string) Contract {
		return Contract{Key: "stock.set", Version: 1, Actions: []Action{{
			Do: ActionRecordUpsert, Model: "inv", Match: []string{"sku"},
			Mapping: map[string]string{"sku": "{{payload.sku}}", to: "{{payload.x}}"},
		}}}
	}
	if err := d.CheckContract(ctx, mk("quantity")); err != nil {
		t.Fatalf("writable mapping must register: %v", err)
	}
	for _, to := range []string{"created_by", "locked", "total", "state", "ghost"} {
		if err := d.CheckContract(ctx, mk(to)); !errors.Is(err, ErrInvalidContract) {
			t.Fatalf("mapping to %q must be rejected, got %v", to, err)
		}
	}
	rc := Contract{Key: "stock.route", Version: 1, Route: Route{Model: "inv", Op: OpCreate, Mapping: map[string]string{"state": "{{payload.s}}"}}}
	if err := d.CheckContract(ctx, rc); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("route mapping to protected column: %v", err)
	}
}
