package guest

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/asteby/metacore-kernel/capability"
)

// The guest mirrors must keep the host's JSON field names.
func TestAITypesMirrorCapability(t *testing.T) {
	pairs := [][2]any{
		{AIRequest{}, capability.AIRequest{}},
		{AIResult{}, capability.AIResult{}},
		{AIMessage{}, capability.AIMessage{}},
		{AIResponseFormat{}, capability.AIResponseFormat{}},
		{AIUsage{}, capability.AIUsage{}},
	}
	for _, p := range pairs {
		g, h := jsonTags(reflect.TypeOf(p[0])), jsonTags(reflect.TypeOf(p[1]))
		if !reflect.DeepEqual(g, h) {
			t.Errorf("%T tags %v != %T tags %v", p[0], g, p[1], h)
		}
	}
}

func jsonTags(t reflect.Type) []string {
	var out []string
	for i := 0; i < t.NumField(); i++ {
		out = append(out, t.Field(i).Tag.Get("json"))
	}
	return out
}

func TestEncodeAIQuery(t *testing.T) {
	b, err := encodeAIQuery(AIRequest{Feature: "ticket_triage", Prompt: "hola", IdempotencyKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Capability string         `json:"capability"`
		Input      map[string]any `json:"input"`
	}
	if err := json.Unmarshal(b, &got); err != nil || got.Capability != "ai.complete" || got.Input["feature"] != "ticket_triage" || got.Input["idempotency_key"] != "k" {
		t.Fatalf("request %s err=%v", b, err)
	}
	// The input validates against the host contract.
	c, _ := capability.Lookup(capability.AIComplete)
	if errs := c.CheckInput(got.Input); len(errs) != 0 {
		t.Fatalf("guest input rejected by the contract: %v", errs)
	}
}

func TestDecodeAIEnvelope(t *testing.T) {
	ok := `{"success":true,"data":{"capability":"ai.complete","results":[{"provider":"host","output":{"status":"ok","content":"urgente","usage":{"input_tokens":3,"output_tokens":1},"cost_sparks":2}}],"errors":[]}}`
	res, err := decodeAIEnvelope([]byte(ok))
	if err != nil || !res.OK() || res.Content != "urgente" || res.CostSparks != 2 {
		t.Fatalf("ok: %+v %v", res, err)
	}
	pulse := `{"success":true,"data":{"results":[{"provider":"host","output":{"status":"insufficient_pulse","message":"sin pulse","usage":{"input_tokens":0,"output_tokens":0},"cost_sparks":0}}],"errors":[]}}`
	res, err = decodeAIEnvelope([]byte(pulse))
	if err != nil || res.Status != "insufficient_pulse" {
		t.Fatalf("no pulse must be a result: %+v %v", res, err)
	}
	var aiErr *AIError
	forbidden := `{"success":true,"data":{"results":[],"errors":[{"provider":"host","code":"forbidden","message":"not granted"}]}}`
	if _, err := decodeAIEnvelope([]byte(forbidden)); !errors.As(err, &aiErr) || aiErr.Code != "forbidden" {
		t.Fatalf("forbidden must be an AIError, got %v", err)
	}
	denied := `{"success":false,"error":{"code":"forbidden","message":"addon lacks capability:query"}}`
	if _, err := decodeAIEnvelope([]byte(denied)); !errors.As(err, &aiErr) {
		t.Fatalf("import denial must be an AIError, got %v", err)
	}
	// Old host / host without AI: unavailable, never an error.
	for _, body := range []string{
		`{"success":false,"error":{"code":"capability_unavailable","message":"no querier"}}`,
		`{"success":false,"error":{"code":"no_provider","message":"x"}}`,
		`{"success":true,"data":{"results":[],"errors":[{"provider":"host","code":"timeout","message":"slow"}]}}`,
		``,
	} {
		res, err := decodeAIEnvelope([]byte(body))
		if err != nil || res.Status != "unavailable" {
			t.Fatalf("%q must be unavailable: %+v %v", body, res, err)
		}
	}
}
