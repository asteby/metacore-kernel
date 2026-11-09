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
