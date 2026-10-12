package tools

import (
	"context"
	"testing"

	"github.com/asteby/metacore-kernel/agent/contextmgr"
)

func mk(name, cat string, params map[string]interface{}) BaseTool {
	return NewToolBuilder(name).WithCategory(cat).WithDescription("d").NewTool(
		func(context.Context, map[string]interface{}, *contextmgr.ConversationContext) (interface{}, error) {
			return name, nil
		})
}

func TestCloneIsIndependent(t *testing.T) {
	r := NewToolRegistry()
	r.Register(mk("a", "data", nil))
	c := r.Clone()
	c.Register(mk("b", "data", nil))
	if _, ok := r.Get("b"); ok {
		t.Fatal("clone leaked into parent")
	}
	if len(c.List()) != 2 || len(r.List()) != 1 {
		t.Fatalf("sizes %d %d", len(c.List()), len(r.List()))
	}
}

func TestRegisterTwiceNoDuplicateCategory(t *testing.T) {
	r := NewToolRegistry()
	r.Register(mk("a", "data", nil))
	r.Register(mk("a", "data", nil))
	if got := len(r.ListByCategory(CategoryData)); got != 1 {
		t.Fatalf("got %d", got)
	}
	r.Register(mk("a", "workflow", nil))
	if len(r.ListByCategory(CategoryData)) != 0 || len(r.ListByCategory(CategoryWorkflow)) != 1 {
		t.Fatal("category move failed")
	}
}

func TestIsActionTool(t *testing.T) {
	r := NewToolRegistry()
	r.Register(mk("gen", "creative", nil))
	r.Register(mk("q", "data", nil))
	if !r.IsActionTool("gen") || r.IsActionTool("q") || !r.IsActionTool("final_answer") {
		t.Fatal("action classification wrong")
	}
}

func TestToOpenAIFormat(t *testing.T) {
	r := NewToolRegistry()
	r.Register(&BaseToolImpl{NameVal: "x", CategoryVal: "data", DescriptionVal: "d", ParametersVal: map[string]interface{}{
		"q": map[string]interface{}{"type": "string", "required": true},
		"n": map[string]interface{}{"type": "number"},
	}})
	r.Register(&BaseToolImpl{NameVal: "y", CategoryVal: "data", DescriptionVal: "d", ParametersVal: map[string]interface{}{
		"type":       "object",
		"properties": map[string]interface{}{"z": map[string]interface{}{"type": "string"}},
		"required":   []string{"z"},
	}})
	fns := r.ToOpenAIFormat()
	if len(fns) != 2 {
		t.Fatal(len(fns))
	}
	for _, f := range fns {
		p := f["parameters"].(map[string]interface{})
		props := p["properties"].(map[string]interface{})
		req := p["required"].([]string)
		switch f["name"] {
		case "x":
			if len(req) != 1 || req[0] != "q" {
				t.Fatalf("x req %v", req)
			}
			if _, bad := props["q"].(map[string]interface{})["required"]; bad {
				t.Fatal("internal required flag leaked")
			}
		case "y":
			if len(req) != 1 || req[0] != "z" || props["z"] == nil {
				t.Fatalf("y %v %v", req, props)
			}
		}
	}
}
