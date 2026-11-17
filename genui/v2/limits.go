package genuiv2

// Version is the contract version this package validates natively.
const Version = "genui/v2"

// VersionV1 is the legacy contract; ParseAny upgrades it.
const VersionV1 = "genui/v1"

// Limits are identical to GENUI_V2_LIMITS in the TS package. Per-field limits
// (children, tabs, steps, columns...) live as maxItems/maxLength in the schema.
// Lengths of text count Unicode code points; Bytes counts UTF-8 bytes.
const (
	MaxBytes              = 128 * 1024
	MaxJSONDepth          = 64
	MaxDepth              = 6
	MaxBlocks             = 200
	MaxChildren           = 24
	MaxTabs               = 8
	MaxSteps              = 8
	MaxOverlays           = 4
	MaxSources            = 8
	MaxBindings           = 200
	MaxActions            = 32
	MaxActionsPerBlock    = 6
	MaxInlineTableColumns = 12
	MaxInlineTableRows    = 200
	MaxSourceTableColumns = 24
	MaxChartSeries        = 6
	MaxChartPoints        = 500
	MaxFormFields         = 40
	MaxMarkdown           = 8000
	MaxText               = 2000
	MaxLabel              = 160
)

// ForbiddenKeys never appear in any object of a document, nor as a segment of
// a path/identifier.
var ForbiddenKeys = []string{"__proto__", "constructor", "prototype"}

func forbidden(s string) bool {
	return s == "__proto__" || s == "constructor" || s == "prototype"
}
