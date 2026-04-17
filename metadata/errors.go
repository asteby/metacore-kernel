package metadata

import "errors"

// Sentinel errors returned by the metadata package. Apps should use
// errors.Is to branch on these rather than matching on message text.
var (
	// ErrModelNotFound is returned when a model key is not registered in
	// modelbase's global registry.
	ErrModelNotFound = errors.New("metadata: model not found")

	// ErrMetadataInvalid is returned when a registered model produces an
	// unusable TableMetadata or ModalMetadata (e.g. empty columns/fields).
	ErrMetadataInvalid = errors.New("metadata: metadata invalid")
)

// Message constants (exported) so apps can tweak wire text without forking
// the handler. They are intentionally plain English; override via i18n at
// the handler layer if needed.
const (
	MsgModelNotFound    = "Model not found"
	MsgMetadataInvalid  = "Metadata invalid"
	MsgInternalError    = "Internal server error"
	MsgCacheInvalidated = "Metadata cache invalidated"
)

// paramModel is the Fiber route parameter name used by Handler.Mount. Kept
// as a constant so the handler and its tests never drift.
const paramModel = "model"

// cacheKeyAll is the reserved cache key used by GetAll. Model keys cannot
// collide with it because modelbase.Register rejects empty keys and this
// sentinel is namespaced with double underscores.
const cacheKeyAll = "__all__"
