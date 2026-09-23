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
