// Package bridge is the host-side glue that mounts the kernel alongside a
// host application's pre-existing addon system, services, and data model.
// It is the single seam where every host (link, ops, visor, hub, ...) wires
// concrete implementations of the kernel's interface contracts.
//
// # Stability
//
// As of KernelVersion 2.0.0, the public surface of this package is stable
// under semver:
//
//   - Config, Bridge, KernelVersion, ActionContext, ActionInterceptor,
//     ActionInterceptorRegistry, Tool, ToolStore, AgentResolver,
//     LegacyLifecycle, LegacyManifestSource, SecretResolver, Manifest.
//
// Adding fields to Config or methods to Bridge is a minor bump. Removing or
// changing the signature of any interface method is a major bump and
// requires the standard deprecation cycle (see ARCHITECTURE.md).
//
// # Contract
//
// The kernel does NOT import host model packages. Instead, hosts implement
// the interfaces in ports.go and pass them through Config. The bridge
// consumes the contract; bridge.go, actions.go, tools.go, and
// webhook_adapter.go are the consumers.
//
// *gorm.DB freely crosses the boundary because installer, dynamic, and
// navigation already depend on it.
//
// # Hosts that mount this bridge today
//
//   - link  — registers domain action interceptors and tool/agent adapters.
//   - ops   — registers ActionInterceptors for marketplace addons.
//   - visor — minimal adapters; no legacy manifests, no agents.
//
// New hosts should follow the same pattern: implement ports.go, hand the
// resulting Config to bridge.New, and never import host model packages from
// the kernel side.
package bridge
