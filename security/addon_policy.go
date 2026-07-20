package security

// addon_policy.go holds THE rule for what an addon is authorised to do, derived
// from its manifest. It lives in the kernel, next to the gate that enforces it,
// so that every consumer asks the same question and gets the same answer:
//
//   - the ops wasm host, resolving a policy per invocation (Host.WithAddonCaps);
//   - the addons repo's manifestcheck, linting manifests in CI.
//
// Keeping it here is not tidiness. The bug this closes (ops#870) had a sibling:
// `secrets:read` passes v3 schema validation but Compile has no case for it, so
// a manifest could look correct and grant nothing. A rule reimplemented in a
// linter drifts from the rule the runtime enforces, and the drift is invisible
// until a guest is denied in production — or, worse, allowed.

import "github.com/asteby/metacore-kernel/manifest"

// CompileForAddon compiles an addon's manifest into the policy the runtime
// gates against, applying the two rules Compile alone does not know about.
//
// 1. `secrets:read <key>` is equivalent to `connector:read <key>`. Both mean
//    "this addon reads that connector's credentials". Addons declare
//    `secrets:read` because `connector:read` was missing from the v3 capability
//    enum until v0.80, so the declaration that the runtime gates on could not be
//    written; honouring both keeps already-published manifests working.
//
// 2. Declaring a connector in the manifest's own `connectors` block implies
//    permission to read it. No addon carries an explicit capability over its own
//    connector, for the same reason. This grant is TRANSITIONAL: defining a
//    connector ("this credential exists and is configured like so") and reading
//    it ("this addon sees those secrets") are different claims, and an addon may
//    define one for another to consume. The returned implicit slice names the
//    connectors that relied on it, so a host can log the reliance and a linter
//    can flag the manifest — and once it is empty across the catalogue, this
//    rule can be dropped so that a guest calling connector_get always says so in
//    its manifest.
//
// A wildcard target on either connector kind is REFUSED, not compiled: `*` is
// exactly the grant ops#870 removed from the host, and honouring it from inside
// a manifest would let an addon re-open the hole for itself. Refused targets are
// returned in refused so the caller can surface them.
//
// The addon's own `db:*` self-schema grants come from Compile, unchanged.
func CompileForAddon(addonKey string, m manifest.Manifest) (caps *Capabilities, implicit []string, refused []manifest.Capability) {
	declared := make([]manifest.Capability, 0, len(m.Capabilities)+len(m.Connectors))
	explicit := map[string]struct{}{}

	for _, c := range m.Capabilities {
		switch c.Kind {
		case CapConnectorRead, CapSecretsRead:
			if c.Target == "*" || c.Target == "" {
				refused = append(refused, c)
				continue
			}
			explicit[c.Target] = struct{}{}
			// Normalise onto the kind Compile actually consumes.
			declared = append(declared, manifest.Capability{
				Kind:   CapConnectorRead,
				Target: c.Target,
				Reason: c.Reason,
			})
		default:
			declared = append(declared, c)
		}
	}

	for _, cn := range m.Connectors {
		if cn.Key == "" || cn.Key == "*" {
			continue
		}
		if _, ok := explicit[cn.Key]; ok {
			continue
		}
		implicit = append(implicit, cn.Key)
		declared = append(declared, manifest.Capability{
			Kind:   CapConnectorRead,
			Target: cn.Key,
			Reason: "implicit: addon declares this connector in its own connectors block",
		})
	}

	return Compile(addonKey, declared), implicit, refused
}

// Capability kinds that authorise reading a connector's credentials.
// CapSecretsRead is the spelling published manifests use; CapConnectorRead is
// the one the gate consumes and the one new manifests should declare.
const (
	CapConnectorRead = "connector:read"
	CapSecretsRead   = "secrets:read"
)
