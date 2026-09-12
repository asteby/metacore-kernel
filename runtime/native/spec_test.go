package native

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
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
		Control: Control{Protocol: ProtocolV1, Transport: TransportUnixHTTP},
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

func TestVerifyArtifactWithDownloadURLSkipsBundlePayload(t *testing.T) {
	a := Artifact{
		Path:        "backend/native/connector-linux-amd64",
		SBOM:        "backend/native/sbom.json",
		SHA256:      strings.Repeat("a", 64),
		DownloadURL: "https://cdn.example.com/connector-linux-amd64",
	}
	// Only the SBOM ships inside the bundle; the payload itself is fetched
	// out-of-band, so its absence here must not fail verification.
	files := map[string][]byte{a.SBOM: []byte(`{"spdxVersion":"SPDX-2.3"}`)}
	if err := VerifyArtifact(a, files); err != nil {
		t.Fatalf("download_url artifact should not require bundle-embedded payload: %v", err)
	}
	delete(files, a.SBOM)
	if err := VerifyArtifact(a, files); err == nil {
		t.Fatal("SBOM is still required inside the bundle even with download_url set")
	}
}

func TestVerifyDownloadedArtifactChecksDigest(t *testing.T) {
	payload := []byte("real downloaded binary")
	sum := sha256.Sum256(payload)
	a := Artifact{Path: "backend/native/connector", SHA256: hex.EncodeToString(sum[:]), DownloadURL: "https://cdn.example.com/x"}
	if err := VerifyDownloadedArtifact(a, payload); err != nil {
		t.Fatalf("verify downloaded: %v", err)
	}
	if err := VerifyDownloadedArtifact(a, []byte("tampered")); err == nil {
		t.Fatal("tampered downloaded payload must fail")
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

func TestSpecValidateAcceptsHTTPSDownloadURL(t *testing.T) {
	s := validSpec()
	s.Artifacts[0].DownloadURL = "https://cdn.example.com/connector-linux-amd64"
	if err := s.Validate(); err != nil {
		t.Fatalf("https download_url should validate: %v", err)
	}
}

func TestSpecValidateRejectsNonHTTPSDownloadURL(t *testing.T) {
	s := validSpec()
	s.Artifacts[0].DownloadURL = "http://cdn.example.com/connector-linux-amd64"
	if err := s.Validate(); err == nil {
		t.Fatal("plain http download_url must fail closed")
	}
}

func TestSpecValidateRejectsMalformedDownloadURL(t *testing.T) {
	s := validSpec()
	s.Artifacts[0].DownloadURL = "not-a-url"
	if err := s.Validate(); err == nil {
		t.Fatal("malformed download_url must fail closed")
	}
}
