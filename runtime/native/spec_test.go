package native

import "testing"

func validSpec() Spec {
	return Spec{
		Entrypoint: "backend/connector",
		Scope:      ScopeInstance,
		Health:     HealthCheck{Path: "/health", Interval: "10s", Timeout: "2s"},
		Resources:  ResourceLimits{MemoryMB: 512, CPUQuotaMCPU: 500, PIDs: 128},
		Network:    NetworkPolicy{Egress: []string{"web.whatsapp.com:443"}},
		Secrets:    []SecretRef{{Handle: "whatsapp.session", Mount: "/run/secrets/session"}},
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
