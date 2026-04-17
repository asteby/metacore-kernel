package dynamic

import (
	"github.com/asteby/metacore-kernel/modelbase"
	"gorm.io/gorm"
)

// TenantScoper injects multi-tenancy WHERE clauses and auto-populates the
// tenant key on create. The default OrganizationScoper scopes by
// organization_id from the authenticated user. Apps override for branch-level
// scoping, schema-per-tenant, or anything else.
type TenantScoper interface {
	ScopeQuery(db *gorm.DB, user modelbase.AuthUser) *gorm.DB
	InjectOnCreate(input map[string]any, user modelbase.AuthUser)
}

// OrganizationScoper is the default — every query gets
// WHERE organization_id = <user's org>.
type OrganizationScoper struct{}

func (OrganizationScoper) ScopeQuery(db *gorm.DB, user modelbase.AuthUser) *gorm.DB {
	return db.Where("organization_id = ?", user.GetOrganizationID())
}

func (OrganizationScoper) InjectOnCreate(input map[string]any, user modelbase.AuthUser) {
	input["organization_id"] = user.GetOrganizationID()
}
