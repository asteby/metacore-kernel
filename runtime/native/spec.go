// Package native defines the host-neutral contract for addon backends that
// cannot run inside WASM (for example a long-lived Node.js Baileys connector).
// It deliberately does not start processes: hosts provide a Supervisor that
// materialises this contract using systemd, containers or another sandbox.
package native

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
)

const (
	ScopeInstance     = "instance"
	ScopeInstallation = "installation"
	ProtocolV1        = "metacore.native/v1"
	TransportUnixHTTP = "unix_http"

	EnvSocket         = "METACORE_NATIVE_SOCKET"
	EnvTokenFile      = "METACORE_NATIVE_TOKEN_FILE"
	EnvInstallationID = "METACORE_INSTALLATION_ID"
	EnvOrganizationID = "METACORE_ORGANIZATION_ID"
	EnvAddonKey       = "METACORE_ADDON_KEY"

	// EnvEventSocket and EnvEventTokenFile provision the reverse channel: a
	// second unix_http socket, scoped to this same installation, that the
	// sidecar dials to POST /v1/events on the host. See events.go for the
	// wire contract. Both are assigned by the supervisor exactly like
	// EnvSocket/EnvTokenFile; manifests cannot choose or see these paths.
	EnvEventSocket    = "METACORE_NATIVE_EVENT_SOCKET"
	EnvEventTokenFile = "METACORE_NATIVE_EVENT_TOKEN_FILE"
)

// Spec is the portable, declarative process contract stored in a signed addon
// bundle. Entrypoint is always relative to the extracted artifact root. Secrets
// are opaque broker handles; raw secret values and arbitrary environment
// variables are intentionally absent from this contract.
type Spec struct {
	Entrypoint string         `json:"entrypoint"`
	Args       []string       `json:"args,omitempty"`
	Scope      string         `json:"scope"`
	Health     HealthCheck    `json:"health"`
	Resources  ResourceLimits `json:"resources"`
	Network    NetworkPolicy  `json:"network"`
	Secrets    []SecretRef    `json:"secrets,omitempty"`
	Artifacts  []Artifact     `json:"artifacts"`
	Control    Control        `json:"control"`
}

// Control pins the local host↔sidecar protocol. The supervisor assigns the
// socket and one-time token file; addon manifests cannot choose host paths.
type Control struct {
	Protocol  string `json:"protocol"`
	Transport string `json:"transport"`
}

// Artifact selects one content-addressed payload for a host platform. Path and
// SBOM are bundle-relative backend paths; SHA256 covers the payload bytes.
type Artifact struct {
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	SBOM   string `json:"sbom"`
}

type HealthCheck struct {
	Path     string `json:"path"`
	Interval string `json:"interval,omitempty"`
	Timeout  string `json:"timeout,omitempty"`
}

type ResourceLimits struct {
	MemoryMB     int `json:"memory_mb"`
	CPUQuotaMCPU int `json:"cpu_quota_mcpu"`
	PIDs         int `json:"pids"`
}

// NetworkPolicy is deny-by-default. Egress entries are hostname:port pairs or
// exact hostnames; wildcard and CIDR support require a future contract version.
type NetworkPolicy struct {
	Egress []string `json:"egress,omitempty"`
}

type SecretRef struct {
	Handle string `json:"handle"`
	Mount  string `json:"mount"`
}

