package agent

import (
	"context"
	"errors"
	"testing"
)

type fakeChatModel struct {
	out map[string]interface{}
	err error
	sys string
}

func (f *fakeChatModel) ChatWithFunctions(_ context.Context, _ []map[string]interface{}, system string, _ []map[string]interface{}) (map[string]interface{}, error) {
	f.sys = system
	return f.out, f.err
}

func openAIReply(content string, calls ...map[string]interface{}) map[string]interface{} {
	msg := map[string]interface{}{"content": content}
	if len(calls) > 0 {
		raw := make([]interface{}, len(calls))
		for i, c := range calls {
			raw[i] = c
		}
		msg["tool_calls"] = raw
	}
	return map[string]interface{}{"choices": []interface{}{map[string]interface{}{"message": msg}}}
}

func fnCall(id, name, args string) map[string]interface{} {
	return map[string]interface{}{"id": id, "type": "function", "function": map[string]interface{}{"name": name, "arguments": args}}
}

func TestAdapterParsesNativeToolCalls(t *testing.T) {
	m := &fakeChatModel{out: openAIReply("", fnCall("c1", "query_data", `{"model":"customers"}`), fnCall("", "calculator", `{"x":1}`), fnCall("c3", "bad", `{not json`))}
	resp, err := NewAIServiceAdapter(m).GenerateChatWithTools(context.Background(), nil, "sys", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 2 {
		t.Fatalf("unparseable call must be skipped: %+v", resp.ToolCalls)
	}
	if resp.ToolCalls[0].ToolName != "query_data" || resp.ToolCalls[0].Parameters["model"] != "customers" {
		t.Fatalf("%+v", resp.ToolCalls[0])
	}
	if resp.ToolCalls[1].ID == "" {
		t.Fatal("missing id must be generated")
	}
}

func TestAdapterWrapsPlainReplyAsFinalAnswerButNotNarration(t *testing.T) {
	a := NewAIServiceAdapter(&fakeChatModel{out: openAIReply("Hay 3 clientes.")})
	resp, _ := a.GenerateChatWithTools(context.Background(), nil, "", nil)
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ToolName != "final_answer" || resp.ToolCalls[0].Parameters["message"] != "Hay 3 clientes." {
		t.Fatalf("%+v", resp.ToolCalls)
	}
	a = NewAIServiceAdapter(&fakeChatModel{out: openAIReply("Voy a revisar los clientes ahora.")})
	resp, _ = a.GenerateChatWithTools(context.Background(), nil, "", nil)
	if len(resp.ToolCalls) != 0 {
		t.Fatalf("deferred narration must not become a final answer: %+v", resp.ToolCalls)
	}
}

func TestAdapterMalformedAndErrors(t *testing.T) {
	for _, out := range []map[string]interface{}{
		{}, {"choices": []interface{}{}}, {"choices": []interface{}{"junk"}}, {"choices": []interface{}{map[string]interface{}{"message": "junk"}}},
	} {
		resp, err := NewAIServiceAdapter(&fakeChatModel{out: out}).GenerateChatWithTools(context.Background(), nil, "", nil)
		if err != nil || resp == nil || len(resp.ToolCalls) != 0 {
			t.Fatalf("malformed %v must be an empty response, got %+v %v", out, resp, err)
		}
	}
	boom := errors.New("provider down")
	if _, err := NewAIServiceAdapter(&fakeChatModel{err: boom}).GenerateChatWithTools(context.Background(), nil, "", nil); !errors.Is(err, boom) {
		t.Fatal("model errors must propagate")
	}
}

func TestParseLegacyResponse(t *testing.T) {
	a := NewAIServiceAdapter(nil)
	r := a.parseResponse(`{"tool_calls":[{"tool_name":"calculator","parameters":{"x":2}}]}`)
	if len(r.ToolCalls) != 1 || r.ToolCalls[0].ToolName != "calculator" {
		t.Fatalf("%+v", r.ToolCalls)
	}
	r = a.parseResponse("hola")
	if r.ToolCalls[0].ToolName != "final_answer" {
		t.Fatal("plain text must wrap as final_answer")
	}
}
