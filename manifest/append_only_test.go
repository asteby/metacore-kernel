package manifest

import (
	"strings"
	"testing"
)

func TestValidateConstraints_AppendOnlyRejectsStageMachine(t *testing.T) {
	ok := ModelDefinition{AppendOnly: true}
	if err := validateConstraints(ok, map[string]struct{}{}); err != nil {
		t.Fatalf("plain ledger: %v", err)
	}
	bad := ModelDefinition{AppendOnly: true, StageField: "state"}
	err := validateConstraints(bad, map[string]struct{}{"state": {}})
	if err == nil || !strings.Contains(err.Error(), "append_only") {
		t.Fatalf("want append_only/stage conflict, got %v", err)
	}
}
