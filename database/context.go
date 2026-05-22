// Package database provides helpers to wire common middleware concerns
// (audit context, tenant scoping, request metadata) onto a gorm.DB session
// derived from a Fiber request.
//
// Addons import this so they don't duplicate the same boilerplate per
// package (inventory, customers, billing-sat, accounting-lite, purchases
// all used to ship their own internal copy).
package database

import (
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Context keys used to stash audit metadata on a gorm.DB session. They mirror
// the values produced by the auth middleware and are read by the activity_log
// service as well as modelbase.BaseUUIDModel.BeforeCreate (which expects
// "activity_log:user_id" to be a *uuid.UUID).
const (
	ContextKeyUserID    = "activity_log:user_id"
	ContextKeyUserName  = "activity_log:user_name"
	ContextKeyUserEmail = "activity_log:user_email"
	ContextKeyIPAddress = "activity_log:ip_address"
	ContextKeyUserAgent = "activity_log:user_agent"
)

// WithFiberContext returns a fresh GORM session enriched with audit context
// pulled from the Fiber request locals (user_id, user_name, user_email) and
// request metadata (ip, user_agent).
//
// All write paths (Create/Update/Delete, including those wrapped in
// db.Transaction) MUST go through a session built by this helper so that:
//
//   - The BeforeCreate hook on modelbase.BaseUUIDModel can populate
//     created_by_id transparently.
//   - The activity_log service records who/where/when for every change.
//
// Using this helper is the single source of truth — handlers and route
// registrations must never call db.Set("activity_log:...") manually.
func WithFiberContext(c fiber.Ctx, db *gorm.DB) *gorm.DB {
	session := db.Session(&gorm.Session{NewDB: true})

	if v := c.Locals("user_id"); v != nil {
		if uid, ok := v.(uuid.UUID); ok {
			session = session.Set(ContextKeyUserID, &uid)
		}
	}
	if v := c.Locals("user_name"); v != nil {
		if name, ok := v.(string); ok {
			session = session.Set(ContextKeyUserName, name)
		}
	}
	if v := c.Locals("user_email"); v != nil {
		if email, ok := v.(string); ok {
			session = session.Set(ContextKeyUserEmail, email)
		}
	}

	session = session.Set(ContextKeyIPAddress, c.IP())
	session = session.Set(ContextKeyUserAgent, c.Get("User-Agent"))

	return session
}