func (s Spec) Validate() error {
	if s.Entrypoint == "" {
		return errors.New("native runtime: entrypoint is required")
	}
	clean := path.Clean(s.Entrypoint)
	if path.IsAbs(s.Entrypoint) || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || clean != s.Entrypoint {
		return fmt.Errorf("native runtime: entrypoint %q must be a clean relative artifact path", s.Entrypoint)
	}
	if s.Scope != ScopeInstance && s.Scope != ScopeInstallation {
		return fmt.Errorf("native runtime: scope %q must be %q or %q", s.Scope, ScopeInstance, ScopeInstallation)
	}
	if s.Control.Protocol != ProtocolV1 || s.Control.Transport != TransportUnixHTTP {
		return fmt.Errorf("native runtime: control must use protocol %q over %q", ProtocolV1, TransportUnixHTTP)
	}
	if s.Health.Path == "" || !strings.HasPrefix(s.Health.Path, "/") || strings.Contains(s.Health.Path, "..") {
		return errors.New("native runtime: health.path must be an absolute URL path without traversal")
	}
	if err := validateDuration("health.interval", s.Health.Interval); err != nil {
		return err
	}
	if err := validateDuration("health.timeout", s.Health.Timeout); err != nil {
		return err
	}
	if s.Resources.MemoryMB < 16 || s.Resources.MemoryMB > 32768 {
		return errors.New("native runtime: resources.memory_mb must be between 16 and 32768")
	}
	if s.Resources.CPUQuotaMCPU < 10 || s.Resources.CPUQuotaMCPU > 16000 {
		return errors.New("native runtime: resources.cpu_quota_mcpu must be between 10 and 16000")
	}
	if s.Resources.PIDs < 1 || s.Resources.PIDs > 4096 {
		return errors.New("native runtime: resources.pids must be between 1 and 4096")
	}
	seen := map[string]struct{}{}
	for i, secret := range s.Secrets {
		if secret.Handle == "" || secret.Mount == "" {
			return fmt.Errorf("native runtime: secrets[%d] requires handle and mount", i)
		}
		if !strings.HasPrefix(secret.Mount, "/run/secrets/") || strings.Contains(secret.Mount, "..") {
			return fmt.Errorf("native runtime: secrets[%d].mount must be below /run/secrets", i)
		}
		if _, ok := seen[secret.Mount]; ok {
			return fmt.Errorf("native runtime: duplicate secret mount %q", secret.Mount)
		}
		seen[secret.Mount] = struct{}{}
	}
	for i, target := range s.Network.Egress {
		if target == "" || strings.ContainsAny(target, "/* \t\n") {
			return fmt.Errorf("native runtime: network.egress[%d] must be an exact hostname or hostname:port", i)
		}
	}
	if len(s.Artifacts) == 0 {
		return errors.New("native runtime: at least one artifact is required")
	}
	seenPlatforms := map[string]struct{}{}
	for i, artifact := range s.Artifacts {
		platform := artifact.OS + "/" + artifact.Arch
		if artifact.OS == "" || artifact.Arch == "" || strings.ContainsAny(artifact.OS, " /\\") || strings.ContainsAny(artifact.Arch, " /\\") {
			return fmt.Errorf("native runtime: artifacts[%d] requires simple os and arch values", i)
		}
		if _, exists := seenPlatforms[platform]; exists {
			return fmt.Errorf("native runtime: duplicate artifact platform %q", platform)
		}
		seenPlatforms[platform] = struct{}{}
		if !cleanBackendPath(artifact.Path) || !cleanBackendPath(artifact.SBOM) {
			return fmt.Errorf("native runtime: artifacts[%d] path and sbom must be clean backend/ paths", i)
		}
		decoded, err := hex.DecodeString(artifact.SHA256)
		if err != nil || len(decoded) != sha256.Size {
			return fmt.Errorf("native runtime: artifacts[%d].sha256 must be 64 lowercase hexadecimal characters", i)
		}
		if artifact.SHA256 != strings.ToLower(artifact.SHA256) {
			return fmt.Errorf("native runtime: artifacts[%d].sha256 must be lowercase", i)
		}
	}
	return nil
}

func cleanBackendPath(value string) bool {
	return strings.HasPrefix(value, "backend/") && path.Clean(value) == value && !strings.Contains(value, "..")
}

// ArtifactFor returns the exact host-platform artifact without fallback.
func (s Spec) ArtifactFor(os, arch string) (Artifact, bool) {
	for _, artifact := range s.Artifacts {
		if artifact.OS == os && artifact.Arch == arch {
			return artifact, true
		}
	}
	return Artifact{}, false
}

// VerifyArtifact checks the selected payload against its manifest digest and
// requires the declared SBOM to be present in the same verified bundle.
func VerifyArtifact(artifact Artifact, files map[string][]byte) error {
	payload, ok := files[artifact.Path]
	if !ok {
		return fmt.Errorf("native runtime: artifact %q is missing from bundle", artifact.Path)
	}
	if _, ok := files[artifact.SBOM]; !ok {
		return fmt.Errorf("native runtime: SBOM %q is missing from bundle", artifact.SBOM)
	}
	sum := sha256.Sum256(payload)
	if hex.EncodeToString(sum[:]) != artifact.SHA256 {
		return fmt.Errorf("native runtime: artifact %q digest mismatch", artifact.Path)
	}
	return nil
}

func validateDuration(field, value string) error {
	if value == "" {
		return nil
	}
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 {
		return fmt.Errorf("native runtime: %s must be a positive duration", field)
	}
	return nil
}
