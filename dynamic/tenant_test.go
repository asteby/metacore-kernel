package dynamic_test

import (
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/google/uuid"
)

func TestParseIsolation_DefaultsShared(t *testing.T) {
	cases := map[string]dynamic.Isolation{
		"":                   dynamic.IsolationShared,
		"shared":             dynamic.IsolationShared,
		"schema-per-tenant":  dynamic.IsolationPerTenant,
		"database-per-tenant": dynamic.IsolationShared, // reserved, falls through
	}
	for in, want := range cases {
		if got := dynamic.ParseIsolation(in); got != want {
			t.Errorf("ParseIsolation(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSchemaName_Shared(t *testing.T) {
	org := uuid.MustParse("a1b2c3d4-e5f6-7890-abcd-ef1234567890")
	got := dynamic.SchemaName("Tickets", org, dynamic.IsolationShared)
	if got != "addon_tickets" {
		t.Fatalf("shared schema = %q, want addon_tickets", got)
	}
}

func TestSchemaName_PerTenant(t *testing.T) {
	org := uuid.MustParse("a1b2c3d4-e5f6-7890-abcd-ef1234567890")
	got := dynamic.SchemaName("Fiscal", org, dynamic.IsolationPerTenant)
	if !strings.HasPrefix(got, "addon_fiscal_") {
		t.Fatalf("per-tenant schema = %q, want prefix addon_fiscal_", got)
	}
	if !strings.HasSuffix(got, "a1b2c3d4") {
		t.Fatalf("per-tenant schema = %q, want suffix org-short a1b2c3d4", got)
	}
}

func TestSchemaName_PerTenantIsolatesPerOrg(t *testing.T) {
	a := uuid.MustParse("11111111-0000-0000-0000-000000000000")
	b := uuid.MustParse("22222222-0000-0000-0000-000000000000")
	sa := dynamic.SchemaName("hr", a, dynamic.IsolationPerTenant)
	sb := dynamic.SchemaName("hr", b, dynamic.IsolationPerTenant)
	if sa == sb {
		t.Fatalf("expected distinct schemas for different orgs, both = %q", sa)
	}
}

func TestSharedSchemaName(t *testing.T) {
	if got := dynamic.SharedSchemaName("HR"); got != "addon_hr" {
		t.Fatalf("SharedSchemaName = %q, want addon_hr", got)
	}
}
