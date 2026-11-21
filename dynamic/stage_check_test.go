package dynamic

import (
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
)

func stageDef(check bool) manifest.ModelDefinition {
	return manifest.ModelDefinition{
		TableName: "orders", ModelKey: "Order", OrgScoped: true,
		Columns:    []manifest.ColumnDef{{Name: "status", Type: "string"}},
		StageField: "status", StageCheck: check,
		Stages: []manifest.StageDef{{Key: "draft"}, {Key: "posted"}, {Key: "it's"}},
	}
}

func TestStageCheckDDLIsOptInAndNotValid(t *testing.T) {
	opts := DDLOptions{AddonKey: "demo", Isolation: IsolationShared}
	off, err := ToDDL(stageDef(false), opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range off {
		if strings.Contains(s, "CONSTRAINT") && strings.Contains(s, "_stage") {
			t.Fatalf("no CHECK without opt-in: %s", s)
		}
	}
	on, err := ToDDL(stageDef(true), opts)
	if err != nil {
		t.Fatal(err)
	}
	var drop, add string
	for _, s := range on {
		switch {
		case strings.Contains(s, "DROP CONSTRAINT IF EXISTS"):
			drop = s
		case strings.Contains(s, "ADD CONSTRAINT"):
			add = s
		}
	}
	if drop == "" || !strings.Contains(add, `CHECK ("status" IN ('draft','posted','it''s')) NOT VALID`) {
		t.Fatalf("drop=%q add=%q", drop, add)
	}
	if len(on) != len(off)+2 {
		t.Fatalf("exactly two extra statements, got %d vs %d", len(on), len(off))
	}
}

func TestStageCheckInertWithoutMachine(t *testing.T) {
	d := stageDef(true)
	d.Stages = nil
	if got := stageCheckStatements("s", d); got != nil {
		t.Fatalf("no stages, no CHECK: %v", got)
	}
	if got := StageCheckValidateStatement("s", "orders"); !strings.Contains(got, `VALIDATE CONSTRAINT "ck_orders_stage"`) {
		t.Fatal(got)
	}
}
