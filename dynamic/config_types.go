package dynamic

import (
	"bytes"
	"encoding/json"
	"sort"

	"github.com/asteby/metacore-kernel/modelbase"
)

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

	// Extra holds the additional scalar columns requested through
	// FieldOptionsConfig.ExtraColumns. They are serialized as SIBLING keys of
	// id/value/label (see MarshalJSON), never under "extra", because that is
	// where the SDK's option_filter reads them. Nil for ordinary options.
	Extra map[string]any `json:"-"`
}

// optionAlias drops Option's methods so MarshalJSON can reuse the default
// encoding of the declared fields without recursing.
type optionAlias Option

// MarshalJSON encodes the declared fields as before and appends Extra as
// sibling keys (sorted, so the output is deterministic). An extra column can
// never overwrite a declared key.
func (o Option) MarshalJSON() ([]byte, error) {
	base, err := json.Marshal(optionAlias(o))
	if err != nil || len(o.Extra) == 0 {
		return base, err
	}
	keys := make([]string, 0, len(o.Extra))
	for k := range o.Extra {
		if reservedOptionKeys[k] {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	buf := bytes.NewBuffer(base[:len(base)-1]) // strip the closing '}'
	for _, k := range keys {
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		vb, err := json.Marshal(o.Extra[k])
		if err != nil {
			return nil, err
		}
		buf.WriteByte(',')
		buf.Write(kb)
		buf.WriteByte(':')
		buf.Write(vb)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// reservedOptionKeys are the keys Option already serializes; an extra column
// with one of these names is ignored.
var reservedOptionKeys = map[string]bool{
	"id": true, "value": true, "label": true, "name": true, "description": true,
	"image": true, "color": true, "icon": true,
}
