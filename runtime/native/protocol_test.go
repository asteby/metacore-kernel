package native

import (
	"encoding/json"
	"strings"
	"testing"
)

func validInvocation() Invocation {
	return Invocation{Protocol: ProtocolV1, Operation: "send_message", Context: InvocationContext{
		OrganizationID: "org-1", InstallationID: "install-1", AddonKey: "connector_whatsapp",
		ActorID: "user-1", TraceID: "trace-1",
	}, Input: json.RawMessage(`{"to":"573001234567","text":"hola"}`)}
}

func TestInvocationValidate(t *testing.T) {
	if err := validInvocation().Validate(); err != nil {
		t.Fatalf("valid invocation rejected: %v", err)
	}
}

func TestInvocationRejectsUntrustedContextAndMalformedOperation(t *testing.T) {
	tests := []Invocation{validInvocation(), validInvocation()}
	tests[0].Context.OrganizationID = ""
	tests[1].Operation = "../../send"
	for _, invocation := range tests {
		if err := invocation.Validate(); err == nil {
			t.Fatalf("invalid invocation accepted: %#v", invocation)
		}
	}
}

func TestInvocationRejectsOversizedInput(t *testing.T) {
	i := validInvocation()
	i.Input = json.RawMessage(`"` + strings.Repeat("x", MaxPayloadBytes) + `"`)
	if err := i.Validate(); err == nil {
		t.Fatal("oversized input accepted")
	}
}

func TestInvocationResultUnion(t *testing.T) {
	good := []InvocationResult{
		{Protocol: ProtocolV1, OK: true, Data: json.RawMessage(`{"message_id":"m1"}`)},
		{Protocol: ProtocolV1, OK: false, Error: &InvocationError{Code: "not_connected", Message: "device is offline", Retryable: true}},
	}
	for _, result := range good {
		if err := result.Validate(); err != nil {
			t.Fatalf("valid result rejected: %v", err)
		}
	}
	bad := InvocationResult{Protocol: ProtocolV1, OK: true, Error: &InvocationError{Code: "bad", Message: "bad"}}
	if err := bad.Validate(); err == nil {
		t.Fatal("ambiguous result accepted")
	}
}
