package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDynamicToolNameDescriptionParameters(t *testing.T) {
	tool := NewDynamicTool(&ToolDefinition{
		Name: "Consultar Pedido!", Description: "Busca un pedido.", Type: ToolTypeAPI, Category: "integration",
		InputSchema: []ToolInputParam{
			{Name: "order_id", Type: "date", Description: "Folio", Required: true, ExtractionHint: "en el mensaje", Example: "A-1", DefaultValue: "X"},
			{Name: "note", Type: "number"},
		},
	})
	if tool.Name() != "consultar_pedido" || tool.Category() != "integration" {
		t.Fatalf("%q %q", tool.Name(), tool.Category())
	}
	if d := tool.Description(); !strings.Contains(d, "REQUIERE: order_id (en el mensaje)") {
		t.Fatalf("description %q", d)
	}
	p := tool.Parameters()
	oid := p["order_id"].(map[string]interface{})
	if oid["type"] != "string" || oid["required"] != true || oid["default"] != "X" || !strings.Contains(oid["description"].(string), "Ejemplo: A-1.") {
		t.Fatalf("%v", oid)
	}
	if p["note"].(map[string]interface{})["type"] != "number" {
		t.Fatal("number type lost")
	}
	if got := sanitizeToolName("9lives"); got != "_9lives" {
		t.Fatalf("%q", got)
	}
	if got := sanitizeToolName(strings.Repeat("a", 90)); len(got) != 64 {
		t.Fatalf("len %d", len(got))
	}
}

func TestDynamicToolParametersFromTemplateVariables(t *testing.T) {
	tool := NewDynamicTool(&ToolDefinition{Name: "t", Endpoint: "https://x/{{id}}", BodyTemplate: `{"q":"{{q}}"}`})
	p := tool.Parameters()
	if len(p) != 2 || p["id"] == nil || p["q"] == nil {
		t.Fatalf("%v", p)
	}
}

func TestNormalizeParameters(t *testing.T) {
	tool := NewDynamicTool(&ToolDefinition{Name: "t", InputSchema: []ToolInputParam{
		{Name: "rfc", Normalize: "uppercase"},
		{Name: "tel", Normalize: "alphanumeric"},
	}})
	out := tool.normalizeParameters(map[string]interface{}{"rfc": " abc ", "tel": "55-12 34", "n": 3})
	if out["rfc"] != "ABC" || out["tel"] != "551234" || out["n"] != 3 {
		t.Fatalf("%v", out)
	}
}

func TestExecuteHTTPTemplatesAuthAndErrors(t *testing.T) {
	allowPrivateEgress = true
	defer func() { allowPrivateEgress = false }()

	var gotPath, gotAuth, gotBody, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotMethod = r.URL.Path, r.Header.Get("Authorization"), r.Method
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		if r.URL.Path == "/boom" {
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"error":"bad"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer srv.Close()

	tool := NewDynamicTool(&ToolDefinition{
		Name: "t", Type: ToolTypeWebhook, Endpoint: srv.URL + "/orders/{{id}}",
		BodyTemplate: `{"q":"{{q}}"}`, AuthType: "bearer", AuthConfig: map[string]interface{}{"token": "tok"},
	})
	res, err := tool.Execute(context.Background(), map[string]interface{}{"id": "7", "q": "hola"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.(map[string]interface{})["ok"] != true || gotPath != "/orders/7" || gotAuth != "Bearer tok" || gotBody != `{"q":"hola"}` || gotMethod != "POST" {
		t.Fatalf("path=%s auth=%s body=%s method=%s", gotPath, gotAuth, gotBody, gotMethod)
	}

	boom := NewDynamicTool(&ToolDefinition{Name: "b", Type: ToolTypeAPI, Method: "GET", Endpoint: srv.URL + "/boom"})
	_, err = boom.Execute(context.Background(), nil, nil)
	if err == nil || !strings.Contains(err.Error(), `"status_code":500`) {
		t.Fatalf("expected structured error, got %v", err)
	}
}

func TestExecuteBlocksInternalEgressAndDisabledTypes(t *testing.T) {
	// allowPrivateEgress is false: a loopback destination must be refused.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	tool := NewDynamicTool(&ToolDefinition{Name: "t", Type: ToolTypeAPI, Method: "GET", Endpoint: srv.URL})
	if _, err := tool.Execute(context.Background(), nil, nil); !errors.Is(err, ErrBlockedEgress) {
		t.Fatalf("loopback must be blocked, got %v", err)
	}
	q := NewDynamicTool(&ToolDefinition{Name: "q", Type: ToolTypeQuery})
	if _, err := q.Execute(context.Background(), nil, nil); !errors.Is(err, ErrQueryToolsDisabled) {
		t.Fatalf("query tools must stay disabled, got %v", err)
	}
	if _, err := NewDynamicTool(&ToolDefinition{Name: "m", Type: ToolTypeMCP}).Execute(context.Background(), nil, nil); err == nil {
		t.Fatal("mcp unsupported")
	}
	if _, err := NewDynamicTool(&ToolDefinition{Name: "x", Type: "weird"}).Execute(context.Background(), nil, nil); err == nil {
		t.Fatal("unknown type")
	}
	if _, err := NewDynamicTool(&ToolDefinition{Name: "e", Type: ToolTypeAPI}).Execute(context.Background(), nil, nil); err == nil {
		t.Fatal("missing endpoint")
	}
}
