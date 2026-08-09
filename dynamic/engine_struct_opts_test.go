package dynamic

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/asteby/metacore-kernel/manifest"
	"gorm.io/gorm"
)

func fieldByName(t reflect.Type, name string) (reflect.StructField, bool) {
	for i := 0; i < t.NumField(); i++ {
		if t.Field(i).Name == name {
			return t.Field(i), true
		}
	}
	return reflect.StructField{}, false
}

// The zero-value StructOptions must reproduce BuildStructType exactly.
func TestBuildStructTypeWithOptions_ZeroValueUnchanged(t *testing.T) {
	def := sampleDef()
	got, err := BuildStructTypeWithOptions(def, StructOptions{})
	if err != nil {
		t.Fatalf("BuildStructTypeWithOptions: %v", err)
	}
	want, err := BuildStructType(def)
	if err != nil {
		t.Fatalf("BuildStructType: %v", err)
	}
	if got != want {
		t.Fatalf("zero-value opts diverged:\n got=%v\nwant=%v", got, want)
	}
	// Default soft-delete field stays *time.Time, and there is no CreatedByID.
	f, ok := fieldByName(got, "DeletedAt")
	if !ok {
		t.Fatal("expected DeletedAt field on soft-delete def")
	}
	if f.Type != reflect.TypeOf(&time.Time{}) {
		t.Fatalf("default DeletedAt = %v, want *time.Time", f.Type)
	}
	if _, ok := fieldByName(got, "CreatedByID"); ok {
		t.Fatal("default struct must not carry CreatedByID")
	}
}

func TestBuildStructTypeWithOptions_SoftDeleteGorm(t *testing.T) {
	// Even a def WITHOUT SoftDelete gets the gorm.DeletedAt field.
	def := sampleDef()
	def.SoftDelete = false
	got, err := BuildStructTypeWithOptions(def, StructOptions{SoftDeleteGorm: true})
	if err != nil {
		t.Fatalf("BuildStructTypeWithOptions: %v", err)
	}
	f, ok := fieldByName(got, "DeletedAt")
	if !ok {
		t.Fatal("expected DeletedAt field with SoftDeleteGorm")
	}
	if f.Type != reflect.TypeOf(gorm.DeletedAt{}) {
		t.Fatalf("DeletedAt type = %v, want gorm.DeletedAt", f.Type)
	}
}

func TestBuildStructTypeWithOptions_IncludeCreatedBy(t *testing.T) {
	def := sampleDef()
	got, err := BuildStructTypeWithOptions(def, StructOptions{IncludeCreatedBy: true})
	if err != nil {
		t.Fatalf("BuildStructTypeWithOptions: %v", err)
	}
	f, ok := fieldByName(got, "CreatedByID")
	if !ok {
		t.Fatal("expected CreatedByID field with IncludeCreatedBy")
	}
	if f.Type != reflect.PtrTo(uuidType) {
		t.Fatalf("CreatedByID type = %v, want *uuid.UUID", f.Type)
	}
	if j := f.Tag.Get("json"); j != "created_by_id" {
		t.Fatalf("CreatedByID json tag = %q, want created_by_id", j)
	}
}

func TestOpsCompatStructOptions(t *testing.T) {
	want := StructOptions{SoftDeleteGorm: true, IncludeCreatedBy: true, AlwaysOrgField: true}
	if OpsCompatStructOptions() != want {
		t.Fatalf("OpsCompatStructOptions() = %+v, want %+v", OpsCompatStructOptions(), want)
	}
	def := sampleDef()
	def.OrgScoped = false // intentional: ops DDL still has organization_id
	got, err := NewSchemaEngine().ToReflectTypeWithOptions(def, OpsCompatStructOptions())
	if err != nil {
		t.Fatalf("ToReflectTypeWithOptions: %v", err)
	}
	df, _ := fieldByName(got, "DeletedAt")
	if df.Type != reflect.TypeOf(gorm.DeletedAt{}) {
		t.Fatalf("DeletedAt = %v, want gorm.DeletedAt", df.Type)
	}
	if _, ok := fieldByName(got, "CreatedByID"); !ok {
		t.Fatal("expected CreatedByID with ops-compat options")
	}
	of, ok := fieldByName(got, "OrganizationID")
	if !ok {
		t.Fatal("expected OrganizationID with ops-compat AlwaysOrgField (OrgScoped=false must not drop tenant field)")
	}
	if j := of.Tag.Get("json"); j != "organization_id" {
		t.Fatalf("OrganizationID json tag = %q, want organization_id", j)
	}
}

// TestBuildStructType_OrgScopedFalseOmitsOrgField documents the trap AlwaysOrgField
// closes: zero-value opts + OrgScoped=false → no organization_id on the struct,
// while SingleSchemaDDLOptions still emits the column. Hosts that key tenant
// filters off the struct field then list unscoped.
func TestBuildStructType_OrgScopedFalseOmitsOrgField(t *testing.T) {
	def := sampleDef()
	def.OrgScoped = false
	got, err := BuildStructType(def)
	if err != nil {
		t.Fatalf("BuildStructType: %v", err)
	}
	if _, ok := fieldByName(got, "OrganizationID"); ok {
		t.Fatal("zero-value BuildStructType must omit OrganizationID when OrgScoped=false")
	}
}

func TestAddColumnDDL_DefaultAndOpsCompat(t *testing.T) {
	eng := NewSchemaEngine()
	col := manifest.ColumnDef{Name: "weight", Type: "float"}

	def, err := eng.AddColumnDDL("public", "widgets", col, DDLOptions{})
	if err != nil {
		t.Fatalf("AddColumnDDL default: %v", err)
	}
	if !strings.HasPrefix(def, `ALTER TABLE "public"."widgets" ADD COLUMN IF NOT EXISTS "weight" `) {
		t.Fatalf("unexpected ALTER prefix: %q", def)
	}
	if !strings.Contains(def, "numeric(18,4)") {
		t.Fatalf("default float type wrong: %q", def)
	}

	ops, err := eng.AddColumnDDL("public", "widgets", col, SingleSchemaDDLOptions("public"))
	if err != nil {
		t.Fatalf("AddColumnDDL ops-compat: %v", err)
	}
	if !strings.Contains(ops, "double precision") {
		t.Fatalf("ops-compat float type wrong: %q", ops)
	}
}

func TestAddColumnsDDL_Multiple(t *testing.T) {
	eng := NewSchemaEngine()
	cols := []manifest.ColumnDef{
		{Name: "a", Type: "string"},
		{Name: "b", Type: "int"},
	}
	stmts, err := eng.AddColumnsDDL("public", "widgets", cols, DDLOptions{})
	if err != nil {
		t.Fatalf("AddColumnsDDL: %v", err)
	}
	if len(stmts) != 2 {
		t.Fatalf("want 2 statements, got %d", len(stmts))
	}
	if !strings.Contains(stmts[0], `"a" varchar(255)`) || !strings.Contains(stmts[1], `"b" integer`) {
		t.Fatalf("unexpected statements: %v", stmts)
	}
}
