// Package modelbase provides the foundational, framework-agnostic type system
// that all metacore kernel modules (auth, dynamic, metadata, permission) and
// downstream applications share.
//
// # Design
//
// modelbase is deliberately minimal. It provides:
//
//   - BaseUUIDModel: common UUID/timestamp/audit fields used by every tenant-
//     scoped record in the platform.
//   - BaseUser / BaseOrganization: canonical tenant + principal types.
//   - AuthUser: stable cross-version contract consumed by auth and permission
//     modules — embedders automatically satisfy it.
//   - TableMetadata / ModalMetadata / ColumnDef / FieldDef / ActionDef /
//     FilterDef / OptionDef: public JSON-serialisable shapes consumed by the
//     frontend DynamicTable / DynamicModal components.
//   - A thread-safe model registry (Register / Get / All) keyed by table name.
//
// # Extension pattern
//
// Apps extend the base types via Go embedding (composition), never by editing
// the shapes here. Example:
//
//	type User struct {
//	    modelbase.BaseUser
//	    BranchID *uuid.UUID `json:"branch_id,omitempty" gorm:"type:uuid;index"`
//	}
//
// # Stability
//
// The AuthUser interface is the stable cross-version contract. Any breaking
// change to a public struct shape (field added/removed/retyped, tag changed)
// REQUIRES a major version bump of the kernel under semver, because such
// changes affect both the DB schema (via GORM AutoMigrate) and the JSON shape
// consumed by clients.
//
// modelbase MUST NOT import any web framework (Fiber, Echo, net/http handlers),
// nor may it import github.com/asteby/metacore-sdk, to keep it usable as the
// lowest dependency in the kernel graph.
package modelbase
