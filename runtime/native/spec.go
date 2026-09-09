// Package native defines the host-neutral contract for addon backends that
// cannot run inside WASM (for example a long-lived Node.js Baileys connector).
// It deliberately does not start processes: hosts provide a Supervisor that
// materialises this contract using systemd, containers or another sandbox.
package native

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
)

const (
	ScopeInstance     = "instance"
	ScopeInstallation = "installation"
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
