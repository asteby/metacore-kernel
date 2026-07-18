package dynamic

import (
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
)

// jsonSchemaEnum mirrors the "type" enum in
// manifest/v3/schema/manifest-v3.schema.json. Every value the JSON Schema
// admits MUST pass ValidateColumnType, otherwise an addon that clears the
// schema gate would still fail at install (the vector divergence this closes).
var jsonSchemaEnum = []string{
	"uuid", "text", "integer", "bigint", "numeric", "boolean",
	"timestamp", "timestamptz", "date", "json", "jsonb", "vector",
}

func TestValidateColumnType_JSONSchemaEnumAllPass(t *testing.T) {
	for _, ty := range jsonSchemaEnum {
		if err := ValidateColumnType(ty); err != nil {
			t.Errorf("JSON Schema enum type %q rejected by ValidateColumnType: %v", ty, err)
		}
		// Every schema-valid type must also produce DDL: the two allowlists
		// must never diverge (this is what broke for "vector").
		if _, err := pgColumnType(manifest.ColumnDef{Type: ty}); err != nil {
			t.Errorf("JSON Schema enum type %q accepted by validator but rejected by pgColumnType: %v", ty, err)
		}
	}
}

func TestValidateColumnType_CanonicalAliasesPass(t *testing.T) {
	for _, ty := range CanonicalColumnTypes {
		if err := ValidateColumnType(ty); err != nil {
			t.Errorf("canonical type %q rejected: %v", ty, err)
		}
	}
}

func TestValidateColumnType_ParameterizedPass(t *testing.T) {
	for _, ty := range []string{
		"numeric(18,4)", "numeric(6,2)", "decimal(10,2)",
		"varchar(120)", "char(3)", "vector(768)",
	} {
		if err := ValidateColumnType(ty); err != nil {
			t.Errorf("parameterized type %q rejected: %v", ty, err)
		}
	}
}

func TestValidateColumnType_GarbageFails(t *testing.T) {
	for _, ty := range []string{
		"", "money", "smallint", "real", "serial", "blob",
		"varchar", "numeric()", "vector(abc)", "notatype",
	} {
		if err := ValidateColumnType(ty); err == nil {
			t.Errorf("expected %q to be rejected, but it passed", ty)
		}
	}
}

// TestValidateColumnType_MatchesPgColumnType guarantees, over the canonical
// set plus a batch of parameterized and garbage inputs, that ValidateColumnType
// accepts EXACTLY the inputs pgColumnType can emit DDL for — they share one
// allowlist by construction.
func TestValidateColumnType_MatchesPgColumnType(t *testing.T) {
	cases := append([]string{}, CanonicalColumnTypes...)
	cases = append(cases,
		"numeric(6,2)", "varchar(20)", "vector(3)",
		"money", "serial", "notatype", "varchar", "",
	)
	for _, ty := range cases {
		_, ddlErr := pgColumnType(manifest.ColumnDef{Type: ty})
		valErr := ValidateColumnType(ty)
		if (ddlErr == nil) != (valErr == nil) {
			t.Errorf("divergence for %q: pgColumnType err=%v, ValidateColumnType err=%v", ty, ddlErr, valErr)
		}
	}
}

// TestVectorResolvesConsistently pins the vector divergence fix: a "vector"
// column produces DDL, a Go type, and passes validation — all three planes
// agree.
func TestVectorResolvesConsistently(t *testing.T) {
	if err := ValidateColumnType("vector"); err != nil {
		t.Fatalf("vector rejected by validator: %v", err)
	}
	sql, err := pgColumnType(manifest.ColumnDef{Type: "vector"})
	if err != nil || sql != "vector" {
		t.Fatalf("pgColumnType(vector) = %q, %v; want \"vector\", nil", sql, err)
	}
	sqlN, err := pgColumnType(manifest.ColumnDef{Type: "vector", Size: 768})
	if err != nil || sqlN != "vector(768)" {
		t.Fatalf("pgColumnType(vector,size=768) = %q, %v; want \"vector(768)\", nil", sqlN, err)
	}
	goT, gormT, err := columnGoType(manifest.ColumnDef{Type: "vector", Size: 3})
	if err != nil {
		t.Fatalf("columnGoType(vector) err: %v", err)
	}
	if gormT != "vector(3)" {
		t.Errorf("columnGoType gorm type = %q; want vector(3)", gormT)
	}
	if !strings.Contains(goT.String(), "Vector") {
		t.Errorf("columnGoType Go type = %q; want a pgvector.Vector", goT.String())
	}
}
