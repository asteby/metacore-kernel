package manifest_test

import (
	"strings"
	"testing"

	"github.com/asteby/metacore-kernel/manifest"
	v3 "github.com/asteby/metacore-kernel/manifest/v3"
)

// runtimeManifestJSON exercises the addon-level pipeline-runtime primitives:
// connectors (with credentials), schedules and inbound webhooks. All additive
// v3 fields the strict schema + struct validator must accept on both surfaces.
const runtimeManifestJSON = `{
  "apiVersion": "asteby.com/v3",
  "kind": "Addon",
  "metadata": { "key": "integration_github", "name": "GitHub", "version": "1.0.0" },
  "compatibility": { "requires": [{ "key": "kernel", "version": ">=3.0.0 <4.0.0" }] },
  "connectors": [
    {
      "key": "github",
      "label": "GitHub",
      "auth": "oauth2",
      "credentials": [
        { "key": "access_token",   "type": "secret", "required": true },
        { "key": "repo",           "type": "string", "required": true },
        { "key": "webhook_secret", "type": "secret" }
      ]
    }
  ],
  "schedules": [
    { "key": "sync_issues", "every": "5m", "do": "wasm:sync_pull" }
  ],
  "webhooks": [
    {
      "key": "github_push",
      "path": "/webhooks/github",
      "verify": "hmac-sha256",
      "secret_ref": "github.webhook_secret",
      "do": "wasm:ingest_webhook"
    }
  ]
}`

func TestParse_Runtime_Accepted(t *testing.T) {
	m, err := v3.Parse([]byte(runtimeManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse rejected runtime manifest: %v", err)
	}
	if len(m.Connectors) != 1 || m.Connectors[0].Key != "github" || m.Connectors[0].Auth != "oauth2" {
		t.Fatalf("connectors = %+v", m.Connectors)
	}
	if len(m.Connectors[0].Credentials) != 3 || m.Connectors[0].Credentials[0].Type != "secret" {
		t.Fatalf("credentials = %+v", m.Connectors[0].Credentials)
	}
	if len(m.Schedules) != 1 || m.Schedules[0].Every != "5m" || m.Schedules[0].Do != "wasm:sync_pull" {
		t.Fatalf("schedules = %+v", m.Schedules)
	}
	if len(m.Webhooks) != 1 || m.Webhooks[0].SecretRef != "github.webhook_secret" {
		t.Fatalf("webhooks = %+v", m.Webhooks)
	}
}

func TestFromV3_Runtime_Mapped(t *testing.T) {
	m, err := v3.Parse([]byte(runtimeManifestJSON))
	if err != nil {
		t.Fatalf("v3.Parse: %v", err)
	}
	out := manifest.FromV3(m)
	if len(out.Connectors) != 1 || out.Connectors[0].Key != "github" {
		t.Fatalf("mapped connectors = %+v", out.Connectors)
	}
	// The "secret"-typed credential maps to Secret=true so the host encrypts it.
	cred := out.Connectors[0].Credentials[0]
	if cred.Key != "access_token" || !cred.Secret {
		t.Fatalf("mapped credential = %+v, want access_token secret=true", cred)
	}
	if len(out.Schedules) != 1 || out.Schedules[0].Do != "wasm:sync_pull" {
		t.Fatalf("mapped schedules = %+v", out.Schedules)
	}
	if len(out.Webhooks) != 1 || out.Webhooks[0].Verify != "hmac-sha256" {
		t.Fatalf("mapped webhooks = %+v", out.Webhooks)
	}
	// Strict (install-surface) validation must also accept it.
	if err := out.Validate("3.5.0"); err != nil {
		t.Fatalf("strict manifest.Validate rejected mapped runtime manifest: %v", err)
	}
}

func TestValidate_Runtime_Rejections(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(string) string
		want   string
	}{
		{
			name:   "schedule bad duration",
			mutate: func(s string) string { return strings.Replace(s, `"every": "5m"`, `"every": "banana"`, 1) },
			want:   "every",
		},
		{
			name:   "schedule bad do prefix",
			mutate: func(s string) string { return strings.Replace(s, `"do": "wasm:sync_pull"`, `"do": "rpc:sync_pull"`, 1) },
			want:   "do",
		},
		{
			name:   "webhook verify without secret_ref",
			mutate: func(s string) string { return strings.Replace(s, `"secret_ref": "github.webhook_secret",`, ``, 1) },
			want:   "secret_ref",
		},
		{
			name:   "webhook secret_ref unknown connector",
			mutate: func(s string) string { return strings.Replace(s, `"github.webhook_secret"`, `"slack.signing_secret"`, 1) },
			want:   "connector",
		},
		{
			name:   "webhook secret_ref unknown credential",
			mutate: func(s string) string { return strings.Replace(s, `"github.webhook_secret"`, `"github.nope"`, 1) },
			want:   "credential",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := tc.mutate(runtimeManifestJSON)
			err := v3.Validate([]byte(raw))
			if err == nil {
				t.Fatalf("expected validation error mentioning %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}
