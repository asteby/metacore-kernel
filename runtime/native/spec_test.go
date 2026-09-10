package native

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func validSpec() Spec {
	return Spec{
		Entrypoint: "backend/connector",
		Scope:      ScopeInstance,
		Health:     HealthCheck{Path: "/health", Interval: "10s", Timeout: "2s"},
		Resources:  ResourceLimits{MemoryMB: 512, CPUQuotaMCPU: 500, PIDs: 128},
		Network:    NetworkPolicy{Egress: []string{"web.whatsapp.com:443"}},
		Secrets:    []SecretRef{{Handle: "whatsapp.session", Mount: "/run/secrets/session"}},
		Artifacts: []Artifact{{OS: "linux", Arch: "amd64", Path: "backend/native/linux-amd64.tar.gz",
			SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", SBOM: "backend/native/linux-amd64.spdx.json"}},
	}
}

func TestVerifyArtifact(t *testing.T) {
	payload := []byte("signed connector")
	sum := sha256.Sum256(payload)
	a := Artifact{Path: "backend/native/connector.tar.gz", SBOM: "backend/native/sbom.json", SHA256: hex.EncodeToString(sum[:])}
	files := map[string][]byte{a.Path: payload, a.SBOM: []byte(`{"spdxVersion":"SPDX-2.3"}`)}
	if err := VerifyArtifact(a, files); err != nil {
		t.Fatalf("verify: %v", err)
	}
	files[a.Path] = []byte("tampered")
	if err := VerifyArtifact(a, files); err == nil {
		t.Fatal("tampered payload must fail")
	}
}

func TestSpecArtifactForRequiresExactPlatform(t *testing.T) {
	s := validSpec()
	if _, ok := s.ArtifactFor("linux", "amd64"); !ok {
		t.Fatal("expected exact artifact match")
	}
	if _, ok := s.ArtifactFor("linux", "arm64"); ok {
		t.Fatal("platform selection must not silently fall back")
	}
}

func TestSpecValidateAcceptsSandboxedService(t *testing.T) {
	if err := validSpec().Validate(); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}
}

func TestSpecValidateRejectsArtifactTraversal(t *testing.T) {
	s := validSpec()
	s.Entrypoint = "../bin/connector"
	if err := s.Validate(); err == nil {
		t.Fatal("entrypoint traversal must fail closed")
	}
}

func TestSpecValidateRejectsRawSecretMount(t *testing.T) {
	s := validSpec()
	s.Secrets[0].Mount = "/tmp/session"
	if err := s.Validate(); err == nil {
		t.Fatal("secret mounts outside /run/secrets must fail closed")
	}
}

func TestSpecValidateRejectsWildcardNetwork(t *testing.T) {
	s := validSpec()
	s.Network.Egress = []string{"*.example.com"}
	if err := s.Validate(); err == nil {
		t.Fatal("wildcard egress must fail closed in v1")
	}
}
