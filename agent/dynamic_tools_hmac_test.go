package agent

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"testing"
	"time"
)

func TestApplyAuthBodyMetacoreHMAC(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://example.com/hook", nil)
	now := time.Unix(1_700_000_000, 0)
	body := []byte(`{"a":1}`)
	if err := applyAuthBody(req, AuthMetacoreHMAC, map[string]interface{}{"secret": "s3", "delivery_id": "d-1"}, body, now); err != nil {
		t.Fatal(err)
	}
	m := hmac.New(sha256.New, []byte("s3"))
	m.Write([]byte(`1700000000.d-1.{"a":1}`))
	want := "sha256=" + hex.EncodeToString(m.Sum(nil))
	if got := req.Header.Get(HeaderHubSignature); got != want {
		t.Fatalf("signature = %q want %q", got, want)
	}
	if req.Header.Get(HeaderHubTimestamp) != "1700000000" || req.Header.Get(HeaderIdempotency) != "d-1" {
		t.Fatalf("headers = %v", req.Header)
	}
	// no secret -> error, nothing sent unsigned.
	if err := applyAuthBody(req, AuthMetacoreHMAC, map[string]interface{}{}, body, now); err == nil {
		t.Fatal("missing secret must fail")
	}
	// generated delivery id when none configured.
	req2, _ := http.NewRequest(http.MethodPost, "https://example.com/hook", nil)
	if err := applyAuthBody(req2, AuthMetacoreHMAC, map[string]interface{}{"secret": "s3"}, body, now); err != nil || req2.Header.Get(HeaderIdempotency) == "" {
		t.Fatalf("generated delivery id: %v %v", err, req2.Header)
	}
	// legacy auth types still go through applyAuth.
	req3, _ := http.NewRequest(http.MethodGet, "https://example.com", nil)
	_ = applyAuthBody(req3, "bearer", map[string]interface{}{"token": "t"}, nil, now)
	if req3.Header.Get("Authorization") != "Bearer t" {
		t.Fatal("bearer broke")
	}
}

func TestDynamicToolParametersFullSchema(t *testing.T) {
	tool := NewDynamicTool(&ToolDefinition{Name: "x", Type: ToolTypeAPI, Schema: map[string]interface{}{
		"required":   []interface{}{"status"},
		"properties": map[string]interface{}{"status": map[string]interface{}{"type": "string", "enum": []interface{}{"a", "b"}}},
	}})
	p := tool.Parameters()
	if p["type"] != "object" {
		t.Fatalf("type = %v", p["type"])
	}
	if req, ok := p["required"].([]string); !ok || len(req) != 1 || req[0] != "status" {
		t.Fatalf("required = %#v", p["required"])
	}
	st := p["properties"].(map[string]interface{})["status"].(map[string]interface{})
	if len(st["enum"].([]interface{})) != 2 {
		t.Fatalf("enum lost: %v", st)
	}
}
