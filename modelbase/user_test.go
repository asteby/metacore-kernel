package modelbase_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/asteby/metacore-kernel/modelbase"
)

func TestBaseUserPasswordRoundTrip(t *testing.T) {
	u := &modelbase.BaseUser{}
	if err := u.SetPassword("s3cret-pw"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	if u.PasswordHash == "" {
		t.Fatal("PasswordHash should be populated after SetPassword")
	}
	if u.PasswordHash == "s3cret-pw" {
		t.Fatal("PasswordHash should not equal plaintext")
	}
	if !u.CheckPassword("s3cret-pw") {
		t.Fatal("CheckPassword should accept the original plaintext")
	}
	if u.CheckPassword("wrong-pw") {
		t.Fatal("CheckPassword should reject wrong plaintext")
	}
}

func TestBaseUserEmptyPassword(t *testing.T) {
	u := &modelbase.BaseUser{}
	if err := u.SetPassword(""); err == nil {
		t.Fatal("SetPassword('') should return an error")
	}
	if u.CheckPassword("anything") {
		t.Fatal("CheckPassword on unset hash must return false")
	}
}

func TestBaseUserSatisfiesAuthUser(t *testing.T) {
	orgID := uuid.New()
	userID := uuid.New()
	u := &modelbase.BaseUser{
		BaseUUIDModel: modelbase.BaseUUIDModel{
			ID:             userID,
			OrganizationID: orgID,
		},
		Email: "ada@example.com",
		Role:  modelbase.RoleAdmin,
	}

	var au modelbase.AuthUser = u // compile-time + runtime check

	if au.GetID() != userID {
		t.Fatalf("GetID: got %v want %v", au.GetID(), userID)
	}
	if au.GetOrganizationID() != orgID {
		t.Fatalf("GetOrganizationID: got %v want %v", au.GetOrganizationID(), orgID)
	}
	if au.GetEmail() != "ada@example.com" {
		t.Fatalf("GetEmail: got %q", au.GetEmail())
	}
	if au.GetRole() != modelbase.RoleAdmin {
		t.Fatalf("GetRole: got %q want %q", au.GetRole(), modelbase.RoleAdmin)
	}
}

func TestBaseUserTableName(t *testing.T) {
	u := &modelbase.BaseUser{}
	if got := u.TableName(); got != "users" {
		t.Fatalf("TableName: got %q want %q", got, "users")
	}
}

func TestBaseOrganizationTableName(t *testing.T) {
	o := &modelbase.BaseOrganization{}
	if got := o.TableName(); got != "organizations" {
		t.Fatalf("TableName: got %q want %q", got, "organizations")
	}
}
