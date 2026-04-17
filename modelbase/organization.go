package modelbase

import "github.com/google/uuid"

// BaseOrganization is the canonical tenant shape. Apps extend it by embedding
// and adding app-specific fields (billing, fiscal data, onboarding flags, …).
//
// The kernel keeps this struct small on purpose: everything here is universal
// across SaaS tenants. Billing/gateway/tax concerns belong in the consuming
// app so the kernel stays domain-agnostic.
type BaseOrganization struct {
	BaseUUIDModel
	Name     string `json:"name" gorm:"size:255;not null"`
	Slug     string `json:"slug" gorm:"uniqueIndex;size:100"`
	Country  string `json:"country,omitempty" gorm:"size:2"`
	Currency string `json:"currency,omitempty" gorm:"size:3"`
	Timezone string `json:"timezone,omitempty" gorm:"size:100"`
	Logo     string `json:"logo,omitempty" gorm:"size:500"`
}

// TableName pins the organisations table name regardless of GORM's pluralisation rules.
func (o *BaseOrganization) TableName() string { return "organizations" }

// GetID satisfies AuthOrg.
func (o *BaseOrganization) GetID() uuid.UUID { return o.ID }

// GetName satisfies AuthOrg.
func (o *BaseOrganization) GetName() string { return o.Name }

// SetName satisfies AuthOrg.
func (o *BaseOrganization) SetName(v string) { o.Name = v }

// Compile-time check that *BaseOrganization satisfies AuthOrg.
var _ AuthOrg = (*BaseOrganization)(nil)
