package apitoken

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Kind is the token kind; it fixes the plaintext prefix.
type Kind string

const (
	// KindPublishable — "pk_", browser-safe, restricted scopes.
	KindPublishable Kind = "publishable"
	// KindSecret — "sk_", server to server.
	KindSecret Kind = "secret"
	// KindPlatform — "pt_", not org-scoped (multi-tenant renderer).
	KindPlatform Kind = "platform"
)

// Prefix returns the plaintext prefix of the kind ("pk_", "sk_", "pt_"), or
// "" for an unknown kind.
func (k Kind) Prefix() string {
	switch k {
	case KindPublishable:
		return "pk_"
	case KindSecret:
		return "sk_"
	case KindPlatform:
		return "pt_"
	}
	return ""
}

// Valid reports whether k is a known kind.
func (k Kind) Valid() bool { return k.Prefix() != "" }

// Status is the token lifecycle state.
type Status string

const (
	// StatusActive — the token authenticates (until ExpiresAt).
	StatusActive Status = "active"
	// StatusRevoked — the token never authenticates again.
	StatusRevoked Status = "revoked"
)

// TableName is the table APIToken persists to.
const TableName = "metacore_api_tokens"

// APIToken is one issued token. OrganizationID is nil only for KindPlatform.
// The plaintext is never stored: TokenHash is its SHA-256 (hex) and Prefix
// its first PrefixLen characters, for display.
type APIToken struct {
	ID                 uuid.UUID  `gorm:"type:uuid;primaryKey" json:"id"`
	OrganizationID     *uuid.UUID `gorm:"type:uuid;index:idx_mc_api_tokens_org" json:"organization_id,omitempty"`
	Name               string     `gorm:"size:120;not null" json:"name"`
	Kind               Kind       `gorm:"size:20;not null" json:"kind"`
	Prefix             string     `gorm:"size:16;not null" json:"prefix"`
	TokenHash          string     `gorm:"size:64;not null;uniqueIndex:idx_mc_api_tokens_hash" json:"-"`
	Scopes             []string   `gorm:"serializer:json;type:jsonb" json:"scopes"`
	Status             Status     `gorm:"size:20;not null;default:active" json:"status"`
	ExpiresAt          *time.Time `json:"expires_at,omitempty"`
	RotatedFromID      *uuid.UUID `gorm:"type:uuid" json:"rotated_from_id,omitempty"`
	LastUsedAt         *time.Time `json:"last_used_at,omitempty"`
	RateLimitPerMinute int        `gorm:"not null;default:0" json:"rate_limit_per_minute"`
	CreatedBy          *uuid.UUID `gorm:"type:uuid" json:"created_by,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

// TableName pins the table.
func (APIToken) TableName() string { return TableName }

// Org returns the organization, uuid.Nil for a platform token.
func (t *APIToken) Org() uuid.UUID {
	if t == nil || t.OrganizationID == nil {
		return uuid.Nil
	}
	return *t.OrganizationID
}

// Usable reports whether the token authenticates at now (active and not
// expired), and if not, the reason (ErrRevoked or ErrExpired).
func (t *APIToken) Usable(now time.Time) error {
	if t.Status != StatusActive {
		return ErrRevoked
	}
	if t.ExpiresAt != nil && !now.Before(*t.ExpiresAt) {
		return ErrExpired
	}
	return nil
}

// Migrate creates or updates the token table. The DDL is portable (Postgres in
// production, SQLite in tests).
func Migrate(ctx context.Context, db *gorm.DB) error {
	if err := db.WithContext(ctx).AutoMigrate(&APIToken{}); err != nil {
		return fmt.Errorf("apitoken: migrate: %w", err)
	}
	return nil
}
