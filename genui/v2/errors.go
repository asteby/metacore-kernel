package genuiv2

import "fmt"

// Stable error codes (contract: ADR 0001 of the SDK, section 2.6). The TS
// validator in @asteby/metacore-genui emits the same strings and the shared
// conformance vectors pin them.
const (
	CodeInvalidJSON        = "genui.invalid_json"
	CodeInvalidDocument    = "genui.invalid_document"
	CodeUnsupportedVersion = "genui.unsupported_version"
	CodeUnknownBlock       = "genui.unknown_block"
	CodeFieldNotAllowed    = "genui.field_not_allowed"
	CodeRequired           = "genui.required"
	CodeType               = "genui.type"
	CodeEnum               = "genui.enum"
	CodePattern            = "genui.pattern"
	CodeDuplicateID        = "genui.duplicate_id"
	CodeUnsafeLink         = "genui.unsafe_link"
	CodeExternalLink       = "genui.external_link_disabled"
	CodeBadRef             = "genui.bad_ref"
	CodeRefNotAllowed      = "genui.ref_not_allowed"
	CodeUnknownOverlay     = "genui.unknown_overlay"
	CodeUnknownTarget      = "genui.unknown_target"
	CodeInvalidAction      = "genui.invalid_action"
	CodeInvalidBlock       = "genui.invalid_block"
	CodeForbiddenKey       = "genui.forbidden_key"
	CodeLimitBytes         = "genui.limit.bytes"
	CodeLimitDepth         = "genui.limit.depth"
	CodeLimitBlocks        = "genui.limit.blocks"
	CodeLimitChildren      = "genui.limit.children"
	CodeLimitTabs          = "genui.limit.tabs"
	CodeLimitSteps         = "genui.limit.steps"
	CodeLimitOverlays      = "genui.limit.overlays"
	CodeLimitSources       = "genui.limit.sources"
	CodeLimitBindings      = "genui.limit.bindings"
	CodeLimitActions       = "genui.limit.actions"
	CodeLimitColumns       = "genui.limit.columns"
	CodeLimitRows          = "genui.limit.rows"
	CodeLimitSeries        = "genui.limit.series"
	CodeLimitFields        = "genui.limit.fields"
	CodeLimitItems         = "genui.limit.items"
	CodeLimitLength        = "genui.limit.length"
	CodeLimitProperties    = "genui.limit.properties"
	CodeLimitMarkdown      = "genui.limit.markdown"
)

// Error is a validation failure with a stable Code. Path is a JSON-ish
// location ("$.blocks[2].items") and may be empty.
type Error struct {
	Code string
	Msg  string
	Path string
}

func (e *Error) Error() string { return e.Code + ": " + e.Msg }

func fail(code, path, format string, a ...any) *Error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, a...), Path: path}
}

// CodeOf returns the stable code of err, or CodeInvalidDocument for a foreign error.
func CodeOf(err error) string {
	if e, ok := err.(*Error); ok {
		return e.Code
	}
	return CodeInvalidDocument
}

// AllCodes lists every stable error code (for caps, docs and coverage tests).
func AllCodes() []string {
	return []string{
		CodeInvalidJSON, CodeInvalidDocument, CodeUnsupportedVersion, CodeUnknownBlock, CodeFieldNotAllowed,
		CodeRequired, CodeType, CodeEnum, CodePattern, CodeDuplicateID, CodeUnsafeLink, CodeExternalLink,
		CodeBadRef, CodeRefNotAllowed, CodeUnknownOverlay, CodeUnknownTarget, CodeInvalidAction, CodeInvalidBlock,
		CodeForbiddenKey, CodeLimitBytes, CodeLimitDepth, CodeLimitBlocks, CodeLimitChildren, CodeLimitTabs,
		CodeLimitSteps, CodeLimitOverlays, CodeLimitSources, CodeLimitBindings, CodeLimitActions, CodeLimitColumns,
		CodeLimitRows, CodeLimitSeries, CodeLimitFields, CodeLimitItems, CodeLimitLength, CodeLimitProperties,
		CodeLimitMarkdown,
	}
}
