package security

// addon_policy.go compiles an addon's manifest into the policy the wasm runtime
// gates against. The RULE for which connectors a manifest authorises is not
// here — it is manifest.ConnectorAccessFor, deliberately placed in a leaf
// package so the addons repo's manifestcheck linter can ask the same question
// without pulling gorm/fiber/pgvector in through this package's signature.go.
// See the comment on that function for why one implementation matters.

import "github.com/asteby/metacore-kernel/manifest"

// CompileForAddon compiles an addon's manifest into its capability policy,
// applying manifest.ConnectorAccessFor for connector authorisation and passing
// every other declared capability through to Compile untouched (including the
// self-schema db:* grants Compile adds).
//
// The returned ConnectorAccess carries any refused wildcard declarations, so
// the host can log them and a linter can flag them without recomputing the
// rule. (Its Implicit field is retired and always empty.)
func CompileForAddon(addonKey string, m manifest.Manifest) (*Capabilities, manifest.ConnectorAccess) {
	access := manifest.ConnectorAccessFor(m)

	declared := make([]manifest.Capability, 0, len(m.Capabilities)+len(access.Granted))
	for _, c := range m.Capabilities {
		// Connector kinds are re-emitted below from the derived access, both to
		// normalise secrets:read onto the kind Compile consumes and to drop the
		// refused wildcards.
		if c.Kind == manifest.CapConnectorRead || c.Kind == manifest.CapSecretsRead {
			continue
		}
		declared = append(declared, c)
	}
	for _, key := range access.Granted {
		declared = append(declared, manifest.Capability{
			Kind:   manifest.CapConnectorRead,
			Target: key,
			Reason: "authorised by manifest.ConnectorAccessFor",
		})
	}

	return Compile(addonKey, declared), access
}
