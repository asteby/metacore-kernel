package modelbase

import (
	"bytes"
	"encoding/json"

	"github.com/asteby/metacore-kernel/validate"
)

// UnmarshalJSON accepts both the structured object {regex,min,max,custom}
// and a Laravel / go-playground rule string ("required|min:2" or
// "required,min=2,max=100" or a custom slug / $org.* token). The kernel
// never emits prose; the SDK localizes the resulting codes.
func (v *ValidationRule) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		spec := validate.ParseRuleString(s)
		v.Regex = spec.Regex
		v.Min = spec.Min
		v.Max = spec.Max
		v.Custom = spec.Custom
		return nil
	}
	type alias ValidationRule
	return json.Unmarshal(b, (*alias)(v))
}
