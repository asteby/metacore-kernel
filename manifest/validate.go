package manifest

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/Masterminds/semver/v3"
)

var (
	keyRe    = regexp.MustCompile(`^[a-z][a-z0-9_]{1,63}$`)
	modelRe  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	columnRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)
	// defaultRe allows only safe DDL DEFAULT expressions:
	//   numeric literal:   42 | 42.5 | -3
	//   quoted string:     'pending' (no embedded quotes or semicolons)
	//   builtin function:  now() | gen_random_uuid() | uuid_generate_v4() | true | false | null
	defaultRe = regexp.MustCompile(
		`^(` +
			`-?\d+(\.\d+)?` + // numeric
			`|'[^'";\\]*'` + // simple quoted string
			`|now\(\)|gen_random_uuid\(\)|uuid_generate_v4\(\)|current_timestamp` +
			`|true|false|null` +
			`)$`)
)

// Validate performs a full structural + semantic check of the manifest.
// It is cheap and side-effect free; callers should run it before install.
func (m *Manifest) Validate(kernelVersion string) error {
	if m == nil {
		return fmt.Errorf("manifest: nil")
	}
	if !keyRe.MatchString(m.Key) {
		return fmt.Errorf("manifest: invalid key %q", m.Key)
	}
	if strings.TrimSpace(m.Name) == "" {
		return fmt.Errorf("manifest: name required")
	}
	if _, err := semver.NewVersion(m.Version); err != nil {
		return fmt.Errorf("manifest: version %q is not semver: %w", m.Version, err)
	}
	if err := m.checkKernelRange(kernelVersion); err != nil {
		return err
	}
	for i, md := range m.ModelDefinitions {
		if !modelRe.MatchString(md.TableName) {
			return fmt.Errorf("manifest.model_definitions[%d]: invalid table_name %q", i, md.TableName)
		}
		if md.ModelKey == "" {
			return fmt.Errorf("manifest.model_definitions[%d]: model_key required", i)
		}
		if len(md.Columns) == 0 {
			return fmt.Errorf("manifest.model_definitions[%d]: columns required", i)
		}
		for j, col := range md.Columns {
			// Block SQL injection via column name — the DDL generator uses
			// %q which does not escape embedded quotes per Postgres rules.
			if !columnRe.MatchString(col.Name) {
				return fmt.Errorf("manifest.model_definitions[%d].columns[%d]: invalid name %q", i, j, col.Name)
			}
			// Default goes raw into `DEFAULT <expr>` — whitelist literals
			// across the union type (string | number | bool | nil).
			if _, ok := DefaultLiteral(col.Default); !ok {
				return fmt.Errorf("manifest.model_definitions[%d].columns[%d].default: %v not allowed (use numeric, 'quoted' literal, now(), gen_random_uuid(), true, false, null)", i, j, col.Default)
			}
		}
	}
	for i, c := range m.Capabilities {
		if !strings.Contains(c.Kind, ":") {
			return fmt.Errorf("manifest.capabilities[%d]: kind must be namespaced (e.g. db:read)", i)
		}
		if c.Target == "" {
			return fmt.Errorf("manifest.capabilities[%d]: target required", i)
		}
		// Bare `*` would grant access to everything — including link-local
		// metadata addresses (169.254.169.254), loopback, and private
		// ranges. Require a concrete host segment for egress permissions.
		if c.Kind == "http:fetch" {
			if c.Target == "*" || c.Target == "*.*" || strings.HasPrefix(c.Target, "*.") && !strings.Contains(strings.TrimPrefix(c.Target, "*."), ".") {
				return fmt.Errorf("manifest.capabilities[%d].target: %q is too broad for http:fetch (require a concrete TLD like api.example.com or *.example.com)", i, c.Target)
			}
		}
		if c.Target == "*" && (c.Kind == "db:read" || c.Kind == "db:write") {
			return fmt.Errorf("manifest.capabilities[%d].target: wildcard %q not allowed for %s — declare explicit model names", i, c.Target, c.Kind)
		}
	}
	if err := m.validateBackend(); err != nil {
		return err
	}
	if m.Frontend != nil {
		switch m.Frontend.Format {
		case "federation", "script", "":
			// ok
		default:
			return fmt.Errorf("manifest.frontend.format: unknown %q", m.Frontend.Format)
		}
	}
	return nil
}

// validateBackend enforces the runtime whitelist and — for wasm — that each
// dispatchable hook resolves to an exported function name. Keeping the check
// here (not in the wasm runtime) means validation stays side-effect free and
// catches misconfigured manifests before we even load any bytes.
func (m *Manifest) validateBackend() error {
	if m.Backend == nil {
		return nil
	}
	switch m.Backend.Runtime {
	case "webhook", "wasm", "binary":
		// ok
	default:
		return fmt.Errorf("manifest.backend.runtime: unknown %q (want webhook|wasm|binary)", m.Backend.Runtime)
	}
	if m.Backend.Runtime == "wasm" {
		if strings.TrimSpace(m.Backend.Entry) == "" {
			return fmt.Errorf("manifest.backend.entry: required when runtime=wasm")
		}
		exports := make(map[string]struct{}, len(m.Backend.Exports))
		for _, e := range m.Backend.Exports {
			exports[e] = struct{}{}
		}
		for hookKey := range m.Hooks {
			// hookKey format: "<model>::<action>" — the action half must be
			// exported so the wasm host can resolve it at dispatch time.
			parts := strings.SplitN(hookKey, "::", 2)
			if len(parts) != 2 {
				continue
			}
			action := parts[1]
			if _, ok := exports[action]; !ok {
				return fmt.Errorf("manifest.hooks[%q]: action %q is not listed in backend.exports", hookKey, action)
			}
		}
	}
	return nil
}

func (m *Manifest) checkKernelRange(kernelVersion string) error {
	if m.Kernel == "" {
		return nil // legacy addon, no constraint
	}
	constraint, err := semver.NewConstraint(m.Kernel)
	if err != nil {
		return fmt.Errorf("manifest.kernel: invalid range %q: %w", m.Kernel, err)
	}
	kv, err := semver.NewVersion(kernelVersion)
	if err != nil {
		return fmt.Errorf("kernel version %q is not semver: %w", kernelVersion, err)
	}
	if !constraint.Check(kv) {
		return fmt.Errorf("manifest.kernel: host %s does not satisfy %s", kernelVersion, m.Kernel)
	}
	return nil
}
