package v3

import (
	"strings"
	"testing"
)

const configManifestTpl = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": {"key": "demo", "name": "Demo", "version": "0.1.0"},
  "compatibility": {"requires": [{"key": "metacore-kernel", "version": ">=3.0.0 <4.0.0"}]},
  "models": [{"key": "Thing", "table": "things", "label": "t.thing", "columns": [
    {"name": "id", "type": "uuid", "primary_key": true, "label": "t.id"}
  ]}],
  "contributions": {"config": %s}
}`

func TestConfigContribution(t *testing.T) {
	cases := []struct {
		name, cfg, wantErr string
	}{
		{"model ok", `{"model": "Thing"}`, ""},
		{"url ok", `{"url": "addon://demo/settings"}`, ""},
		{"empty", `{}`, "one of model or url is required"},
		{"both", `{"model": "Thing", "url": "/x"}`, "mutually exclusive"},
		{"unknown model", `{"model": "Nope"}`, "is not a model of this addon"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Validate([]byte(strings.Replace(configManifestTpl, "%s", c.cfg, 1)))
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("want ok, got: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("want %q, got %v", c.wantErr, err)
			}
		})
	}
}
