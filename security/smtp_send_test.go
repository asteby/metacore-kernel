package security

import (
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
)

func TestCanSMTPHosts(t *testing.T) {
	caps := Compile("connector_email", []manifest.Capability{
		{Kind: "smtp:send", Target: "connector:email.smtp_host"},
		{Kind: "smtp:send", Target: "smtp.sendgrid.net"},
	})
	if refs := caps.SMTPHostRefs(); len(refs) != 1 || refs[0].Connector != "email" || refs[0].Credential != "smtp_host" {
		t.Fatalf("refs inesperadas: %+v", refs)
	}
	// host fijo en el manifiesto
	if err := caps.CanSMTPHosts("smtp.sendgrid.net", nil); err != nil {
		t.Fatalf("host fijo debió pasar: %v", err)
	}
	// host salido del conector de la org (con o sin puerto, sin distinguir mayúsculas)
	if err := caps.CanSMTPHosts("mail.7leguas.com.mx", []string{"mail.7leguas.com.mx"}); err != nil {
		t.Fatalf("host del conector debió pasar: %v", err)
	}
	if err := caps.CanSMTPHosts("Mail.7Leguas.com.mx", []string{"mail.7leguas.com.mx:587"}); err != nil {
		t.Fatalf("host del conector con puerto y mayúsculas debió pasar: %v", err)
	}
	// otro host: no
	if err := caps.CanSMTPHosts("evil.example.com", []string{"mail.7leguas.com.mx"}); err == nil {
		t.Fatal("un host no concedido debió rechazarse")
	}
	// SSRF: aunque el conector apunte a loopback / red privada / metadata, se rechaza
	for _, h := range []string{"localhost", "127.0.0.1", "10.0.0.5", "169.254.169.254", "192.168.1.10"} {
		if err := caps.CanSMTPHosts(h, []string{h}); err == nil {
			t.Fatalf("%s debió rechazarse (SSRF)", h)
		}
	}
	// sin la capacidad no hay SMTP
	none := Compile("otro", []manifest.Capability{{Kind: "http:fetch", Target: "api.example.com"}})
	if err := none.CanSMTPHosts("mail.7leguas.com.mx", []string{"mail.7leguas.com.mx"}); err != nil {
		// el host del conector solo cuenta si el addon declaró smtp:send; aquí lo trae el llamador
		// (smtpConnectorHosts devuelve vacío sin refs), así que sin refs el llamador pasa nil.
		t.Logf("(informativo) %v", err)
	}
	if err := none.CanSMTPHosts("mail.7leguas.com.mx", nil); err == nil {
		t.Fatal("sin smtp:send debió rechazarse")
	}
}
