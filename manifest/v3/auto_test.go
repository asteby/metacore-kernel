package v3

import (
	"strings"
	"testing"
)

func TestGenerateAutoSecret(t *testing.T) {
	a, err := GenerateAutoSecret()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := GenerateAutoSecret()
	if a == b {
		t.Fatal("two generated secrets are equal")
	}
	if !strings.HasPrefix(a, AutoSecretPrefix) || len(a) != len(AutoSecretPrefix)+43 {
		t.Fatalf("unexpected secret shape %q", a)
	}
}

func TestValidateSettingExtras_Auto(t *testing.T) {
	ok := Setting{Key: "api_key", Type: "secret", Required: true, Auto: SettingAutoStorefrontBearer}
	if errs := ValidateSettingExtras("connectors[0].credentials", []Setting{ok}); len(errs) != 0 {
		t.Fatalf("valid auto credential rejected: %v", errs)
	}
	cases := map[string]struct {
		where string
		s     Setting
		want  string
	}{
		"unknown value": {"connectors[0].credentials", Setting{Key: "k", Type: "secret", Auto: "x"}, "not supported"},
		"not secret":    {"connectors[0].credentials", Setting{Key: "k", Type: "text", Auto: SettingAutoStorefrontBearer}, "requires type secret"},
		"default":       {"connectors[0].credentials", Setting{Key: "k", Type: "secret", Auto: SettingAutoStorefrontBearer, Default: "x"}, "cannot declare a default"},
		"in settings":   {"settings", Setting{Key: "k", Type: "secret", Auto: SettingAutoStorefrontBearer}, "only valid on a connector credential"},
	}
	for name, c := range cases {
		errs := ValidateSettingExtras(c.where, []Setting{c.s})
		if !strings.Contains(strings.Join(errs, ";"), c.want) {
			t.Errorf("%s: want %q in %v", name, c.want, errs)
		}
	}
}

func TestValidateSettingExtras_AutoWebhookSigning(t *testing.T) {
	const w = "connectors[0].credentials"
	ok := Setting{Key: "signing_key", Type: "secret", Required: true, Auto: SettingAutoWebhookSigning}
	if errs := ValidateSettingExtras(w, []Setting{ok}); len(errs) != 0 {
		t.Fatalf("valid webhook_signing credential rejected: %v", errs)
	}
	if !IsKnownSettingAuto("webhook_signing") || !IsKnownSettingAuto("storefront_bearer") || IsKnownSettingAuto("x") {
		t.Fatal("IsKnownSettingAuto mismatch")
	}
	cases := map[string]struct {
		where string
		s     Setting
		want  string
	}{
		"outside connectors": {"settings", Setting{Key: "k", Type: "secret", Auto: SettingAutoWebhookSigning}, "only valid on a connector credential"},
		"not secret":         {w, Setting{Key: "k", Type: "text", Auto: SettingAutoWebhookSigning}, "requires type secret"},
		"default":            {w, Setting{Key: "k", Type: "secret", Auto: SettingAutoWebhookSigning, Default: "x"}, "cannot declare a default"},
		"visible_when":       {w, Setting{Key: "k", Type: "secret", Auto: SettingAutoWebhookSigning, VisibleWhen: &VisibleWhen{}}, "cannot declare visible_when"},
		"unknown":            {w, Setting{Key: "k", Type: "secret", Auto: "nope"}, "storefront_bearer, webhook_signing"},
	}
	for name, c := range cases {
		errs := ValidateSettingExtras(c.where, []Setting{c.s})
		if !strings.Contains(strings.Join(errs, ";"), c.want) {
			t.Errorf("%s: want %q in %v", name, c.want, errs)
		}
	}
}
