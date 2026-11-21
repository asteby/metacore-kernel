package dynamic

import (
	"fmt"
	"strings"

	"github.com/asteby/metacore-kernel/manifest"
)

// StageCheckName is the name of the CHECK constraint derived from a model's
// stages (models[].stage_check).
func StageCheckName(table string) string { return "ck_" + table + "_stage" }

// stageCheckStatements renders the opt-in CHECK derived from the model's stages:
//
//	CHECK ("stage_field" IN ('draft','posted'))   -- NULL still passes
//
// It is created NOT VALID: Postgres enforces it on every INSERT/UPDATE from now
// on but does not scan existing rows, so a table that holds values from before
// the machine existed installs cleanly. Run StageCheckValidateStatement once the
// data is clean to validate the old rows too. Re-running the statements is safe
// (the constraint is dropped and recreated, so a changed stage list takes
// effect). Returns nil when the model did not opt in or has no machine.
func stageCheckStatements(schema string, def manifest.ModelDefinition) []string {
	if !def.StageCheck || def.StageField == "" || len(def.Stages) == 0 {
		return nil
	}
	vals := make([]string, 0, len(def.Stages))
	for _, st := range def.Stages {
		vals = append(vals, "'"+strings.ReplaceAll(st.Key, "'", "''")+"'")
	}
	name := StageCheckName(def.TableName)
	return []string{
		fmt.Sprintf(`ALTER TABLE %q.%q DROP CONSTRAINT IF EXISTS %q`, schema, def.TableName, name),
		fmt.Sprintf(`ALTER TABLE %q.%q ADD CONSTRAINT %q CHECK (%q IN (%s)) NOT VALID`,
			schema, def.TableName, name, def.StageField, strings.Join(vals, ",")),
	}
}

// StageCheckValidateStatement validates the derived CHECK against the rows that
// predate it. It fails (and changes nothing) while any row holds a value outside
// the stages: fix the data, then run it again.
func StageCheckValidateStatement(schema, table string) string {
	return fmt.Sprintf(`ALTER TABLE %q.%q VALIDATE CONSTRAINT %q`, schema, table, StageCheckName(table))
}
