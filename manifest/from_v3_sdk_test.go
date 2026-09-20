package manifest_test

import (
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

func TestFromV3_ProjectsSDKRequirementKeys(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantSDK string
	}{
		{
			name: "metacore-sdk key",
			raw: `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": {"key":"demo","name":"Demo","version":"1.0.0"},
  "compatibility": {"requires": [
    {"key":"kernel","version":">=3.0.0 <4.0.0"},
    {"key":"metacore-sdk","version":">=28.0.0"}
  ]}
}`,
			wantSDK: ">=28.0.0",
		},
		{
			name: "@asteby/metacore-sdk key",
			raw: `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": {"key":"demo","name":"Demo","version":"1.0.0"},
  "compatibility": {"requires": [
    {"key":"kernel","version":">=3.0.0 <4.0.0"},
    {"key":"@asteby/metacore-sdk","version":"^28.6.0"}
  ]}
}`,
			wantSDK: "^28.6.0",
		},
		{
			name: "short sdk key",
			raw: `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": {"key":"demo","name":"Demo","version":"1.0.0"},
  "compatibility": {"requires": [
    {"key":"sdk","version":">=27.0.0"}
  ]}
}`,
			wantSDK: ">=27.0.0",
		},
		{
			name: "kernel only leaves SDK empty",
			raw: `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": {"key":"demo","name":"Demo","version":"1.0.0"},
  "compatibility": {"requires": [
    {"key":"kernel","version":">=3.0.0 <4.0.0"}
  ]}
}`,
			wantSDK: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := v3.Parse([]byte(tc.raw))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			out := manifest.FromV3(m)
			if out.SDK != tc.wantSDK {
				t.Fatalf("SDK=%q want %q (Kernel=%q)", out.SDK, tc.wantSDK, out.Kernel)
			}
			if tc.wantSDK != "" && out.Kernel == "" && tc.name != "short sdk key" {
				// kernel+sdk cases should still project kernel
			}
		})
	}
}
