package manifest_test

import (
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// pipelineManifestJSON exercises the stage-machine + kanban view-type surface:
// a model with stage_field/stages/transitions/on_transition and a nav entry
// declaring view_type:kanban + group_by. All are additive v3 fields the strict
// schema + struct validator must accept.
const pipelineManifestJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "integration_github", "name": "GitHub", "version": "1.0.0" },
  "compatibility": { "requires": [{ "key": "kernel", "version": ">=3.0.0 <4.0.0" }] },
  "models": [
    {
      "key": "Issue",
      "table": "github_issues",
      "columns": [
        { "name": "id", "type": "uuid", "primary_key": true, "default": "gen_random_uuid()" },
        { "name": "title", "type": "text", "not_null": true },
        { "name": "stage", "type": "text", "not_null": true }
      ],
      "stage_field": "stage",
      "stages": [
        { "key": "backlog",     "label": "Backlog",     "color": "slate", "order": 0 },
        { "key": "in_progress", "label": "In Progress", "color": "blue",  "order": 1 },
        { "key": "review",      "label": "Review",      "color": "amber", "order": 2 },
        { "key": "done",        "label": "Done",        "color": "green", "order": 3, "is_final": true }
      ],
      "transitions": [
        { "from": "backlog",     "to": "in_progress" },
        { "from": "in_progress", "to": "review" },
        { "from": "review",      "to": "done" },
        { "from": "review",      "to": "in_progress" }
      ],
      "on_transition": [
        { "from": "*", "to": "done", "do": "wasm:push_on_transition", "required": true },
        { "from": "*", "to": "*",    "do": "wasm:log_stage_change" }
      ]
    }
  ],
  "contributions": {
    "navigation": [
      {
        "title": "GitHub",
        "items": [
          { "title": "Board", "model": "Issue", "view_type": "kanban", "group_by": "stage" },
          { "title": "All", "model": "Issue" }
        ]
      }
    ]
  }
}`

// TestParse_Pipeline_Accepted asserts the stage machine + kanban nav survive
// v3.Parse (schema + cross-field validation) and round-trip onto the typed model.
func TestParse_Pipeline_Accepted(t *testing.T) {
	m, err := v3.Parse([]byte(pipelineManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse rejected pipeline manifest: %v", err)
	}
	if len(m.Models) != 1 {
		t.Fatalf("models = %d, want 1", len(m.Models))
	}
	mod := m.Models[0]
	if mod.StageField != "stage" {
		t.Errorf("stage_field = %q, want stage", mod.StageField)
	}
	if len(mod.Stages) != 4 || mod.Stages[3].Key != "done" || !mod.Stages[3].IsFinal {
		t.Errorf("stages = %+v", mod.Stages)
	}
	if len(mod.Transitions) != 4 {
		t.Errorf("transitions = %d, want 4", len(mod.Transitions))
	}
	if len(mod.OnTransition) != 2 || !mod.OnTransition[0].Required {
		t.Errorf("on_transition = %+v", mod.OnTransition)
	}
	board := m.Contributions.Navigation[0].Items[0]
	if board.ViewType != "kanban" || board.GroupBy != "stage" {
		t.Errorf("board nav = %+v, want view_type kanban group_by stage", board)
	}
}

// TestFromV3_Pipeline_Mapped asserts the stage machine + kanban hint map onto the
// host ModelDefinition / NavItem so the dynamic engine and host can consume them.
func TestFromV3_Pipeline_Mapped(t *testing.T) {
	m, err := v3.Parse([]byte(pipelineManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse: %v", err)
	}
	out := manifest.FromV3(m)
	if len(out.ModelDefinitions) != 1 {
		t.Fatalf("model_definitions = %d, want 1", len(out.ModelDefinitions))
	}
	md := out.ModelDefinitions[0]
	if md.StageField != "stage" {
		t.Errorf("mapped stage_field = %q, want stage", md.StageField)
	}
	if len(md.Stages) != 4 || md.Stages[1].Key != "in_progress" || md.Stages[1].Color != "blue" {
		t.Errorf("mapped stages = %+v", md.Stages)
	}
	if len(md.Transitions) != 4 || md.Transitions[0].From != "backlog" {
		t.Errorf("mapped transitions = %+v", md.Transitions)
	}
	if len(md.OnTransition) != 2 || md.OnTransition[0].Do != "wasm:push_on_transition" || !md.OnTransition[0].Required {
		t.Errorf("mapped on_transition = %+v", md.OnTransition)
	}
	board := out.Navigation[0].Items[0]
	if board.ViewType != "kanban" || board.GroupBy != "stage" {
		t.Errorf("mapped board nav = %+v", board)
	}
	// Strict (install-surface) validation must also accept it.
	if err := out.Validate("3.5.0"); err != nil {
		t.Fatalf("strict manifest.Validate rejected mapped pipeline: %v", err)
	}
}

// TestValidate_Pipeline_Rejections is table-driven over the cross-field rules the
// schema cannot express: undeclared stage_field, transition referencing an
// unknown stage, kanban nav without group_by, and an on_transition with a bad
// dispatch prefix.
func TestValidate_Pipeline_Rejections(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(string) string
		want   string
	}{
		{
			name: "stage_field not a column",
			mutate: func(s string) string {
				return strings.Replace(s, `"stage_field": "stage"`, `"stage_field": "ghost"`, 1)
			},
			want: "stage_field",
		},
		{
			name: "transition to unknown stage",
			mutate: func(s string) string {
				return strings.Replace(s, `{ "from": "review",      "to": "done" }`, `{ "from": "review",      "to": "shipped" }`, 1)
			},
			want: "transition",
		},
		{
			name: "kanban nav without group_by",
			mutate: func(s string) string {
				return strings.Replace(s, `"view_type": "kanban", "group_by": "stage"`, `"view_type": "kanban"`, 1)
			},
			want: "group_by",
		},
		{
			name: "hook with bad prefix",
			mutate: func(s string) string {
				return strings.Replace(s, `"do": "wasm:log_stage_change"`, `"do": "rpc:log_stage_change"`, 1)
			},
			want: "do",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := tc.mutate(pipelineManifestJSON)
			err := v3.Validate([]byte(raw))
			if err == nil {
				t.Fatalf("expected validation error mentioning %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}
