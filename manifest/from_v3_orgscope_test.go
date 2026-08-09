package manifest_test

import (
	"testing"

	"github.com/asteby/metacore-kernel/dynamic"
	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// TestFromV3_SharedForcesOrgScopedEvenWithoutColumn proves the runtime
// backstop for the POSSalePayment class of bug: a shared-isolation model that
// omits organization_id still becomes OrgScoped=true after FromV3, and
// OpsCompatStructOptions projects the OrganizationID field so hosts that gate
// tenant filters on the struct field cannot list unscoped.
func TestFromV3_SharedForcesOrgScopedEvenWithoutColumn(t *testing.T) {
	m := &v3.Manifest{
		APIVersion: v3.APIVersion,
		Kind:       v3.KindAddon,
		Metadata:   v3.Metadata{Key: "pos", Name: "POS", Version: "0.0.0"},
		Tenancy:    &v3.Tenancy{Isolation: "shared", RLSColumn: "organization_id"},
		Models: []v3.Model{{
			Key:   "POSSalePayment",
			Table: "pos_sale_payments",
			Columns: []v3.Column{
				{Name: "amount", Type: "numeric"},
			},
		}},
	}

	host := manifest.FromV3(m)
	if len(host.ModelDefinitions) != 1 {
		t.Fatalf("expected 1 model, got %d", len(host.ModelDefinitions))
	}
	def := host.ModelDefinitions[0]
	if !def.OrgScoped {
		t.Fatal("FromV3 left OrgScoped=false for shared model without organization_id column")
	}

	broken := def
	broken.OrgScoped = false
	legacy, err := dynamic.BuildStructType(broken)
	if err != nil {
		t.Fatalf("BuildStructType: %v", err)
	}
	legacyHasOrg := false
	for i := 0; i < legacy.NumField(); i++ {
		if legacy.Field(i).Tag.Get("json") == "organization_id" {
			legacyHasOrg = true
		}
	}
	if legacyHasOrg {
		t.Fatal("zero-value BuildStructType should omit organization_id when OrgScoped=false")
	}

	fixed, err := dynamic.BuildStructTypeWithOptions(broken, dynamic.OpsCompatStructOptions())
	if err != nil {
		t.Fatalf("BuildStructTypeWithOptions: %v", err)
	}
	fixedHasOrg := false
	for i := 0; i < fixed.NumField(); i++ {
		if fixed.Field(i).Tag.Get("json") == "organization_id" {
			fixedHasOrg = true
		}
	}
	if !fixedHasOrg {
		t.Fatal("OpsCompatStructOptions did not project OrganizationID")
	}
}

func TestValidate_SharedRequiresOrganizationID_PublishGate(t *testing.T) {
	raw := []byte(`{
		"apiVersion":"asteby.com/v3","kind":"Addon",
		"metadata":{"key":"pos","name":"POS","version":"0.0.1"},
		"compatibility":{"requires":[{"key":"kernel","version":">=3.0.0 <4.0.0"}]},
		"tenancy":{"isolation":"shared","rls_column":"organization_id"},
		"models":[{"key":"Pay","table":"pays","columns":[{"name":"id","type":"uuid","primary_key":true},{"name":"amount","type":"numeric"}]}]
	}`)
	err := v3.Validate(raw)
	if err == nil {
		t.Fatal("Validate accepted shared model without organization_id")
	}
}
