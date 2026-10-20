package v3

import (
	"fmt"

	"github.com/asteby/metacore-kernel/manifest/inputrules"
)

// validateInputRules checks every `input_rules` block of the manifest:
// settings[], connectors[].credentials[], edge_devices[].pairing_credentials[]
// and the declarative fields of contributions.actions (flat, wizard steps and
// line-item columns). Patterns must be safe and portable (RE2 ∩ ECMAScript,
// no nested quantifiers), lengths coherent, enums known and examples must pass
// their own rules. See manifest/inputrules.Validate.
func validateInputRules(m *Manifest) []string {
	var errs []string
	add := func(path string, r *InputRules) {
		for _, e := range inputrules.Validate(r) {
			errs = append(errs, fmt.Sprintf("%s.input_rules.%s", path, e))
		}
	}
	for _, s := range m.Settings {
		add(fmt.Sprintf("settings[%s]", s.Key), s.InputRules)
	}
	for _, c := range m.Connectors {
		for _, cr := range c.Credentials {
			add(fmt.Sprintf("connectors[%s].credentials[%s]", c.Key, cr.Key), cr.InputRules)
		}
	}
	for _, d := range m.EdgeDevices {
		for _, cr := range d.PairingCredentials {
			add(fmt.Sprintf("edge_devices[%s].pairing_credentials[%s]", d.Key, cr.Key), cr.InputRules)
		}
	}
	var walk func(prefix string, fields []ActionField)
	walk = func(prefix string, fields []ActionField) {
		for _, f := range fields {
			p := fmt.Sprintf("%s.fields[%s]", prefix, f.Key)
			add(p, f.InputRules)
			if len(f.ItemFields) > 0 {
				walk(p+".item_fields", f.ItemFields)
			}
		}
	}
	if m.Contributions != nil {
		for _, a := range m.Contributions.Actions {
			p := fmt.Sprintf("contributions.actions[%s]", a.Key)
			walk(p, a.Fields)
			for si, st := range a.Steps {
				walk(fmt.Sprintf("%s.steps[%d]", p, si), st.Fields)
			}
		}
	}
	return errs
}
