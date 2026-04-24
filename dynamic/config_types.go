package dynamic

import "github.com/asteby/metacore-kernel/modelbase"

// Config types live in modelbase alongside TableMetadata / ModalMetadata so
// apps alias them into their own `models` package. Re-exported here as
// type aliases so dynamic service/handler code can refer to them without
// requiring every caller to also import modelbase.

// SearchConfig is a re-export of modelbase.SearchConfig.
type SearchConfig = modelbase.SearchConfig

// OptionsConfig is a re-export of modelbase.OptionsConfig.
type OptionsConfig = modelbase.OptionsConfig

// FieldOptionsConfig is a re-export of modelbase.FieldOptionsConfig.
type FieldOptionsConfig = modelbase.FieldOptionsConfig

// StaticOption is a re-export of modelbase.StaticOption.
type StaticOption = modelbase.StaticOption

// Option is the runtime projection returned by Options and Search — purely
// a response/DTO shape so it stays in the dynamic package. The dual
// id/value and label/name fields are preserved for legacy frontend parity.
type Option struct {
	ID          any `json:"id"`
	Value       any `json:"value"`
	Label       any `json:"label"`
	Name        any `json:"name"`
	Description any `json:"description,omitempty"`
	Image       any `json:"image,omitempty"`
	Color       any `json:"color,omitempty"`
	Icon        any `json:"icon,omitempty"`
}
