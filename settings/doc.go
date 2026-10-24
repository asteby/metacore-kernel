// Package settings is the kernel's scoped store for NON-secret addon settings
// and the single contract that resolves their effective value.
//
// See docs/adr/0002-addon-settings-scoped-store.md. In short:
//
//   - Store persists what someone explicitly set, per (org, addon, scope),
//     with an optimistic-concurrency revision. Manifest defaults are never
//     written to it.
//   - Resolver computes the effective values with one explicit precedence:
//     manifest default < org value < branch override (only keys the manifest
//     declares with scope "branch").
//   - Secrets and write-only settings are NOT stored here (use
//     org_integrations); keys starting with "_" are reserved for the system.
//
// F1 is additive: nothing in the kernel reads or writes this store unless the
// host wires it (see wasm.Host.WithSettingsResolver and WasmResolver).
package settings
