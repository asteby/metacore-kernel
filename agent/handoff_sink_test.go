package agent

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/agent/contextmgr"
	"github.com/google/uuid"
)

type fakeSink struct {
	calls int
	got   HandoffRequest
	res   HandoffResult
	err   error
	boom  bool
}

func (f *fakeSink) Handoff(_ context.Context, r HandoffRequest) (HandoffResult, error) {
	f.calls++
	f.got = r
	if f.boom {
		panic("sink exploded")
	}
	return f.res, f.err
}

func handoffCtx(n int) *contextmgr.ConversationContext {
	c := &contextmgr.ConversationContext{ConversationID: uuid.New()}
	for i := 0; i < n; i++ {
		c.History = append(c.History, contextmgr.Message{Role: "user", Content: strings.Repeat("x", 10)})
	}
	return c
}

func TestHandoffWithoutSinkUnchanged(t *testing.T) {
	cases := []struct {
		name   string
		params map[string]interface{}
		reason string
	}{
		{"with reason", map[string]interface{}{"reason": "angry customer"}, "angry customer"},
		{"empty reason", map[string]interface{}{}, "Usuario solicitó atención humana"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tool := NewHandoffToHumanTool()
			got, err := tool.Execute(context.Background(), tc.params, handoffCtx(2))
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]interface{}{
				"status":  "handoff_initiated",
				"reason":  tc.reason,
				"message": "La conversación será transferida a un agente humano",
				"action":  "DISABLE_BOT",
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got %v want %v", got, want)
			}
			if _, ok := tool.Parameters()["severity"]; ok {
				t.Fatal("severity must not appear without a sink")
			}
		})
	}
	// A nil sink option is also the legacy behavior.
	if NewHandoffToHumanTool(WithHandoffSink(nil)).sink != nil {
		t.Fatal("nil sink must stay nil")
	}
}

func TestHandoffWithSink(t *testing.T) {
	cases := []struct {
		name       string
		sink       *fakeSink
		opts       []HandoffOption
		params     map[string]interface{}
		history    int
		wantTurns  int
		wantSev    string
		wantAction bool
		wantStatus string
		wantRef    string
	}{
		{"ok", &fakeSink{res: HandoffResult{Reference: "F-1", Status: "open"}}, nil,
			map[string]interface{}{"reason": "r", "severity": "HIGH"}, 3, 3, "high", true, "handoff_initiated", "F-1"},
		{"unknown severity defaults", &fakeSink{}, nil,
			map[string]interface{}{"reason": "r", "severity": "zzz"}, 1, 1, "normal", true, "handoff_initiated", ""},
		{"transcript bounded", &fakeSink{}, []HandoffOption{WithHandoffTranscriptLimits(2, 4)},
			map[string]interface{}{"reason": "r"}, 5, 2, "normal", true, "handoff_initiated", ""},
		{"sink error is controlled", &fakeSink{err: errors.New("secret internal detail")}, nil,
			map[string]interface{}{"reason": "r"}, 1, 1, "normal", false, "handoff_failed", ""},
		{"sink panic is controlled", &fakeSink{boom: true}, nil,
			map[string]interface{}{"reason": "r"}, 1, 1, "normal", false, "handoff_failed", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tool := NewHandoffToHumanTool(append([]HandoffOption{WithHandoffSink(tc.sink)}, tc.opts...)...)
			out, err := tool.Execute(context.Background(), tc.params, handoffCtx(tc.history))
			if err != nil {
				t.Fatal(err)
			}
			if tc.sink.calls != 1 {
				t.Fatalf("calls=%d", tc.sink.calls)
			}
			if tc.sink.got.Reason != "r" || tc.sink.got.Severity != tc.wantSev || len(tc.sink.got.Transcript) != tc.wantTurns || tc.sink.got.ConversationID == "" {
				t.Fatalf("req=%+v", tc.sink.got)
			}
			m := out.(map[string]interface{})
			if m["status"] != tc.wantStatus {
				t.Fatalf("status=%v", m["status"])
			}
			_, hasAction := m["action"]
			if hasAction != tc.wantAction {
				t.Fatalf("action present=%v", hasAction)
			}
			if !tc.wantAction {
				if _, has := m["message"]; has {
					t.Fatal("failure must not set a user-facing message")
				}
				if strings.Contains(m["error"].(string), "secret") {
					t.Fatal("sink error detail leaked")
				}
			}
			if tc.wantRef != "" && m["reference"] != tc.wantRef {
				t.Fatalf("reference=%v", m["reference"])
			}
			for _, turn := range tc.sink.got.Transcript {
				if len([]rune(turn.Text)) > 10 {
					t.Fatalf("turn too long: %q", turn.Text)
				}
			}
		})
	}
}

func TestHandoffTranscriptRedactedAndTruncated(t *testing.T) {
	sink := &fakeSink{}
	tool := NewHandoffToHumanTool(
		WithHandoffSink(sink),
		WithHandoffRedactor(func(s string) string { return strings.ReplaceAll(s, "secret", "[x]") }),
		WithHandoffTranscriptLimits(10, 8),
	)
	c := &contextmgr.ConversationContext{History: []contextmgr.Message{
		{Role: "user", Content: "my secret is here"},
		{Role: "assistant", Content: "   "},
	}}
	if _, err := tool.Execute(context.Background(), map[string]interface{}{"reason": "r"}, c); err != nil {
		t.Fatal(err)
	}
	tr := sink.got.Transcript
	if len(tr) != 1 || strings.Contains(tr[0].Text, "secret") || len([]rune(tr[0].Text)) != 8 || tr[0].Role != "user" {
		t.Fatalf("transcript=%+v", tr)
	}
}

func TestHandoffContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sink := &fakeSink{}
	tool := NewHandoffToHumanTool(WithHandoffSink(sink))
	out, err := tool.Execute(ctx, map[string]interface{}{"reason": "r"}, handoffCtx(1))
	if !errors.Is(err, context.Canceled) || out != nil || sink.calls != 0 {
		t.Fatalf("out=%v err=%v calls=%d", out, err, sink.calls)
	}
}

func TestHandoffNilConversationContextAndDescription(t *testing.T) {
	sink := &fakeSink{}
	tool := NewHandoffToHumanTool(WithHandoffSink(sink), WithHandoffDescription("custom"))
	if _, err := tool.Execute(context.Background(), map[string]interface{}{"reason": "r"}, nil); err != nil {
		t.Fatal(err)
	}
	if sink.got.ConversationID != "" || sink.got.Transcript != nil {
		t.Fatalf("req=%+v", sink.got)
	}
	if tool.Description() != "custom" || NewHandoffToHumanTool().Description() == "custom" {
		t.Fatal("description override")
	}
}
