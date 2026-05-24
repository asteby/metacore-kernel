// Package database — currency hook.
//
// This file ships a global GORM Before-Create callback that populates a
// model's `CurrencyCode` (or `Moneda`) string field at INSERT time from
// the per-organization currency resolved through `config.OrgCurrencyGetter`.
//
// Motivation
//
// Addons used to carry `gorm:"default:'MXN'"` struct tags on every
// currency_code column. That hardcoded the Mexican peso into the
// platform (violating the "no locale-specific defaults in addons" rule:
// see metacore-kernel/config/currency.go). The tag was kept as an
// "ultimate DB fallback" while a proper INSERT-time resolver was
// designed. This is that resolver.
//
// Once apps register this callback on their root *gorm.DB, addons can
// drop the GORM tag entirely: empty CurrencyCode → callback resolves it
// from the request context's `config.OrgCurrencyGetter` keyed on the
// model's `OrganizationID` (matching modelbase.BaseUUIDModel.OrganizationID),
// with `fallback` used when no getter is wired or the org has no
// currency configured. The fallback should be a geography-agnostic
// ISO-4217 code (USD), NEVER a locale-specific one like MXN.
//
// Scope
//
//   - Detection by reflection of a string field named "CurrencyCode" or
//     "Moneda" (the latter survives a CFDI carta-porte model that uses
//     SAT-specific Spanish naming).
//   - Org-ID resolution by reflection of a uuid.UUID field named
//     "OrganizationID" (matches modelbase.BaseUUIDModel).
//   - Only fills when the field is empty — explicit values from API
//     payloads always win.
//   - Pulls the getter from the gorm.DB statement context, so request
//     context plumbing (config.WithCurrencyGetter) keeps working
//     transparently.
package database

import (
	"context"
	"reflect"
	"sync"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/asteby/metacore-kernel/config"
)

// CurrencyHookFallback is the ISO-4217 code used when no getter is
// wired on the request context or the org has no currency configured.
// USD is intentional: it is the only globally-recognised code that does
// NOT bias the platform toward a specific country. Apps that need a
// different fallback pass it explicitly to RegisterCurrencyDefaultCallback.
const CurrencyHookFallback = "USD"

// currencyFieldNames lists the struct field names that the hook treats
// as currency code holders. "CurrencyCode" covers the canonical naming
// used across customers / purchases / accounting-lite / inventory /
// hr-lite. "Moneda" survives the waybill-cartaporte CFDI model whose
// schema is dictated by SAT and uses Spanish naming.
//
// The list is deliberately closed: extending it requires a kernel
// release, which forces a conscious decision rather than letting addons
// invent their own currency field names.
var currencyFieldNames = []string{"CurrencyCode", "Moneda"}

// fieldIndexCacheKey identifies a (modelType, fieldName) pair for the
// reflect-index cache. Reflection on the same model on every INSERT is
// hot-path work; caching the field index keeps the hook close to a
// single map lookup per row.
type fieldIndexCacheKey struct {
	t    reflect.Type
	name string
}

type currencyFieldMeta struct {
	currencyIdx []int // index path to the currency string field (nil = none)
	orgIdx      []int // index path to OrganizationID uuid.UUID field (nil = none)
}

var (
	currencyFieldCache   sync.Map // map[reflect.Type]currencyFieldMeta
	_                    = fieldIndexCacheKey{} // reserved for future per-name caching
)

// RegisterCurrencyDefaultCallback wires the BeforeCreate currency hook
// onto db. Safe to call multiple times on the same *gorm.DB: GORM's
// callback registry treats Register(name) as upsert.
//
// fallback is used when (a) no `config.OrgCurrencyGetter` is wired on
// the request context, or (b) the getter returns ok=false for the
// org. Pass "USD" unless your app has a documented reason to bias
// otherwise.
//
// Usage in an app's bootstrap:
//
//	db := gorm.Open(...)
//	database.RegisterCurrencyDefaultCallback(db, database.CurrencyHookFallback)
//
// After this call, addons no longer need `gorm:"default:'MXN'"` tags;
// empty `CurrencyCode` strings on inserts will be populated by the hook.
func RegisterCurrencyDefaultCallback(db *gorm.DB, fallback string) error {
	if fallback == "" {
		fallback = CurrencyHookFallback
	}
	return db.Callback().Create().Before("gorm:create").Register(
		"metacore:currency_default",
		func(tx *gorm.DB) {
			applyCurrencyDefault(tx, fallback)
		},
	)
}

// applyCurrencyDefault is the callback body. Extracted so tests can
// invoke it directly without going through gorm.Open + AutoMigrate.
func applyCurrencyDefault(tx *gorm.DB, fallback string) {
	if tx.Statement == nil || tx.Statement.ReflectValue.Kind() == reflect.Invalid {
		return
	}

	rv := tx.Statement.ReflectValue
	// GORM batch inserts pass a slice; iterate each element.
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		for i := 0; i < rv.Len(); i++ {
			fillOne(tx, rv.Index(i), fallback)
		}
	default:
		fillOne(tx, rv, fallback)
	}
}

// fillOne resolves and writes the currency code for a single struct
// value. It handles pointer indirection and is a no-op for kinds it
// cannot reason about.
func fillOne(tx *gorm.DB, v reflect.Value, fallback string) {
	for v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return
	}

	meta := lookupCurrencyMeta(v.Type())
	if meta.currencyIdx == nil {
		return
	}

	curField := v.FieldByIndex(meta.currencyIdx)
	if !curField.CanSet() || curField.Kind() != reflect.String {
		return
	}
	if curField.String() != "" {
		// Explicit value supplied by caller — never overwrite.
		return
	}

	orgID := uuid.Nil
	if meta.orgIdx != nil {
		of := v.FieldByIndex(meta.orgIdx)
		if of.IsValid() && of.Type() == reflect.TypeOf(uuid.UUID{}) {
			orgID = of.Interface().(uuid.UUID)
		}
	}

	ctx := tx.Statement.Context
	if ctx == nil {
		ctx = context.Background()
	}
	getter := config.CurrencyGetterFromContext(ctx)
	curField.SetString(config.GetCurrencyOrDefault(ctx, getter, orgID, fallback))
}

// lookupCurrencyMeta returns a cached (currencyIdx, orgIdx) pair for t,
// computing it on the first call. Returns zero-value meta when the
// struct carries no recognised currency field; the receiver is expected
// to bail out in that case.
func lookupCurrencyMeta(t reflect.Type) currencyFieldMeta {
	if cached, ok := currencyFieldCache.Load(t); ok {
		return cached.(currencyFieldMeta)
	}

	var meta currencyFieldMeta
	for _, name := range currencyFieldNames {
		if f, ok := t.FieldByName(name); ok && f.Type.Kind() == reflect.String {
			meta.currencyIdx = f.Index
			break
		}
	}
	if of, ok := t.FieldByName("OrganizationID"); ok && of.Type == reflect.TypeOf(uuid.UUID{}) {
		meta.orgIdx = of.Index
	}

	currencyFieldCache.Store(t, meta)
	return meta
}
