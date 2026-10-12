package agent

import (
	"strings"
	"testing"
)

func TestCleanMarkdownForChatKeepsGFMBold(t *testing.T) {
	in := "Puedo ayudarte:\n\n- **Recursos Humanos**: Gestión\n- **Productos**: Catálogo\n"
	out := CleanMarkdownForChat(in)
	if !strings.Contains(out, "**Recursos Humanos**") || !strings.Contains(out, "**Productos**") {
		t.Fatalf("expected GFM **bold** preserved, got: %q", out)
	}
}

func TestCleanMarkdownForChatHeadersBecomeBold(t *testing.T) {
	out := CleanMarkdownForChat("# Título\n\nHola")
	if out != "**Título**\n\nHola" {
		t.Fatalf("got %q", out)
	}
}

func TestFormatMarkdownForWhatsApp(t *testing.T) {
	out := FormatMarkdownForWhatsApp("Hola **Mundo** y **más**")
	want := "Hola *Mundo* y *más*"
	if out != want {
		t.Fatalf("got %q want %q", out, want)
	}
}

func TestCleanAgentResponsePreservesBold(t *testing.T) {
	out := CleanAgentResponse("- **Clientes**: lista")
	if !strings.Contains(out, "**Clientes**") {
		t.Fatalf("CleanAgentResponse stripped bold: %q", out)
	}
}
