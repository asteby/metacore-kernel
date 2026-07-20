package manifest

// connector_access.go holds THE rule for which connectors an addon's manifest
// authorises it to read. Every consumer must ask this one function:
//
//   - the wasm gate, via security.CompileForAddon (ops resolves a policy per
//     invocation with Host.WithAddonCaps);
//   - the addons repo's manifestcheck linter, flagging manifests in CI.
//
// It lives in `manifest`, not in `security`, for a boring but load-bearing
// reason: `manifest` is effectively a leaf (semver, jsonschema, x/text),
// while `security` transitively pulls gorm, fiber, fasthttp and pgvector
// through signature.go → bundle → dynamic → modelbase. A linter whose whole
// point is staying light cannot import that, and a rule its consumer cannot
// import gets reimplemented — which is precisely the failure this closes.
//
// The bug that motivated it (ops#870) had a sibling worth remembering:
// `secrets:read` passes v3 schema validation but has no case in
// security.Compile, so a manifest could look correct and grant nothing. Two
// implementations of one authorisation rule drift, and the drift stays
// invisible until a guest is denied in production — or wrongly allowed.

// Capability kinds that authorise reading a connector's credentials.
// CapSecretsRead is the spelling published manifests use; CapConnectorRead is
// the one the gate consumes and the one new manifests should declare.
const (
	CapConnectorRead = "connector:read"
	CapSecretsRead   = "secrets:read"
)

// ConnectorAccess is what a manifest says about the connectors its addon reads.
type ConnectorAccess struct {
	// Granted is every connector key the addon may read, deduped: explicit
	// declarations plus the implicit owner grant below.
	Granted []string

	// Implicit is the subset of Granted that was authorised ONLY by the addon
	// declaring the connector in its own `connectors` block, with no explicit
	// capability. This grant is TRANSITIONAL: defining a connector ("this
	// credential exists and is configured like so") and reading it ("this addon
	// sees those secrets") are different claims, and an addon may define one for
	// another to consume. It exists because no addon carries a capability over
	// its own connector — `connector:read` was missing from the v3 enum until
	// v0.80, so the declaration could not be written — and removing it before
	// the manifests migrate would stop fiscal_mexico stamping every CFDI 4.0 and
	// integration_github syncing, on the same deploy.
	//
	// Hosts should log these and linters should flag them. Once it is empty
	// across the catalogue, delete the implicit branch below and the rule
	// becomes: if a guest calls connector_get, its manifest says so.
	Implicit []string

	// Refused are declarations that were NOT honoured: a wildcard target on a
	// connector kind. `*` is exactly the host-wide grant ops#870 removed, and
	// honouring it from inside a manifest would let an addon re-open the hole
	// for itself. Surfaced so callers can report it rather than silently drop it.
	Refused []Capability
}

// ConnectorAccessFor derives the connector access a manifest declares.
//
// `secrets:read <key>` and `connector:read <key>` are equivalent: both mean
// "this addon reads that connector's credentials". Manifests declare the former
// because the latter was not a valid v3 kind when they were written.
//
// Callers holding a *v3.Manifest (anything coming out of v3.Parse, e.g. a
// linter) convert first — FromV3 maps capabilities and connector keys 1:1, so
// nothing this rule reads is lost:
//
//	access := manifest.ConnectorAccessFor(manifest.FromV3(m))
func ConnectorAccessFor(m Manifest) ConnectorAccess {
	var out ConnectorAccess
	explicit := map[string]struct{}{}

	for _, c := range m.Capabilities {
		if c.Kind != CapConnectorRead && c.Kind != CapSecretsRead {
			continue
		}
		if c.Target == "*" || c.Target == "" {
			out.Refused = append(out.Refused, c)
			continue
		}
		if _, dup := explicit[c.Target]; dup {
			continue
		}
		explicit[c.Target] = struct{}{}
		out.Granted = append(out.Granted, c.Target)
	}

	for _, cn := range m.Connectors {
		if cn.Key == "" || cn.Key == "*" {
			continue
		}
		if _, ok := explicit[cn.Key]; ok {
			continue
		}
		explicit[cn.Key] = struct{}{}
		out.Granted = append(out.Granted, cn.Key)
		out.Implicit = append(out.Implicit, cn.Key)
	}

	return out
}
