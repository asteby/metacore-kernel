package v3

import (
	"strings"
	"testing"
)

func actionPriorityManifest(priority string) []byte {
	return []byte(`{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "shares", "name": "Shares", "version": "1.0.0" },
  "compatibility": { "requires": [{ "key": "kernel", "version": ">=3.0.0 <4.0.0" }] },
  "contributions": {
    "actions": [
      { "key": "send_email", "label": "Enviar por correo", "target_model": "order",
        "handler": { "type": "wasm", "function": "send" }, "priority": "` + priority + `" }
    ]
  }
}`)
}

func TestValidate_ActionPriority(t *testing.T) {
	for _, p := range []string{"primary", "secondary"} {
		m, err := Parse(actionPriorityManifest(p))
		if err != nil {
			t.Fatalf("priority %q: unexpected error: %v", p, err)
		}
		if got := m.Contributions.Actions[0].Priority; got != p {
			t.Fatalf("priority %q: parsed %q", p, got)
		}
	}
}

func TestValidate_ActionPriorityRejectsUnknown(t *testing.T) {
	_, err := Parse(actionPriorityManifest("footer"))
	if err == nil || !strings.Contains(err.Error(), "priority") {
		t.Fatalf("expected a priority enum error, got %v", err)
	}
}
