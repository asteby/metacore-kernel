package capability

import (
	"reflect"
	"strings"
	"testing"
)

func TestResolveMapsRecordPayloadAndConst(t *testing.T) {
	record := map[string]any{"phone": "5215512345678", "name": "Ana"}
	payload := map[string]any{"text": "hola", "extra": 1}
	got := Resolve(map[string]string{"to": "record.phone", "message": "payload.text", "device_id": "const:main", "media_url": "record.missing"}, record, payload)
	want := map[string]any{"text": "hola", "extra": 1, "to": "5215512345678", "message": "hola", "device_id": "main"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Resolve = %#v, want %#v", got, want)
	}
}

func TestValidateInputAgainstContract(t *testing.T) {
	c, _ := Lookup(MessagingWhatsAppSend)
	if errs := ValidateInput(map[string]string{"to": "record.phone", "message": "payload.text"}, &c); len(errs) != 0 {
		t.Fatalf("valid mapping rejected: %v", errs)
	}
	errs := ValidateInput(map[string]string{"phone": "record.phone", "to": "lead.phone"}, &c)
	if len(errs) != 2 || !strings.Contains(strings.Join(errs, ";"), `"phone" is not a field`) {
		t.Fatalf("expected unknown field + bad source errors, got %v", errs)
	}
}

func TestMissingRequired(t *testing.T) {
	c, _ := Lookup(MessagingWhatsAppSend)
	if got := MissingRequired(c, map[string]any{"to": "  ", "message": "x"}); !reflect.DeepEqual(got, []string{"to"}) {
		t.Fatalf("blank `to` must be missing, got %v", got)
	}
	if !ValidKey("messaging.whatsapp.send") || ValidKey("whatsapp") || ValidKey("Messaging.send") {
		t.Fatal("capability keys are dotted lowercase segments")
	}
}

func TestMessagingEmailSendContract(t *testing.T) {
	c, ok := Lookup(MessagingEmailSend)
	if !ok || c.Key != "messaging.email.send" || c.IsQuery() || c.Idempotent {
		t.Fatalf("messaging.email.send must be a registered fire-and-forget action: %+v ok=%v", c, ok)
	}
	if !ValidKey(c.Key) {
		t.Fatal("key must be well formed")
	}
	if got := MissingRequired(c, map[string]any{"subject": "hi"}); len(got) != 1 || got[0] != "to" {
		t.Fatalf("to must be required, missing=%v", got)
	}
	if errs := c.CheckInput(map[string]any{
		"to": "a@x.com; b@x.com", "subject": "s", "html": "<p>x</p>", "variables": map[string]any{"n": 1},
		"attachments":     []any{map[string]any{"filename": "a.pdf", "blob_ref": "b1"}},
		"idempotency_key": "k1",
	}); len(errs) != 0 {
		t.Fatalf("valid input rejected: %v", errs)
	}
	if errs := c.CheckInput(map[string]any{"to": 5}); len(errs) == 0 {
		t.Fatal("non-string to must fail")
	}
	if errs := c.CheckInput(map[string]any{"to": "a@x.com", "attachments": []any{map[string]any{"filename": 7}}}); len(errs) == 0 {
		t.Fatal("attachment with a non-string filename must fail")
	}
	// Consumer-side mapping validation uses the contract's fields.
	if errs := ValidateInput(map[string]string{"to": "record.email", "subject": "const:Hola"}, &c); len(errs) != 0 {
		t.Fatalf("mapping rejected: %v", errs)
	}
	if errs := ValidateInput(map[string]string{"recipient": "record.email"}, &c); len(errs) == 0 {
		t.Fatal("unknown field must be rejected")
	}
	if len(c.Required)+len(c.Optional) != len(c.Input.Fields) {
		t.Fatal("Required/Optional must cover exactly the Input fields")
	}
}
