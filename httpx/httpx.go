// Package httpx holds transport-agnostic helpers that any multi-tenant HTTP
// app needs on day one: extracting the caller's organization and user IDs
// from request-scoped storage, and reading nested fields from arbitrary
// structs via reflection (useful for generic search/options endpoints).
//
// The package does not depend on any HTTP framework. Apps adapt their
// framework-specific context (fiber.Ctx, echo.Context, *http.Request with
// context.Context, ...) to the minimal ContextLookup interface below.
//
// Errors are returned as sentinel values so callers can translate them to
// the HTTP status/shape their app prefers (401 JSON body, problem+json,
// gRPC codes, ...).
package httpx

import (
	"errors"
	"reflect"
	"strings"

	"github.com/google/uuid"
)

// ContextLookup is the minimum surface httpx needs from a request context
// to extract tenant/user identity set by upstream middleware. Apps wrap
// their native context (e.g. *fiber.Ctx) with a tiny adapter that
// implements this interface.
type ContextLookup interface {
	// Locals returns a request-scoped value previously stored under key,
	// or nil if absent. Matches Fiber's Ctx.Locals and is easy to
	// implement for net/http (context.Value), echo (Get), gin (Get), etc.
	Locals(key string) any
}

// Sentinel errors. Callers compare with errors.Is.
var (
	// ErrOrgMissing means no "organization_id" was present in the context.
	// Typical remediation: return 401 from the handler.
	ErrOrgMissing = errors.New("httpx: organization context not found")
	// ErrOrgInvalid means "organization_id" was present but not a uuid.UUID.
	// This usually indicates a misconfigured middleware.
	ErrOrgInvalid = errors.New("httpx: invalid organization context")
	// ErrUserMissing means no "user_id" was present in the context.
	ErrUserMissing = errors.New("httpx: user context not found")
	// ErrUserInvalid means "user_id" was present but not a uuid.UUID.
	ErrUserInvalid = errors.New("httpx: invalid user context")
)

// Context keys httpx reads from ContextLookup. Exported so apps that set
// locals from middleware can reference the exact keys kernel expects.
const (
	LocalOrganizationID = "organization_id"
	LocalUserID         = "user_id"
)

// ExtractOrgID returns the uuid.UUID stored at LocalOrganizationID. It
// returns ErrOrgMissing if the key is absent and ErrOrgInvalid if the
// stored value is not a uuid.UUID.
func ExtractOrgID(ctx ContextLookup) (uuid.UUID, error) {
	return extractUUID(ctx, LocalOrganizationID, ErrOrgMissing, ErrOrgInvalid)
}

// ExtractUserID returns the uuid.UUID stored at LocalUserID. It returns
// ErrUserMissing if the key is absent and ErrUserInvalid if the stored
// value is not a uuid.UUID.
func ExtractUserID(ctx ContextLookup) (uuid.UUID, error) {
	return extractUUID(ctx, LocalUserID, ErrUserMissing, ErrUserInvalid)
}

func extractUUID(ctx ContextLookup, key string, missing, invalid error) (uuid.UUID, error) {
	val := ctx.Locals(key)
	if val == nil {
		return uuid.UUID{}, missing
	}
	id, ok := val.(uuid.UUID)
	if !ok {
		return uuid.UUID{}, invalid
	}
	return id, nil
}

// GetFieldValue resolves a dotted fieldPath on item (a reflect.Value
// wrapping a struct or pointer-to-struct) and returns the underlying
// interface value, or nil if any segment fails to resolve.
//
// Resolution order per segment:
//  1. Exact Go field name (honoring promoted fields).
//  2. Case-insensitive Go field name (honoring promoted fields).
//  3. `json` tag name (direct fields only).
//
// Nil pointers anywhere along the path yield nil.
func GetFieldValue(item reflect.Value, fieldPath string) any {
	if fieldPath == "" {
		return nil
	}

	parts := strings.Split(fieldPath, ".")
	current := item

	for _, part := range parts {
		if current.Kind() == reflect.Ptr {
			if current.IsNil() {
				return nil
			}
			current = current.Elem()
		}

		if current.Kind() != reflect.Struct {
			return nil
		}

		// 1. Exact match (supports promoted fields).
		if f := current.FieldByName(part); f.IsValid() {
			current = f
			continue
		}

		// 2. Case-insensitive match (supports promoted fields).
		matchFunc := func(name string) bool {
			return strings.EqualFold(name, part)
		}
		if structField, ok := current.Type().FieldByNameFunc(matchFunc); ok {
			current = current.FieldByIndex(structField.Index)
			continue
		}

		// 3. JSON tag on direct fields.
		found := false
		for i := 0; i < current.NumField(); i++ {
			field := current.Type().Field(i)
			jsonTag := field.Tag.Get("json")
			if jsonTag == "" {
				continue
			}
			jsonName := strings.Split(jsonTag, ",")[0]
			if jsonName == part {
				current = current.Field(i)
				found = true
				break
			}
		}
		if !found {
			return nil
		}
	}

	if current.Kind() == reflect.Ptr {
		if current.IsNil() {
			return nil
		}
		current = current.Elem()
	}
	return current.Interface()
}
