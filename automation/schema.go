package automation

import (
	"bytes"
	_ "embed"
	"fmt"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

//go:embed schema/automation.v1.json
var schemaBytes []byte

var compiledSchema *jsonschema.Schema

func init() {
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaBytes))
	if err != nil {
		panic(fmt.Errorf("automation: parse embedded schema: %w", err))
	}
	if err := c.AddResource("automation.v1.json", doc); err != nil {
		panic(fmt.Errorf("automation: add schema: %w", err))
	}
	s, err := c.Compile("automation.v1.json")
	if err != nil {
		panic(fmt.Errorf("automation: compile schema: %w", err))
	}
	compiledSchema = s
}

// SchemaJSON returns a copy of the embedded JSON Schema (served to editors so
// the UI form is generated from the same contract the server enforces).
func SchemaJSON() []byte { return append([]byte(nil), schemaBytes...) }

const maxSpecBytes = 32 << 10

func validateSchema(raw []byte) error {
	if len(raw) == 0 {
		return invalid("empty spec")
	}
	if len(raw) > maxSpecBytes {
		return invalid("the spec exceeds %d bytes", maxSpecBytes)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return invalid("invalid JSON: %v", err)
	}
	if err := compiledSchema.Validate(inst); err != nil {
		return invalid("does not match schema %s: %v", SpecVersion, err)
	}
	return nil
}
