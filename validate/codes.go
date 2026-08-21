// Package validate is the locale-agnostic field validator the kernel and
// hosts share — Laravel-style rule names, structured {code, params} issues,
// never prose. The SDK (and any host i18n bundle) turns codes into the
// operator's language.
package validate

// Stable issue codes. The SDK localizes `validation.<code>` with the field
// label and the params below. Adding a code is an additive contract change;
// renaming one is breaking.
const (
	CodeRequired          = "required"
	CodeInvalidOption     = "invalid_option"
	CodeNotFound          = "not_found"
	CodeDuplicate         = "duplicate"
	CodeInvalidType       = "invalid_type"
	CodeMin               = "min"
	CodeMax               = "max"
	CodeRegex             = "regex"
	CodeEmail             = "email"
	CodeUUID              = "uuid"
	CodeURL               = "url"
	CodeNumeric           = "numeric"
	CodeInteger           = "integer"
	CodeCustom            = "custom"
	CodeLineItemsRequired = "line_items_required"
)

// KindValue / KindLength discriminate min/max: a numeric column bounds the
// value, a string/array column bounds the length. The SDK picks
// validation.min vs validation.min_length from this param.
const (
	KindValue  = "value"
	KindLength = "length"
)
