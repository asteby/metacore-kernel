package v3

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Masterminds/semver/v3"
	"github.com/santhosh-tekuri/jsonschema/v5"
)

//go:embed schema/manifest-v3.schema.json
var schemaBytes []byte

var compiledSchema *jsonschema.Schema

func init() {
	c := jsonschema.NewCompiler()
	c.Draft = jsonschema.Draft2020
	if err := c.AddResource("manifest-v3.schema.json", strings.NewReader(string(schemaBytes))); err != nil {
		panic(fmt.Errorf("v3: embed schema add resource: %w", err))
	}
	s, err := c.Compile("manifest-v3.schema.json")
	if err != nil {
		panic(fmt.Errorf("v3: compile embedded schema: %w", err))
	}
	compiledSchema = s
}

// SchemaJSON returns the embedded JSON schema bytes. Useful for tools that
// want to surface the schema (CLI, docs site, IDE plugins).
func SchemaJSON() []byte {
	out := make([]byte, len(schemaBytes))
	copy(out, schemaBytes)
	return out
}

// Validate parses raw as a v3 manifest, runs it through the JSON schema and
// then enforces the cross-field invariants the schema cannot express:
//
//   - apiVersion must equal APIVersion
//   - every compatibility.requires[].version must be a valid semver range
//   - every lifecycle.upgrade[].from must be a valid semver range
//   - kind=Preset must not declare models or own lifecycle
//   - kind=Addon must not declare a preset block
//
// On failure the returned error wraps a list of all violations so authors
// get the full picture in a single round trip.
func Validate(raw []byte) error {
	if len(raw) == 0 {
		return errors.New("v3: manifest is empty")
	}

	// Schema check first; if the doc is structurally broken the rest is
	// pointless.
	var asAny interface{}
	if err := json.Unmarshal(raw, &asAny); err != nil {
		return fmt.Errorf("v3: invalid JSON: %w", err)
	}
	if err := compiledSchema.Validate(asAny); err != nil {
		return fmt.Errorf("v3: schema validation failed: %w", err)
	}

	// Decode into the typed shape for cross-field checks.
	var m Manifest
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return fmt.Errorf("v3: decode into typed manifest: %w", err)
	}

	var errs []string

	if m.APIVersion != APIVersion {
		errs = append(errs, fmt.Sprintf("apiVersion must be %q, got %q", APIVersion, m.APIVersion))
	}

	switch m.Kind {
	case KindAddon, KindPreset, KindTheme, KindConnectorPack:
		// known
	default:
		errs = append(errs, fmt.Sprintf("kind %q is not one of Addon|Preset|Theme|ConnectorPack", m.Kind))
	}

	for i, r := range m.Compatibility.Requires {
		if _, err := semver.NewConstraint(r.Version); err != nil {
			errs = append(errs, fmt.Sprintf("compatibility.requires[%d].version %q is not a valid semver range: %v", i, r.Version, err))
		}
		if r.Key == "" {
			errs = append(errs, fmt.Sprintf("compatibility.requires[%d].key is empty", i))
		}
	}

	if m.Lifecycle != nil {
		for i, step := range m.Lifecycle.Upgrade {
			if _, err := semver.NewConstraint(step.From); err != nil {
				errs = append(errs, fmt.Sprintf("lifecycle.upgrade[%d].from %q is not a valid semver range: %v", i, step.From, err))
			}
		}
	}

	switch m.Kind {
	case KindAddon:
		if m.Preset != nil {
			errs = append(errs, "kind=Addon must not declare a preset block")
		}
	case KindPreset:
		if m.Preset == nil {
			errs = append(errs, "kind=Preset requires a preset block")
		}
		if len(m.Models) > 0 {
			errs = append(errs, "kind=Preset must not declare models[]")
		}
		if m.Lifecycle != nil {
			errs = append(errs, "kind=Preset must not declare a lifecycle block")
		}
	case KindTheme:
		if m.Theme == nil {
			errs = append(errs, "kind=Theme requires a theme block")
		}
	case KindConnectorPack:
		if m.ConnectorPack == nil {
			errs = append(errs, "kind=ConnectorPack requires a connector_pack block")
		}
	}

	for mi, mod := range m.Models {
		for ri, rel := range mod.Relations {
			where := fmt.Sprintf("models[%d].relations[%d]", mi, ri)
			switch rel.Kind {
			case "one_to_many", "many_to_many":
				// known
			default:
				errs = append(errs, fmt.Sprintf("%s.kind %q is not one of one_to_many|many_to_many", where, rel.Kind))
			}
			if rel.Name == "" {
				errs = append(errs, fmt.Sprintf("%s.name is empty", where))
			}
			if rel.Through == "" {
				errs = append(errs, fmt.Sprintf("%s.through is empty", where))
			}
			if rel.ForeignKey == "" {
				errs = append(errs, fmt.Sprintf("%s.foreign_key is empty", where))
			}
		}
	}

	if m.Contributions != nil {
		for ai, a := range m.Contributions.Actions {
			for fi, f := range a.Fields {
				if f.Balance == nil {
					continue
				}
				where := fmt.Sprintf("contributions.actions[%d].fields[%d]", ai, fi)
				if len(f.ItemFields) == 0 {
					errs = append(errs, fmt.Sprintf("%s declares a balance rule but has no item_fields (balance only applies to a line-items array field)", where))
					continue
				}
				cols := map[string]struct{}{}
				for _, it := range f.ItemFields {
					cols[it.Key] = struct{}{}
				}
				if f.Balance.DebitColumn == "" || f.Balance.CreditColumn == "" {
					errs = append(errs, fmt.Sprintf("%s.balance requires both debit_column and credit_column", where))
				}
				if f.Balance.DebitColumn != "" {
					if _, ok := cols[f.Balance.DebitColumn]; !ok {
						errs = append(errs, fmt.Sprintf("%s.balance.debit_column %q is not one of the field's item_fields", where, f.Balance.DebitColumn))
					}
				}
				if f.Balance.CreditColumn != "" {
					if _, ok := cols[f.Balance.CreditColumn]; !ok {
						errs = append(errs, fmt.Sprintf("%s.balance.credit_column %q is not one of the field's item_fields", where, f.Balance.CreditColumn))
					}
				}
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("v3: manifest validation failed:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

// Parse runs Validate and, on success, returns the typed manifest.
func Parse(raw []byte) (*Manifest, error) {
	if err := Validate(raw); err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("v3: decode: %w", err)
	}
	return &m, nil
}
