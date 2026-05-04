// Package manifest defines the declarative contract every metacore addon
// ships and the kernel consumes. It is the single source of truth: the Go
// types in this package are mirrored by the TypeScript SDK via generated
// definitions, so any field added here surfaces to addon authors after a
// regen.
//
// # Shape
//
// A Manifest carries identity (Key, Name, Version, Kernel range), the data
// model (ModelDefinitions and Extensions), declarative UI hooks (Navigation,
// Settings, Actions, Tools), the addon's runtime selection (Backend, with
// "wasm" / "webhook" / "binary"), the federated frontend bundle (Frontend),
// scoped permissions (Capabilities) and provenance (Signature).
//
// # Validation
//
// Manifest.Validate performs a structural and semantic check before install:
// key/model/column name shapes, kernel-version range, default-literal
// whitelist, capability namespacing, wasm export consistency and — since
// the v2 column extension — visibility, widget and validation-rule sanity.
// It is side-effect free; callers can run it cheaply at any pipeline stage.
//
// # Column extension (v2)
//
// ColumnDef historically described only the DDL plane (name, type, default,
// unique, ref). Starting with this revision it also carries optional UI and
// server-validation metadata so a single column declaration drives the
// migration AND the table/modal rendering AND the input rules:
//
//   - Visibility — "table" | "modal" | "list" | "all" (empty = "all").
//   - Searchable — opts the column into the model's search index.
//   - Validation — pointer to ValidationRule (Regex, Min, Max, Custom).
//   - Widget     — UI input slug (text, email, select, datetime, …).
//
// Every new field is optional and the zero value preserves the previous
// behaviour, so manifests authored against older kernels still validate
// without modification. Consumers (dynamic schema, modelbase metadata, the
// TS SDK) are NOT updated in this revision; they will pick the new fields
// up incrementally in follow-up PRs.
//
// # Action triggers
//
// ActionDef historically described only the UI shell (Fields, Modal,
// Confirm) and relied on an implicit Hooks-map → webhook resolution at
// dispatch time. Starting with this revision it also carries an optional
// Trigger pointer (ActionTrigger) that makes the dispatch shape explicit:
//
//   - Type    — "wasm" | "webhook" | "noop".
//   - Export  — wasm export symbol; required when Type=wasm and MUST appear
//               in Backend.Exports so the wasm host can resolve it.
//   - RunInTx — invoke the wasm export inside the request DB transaction.
//
// Trigger is purely additive — manifests that omit it keep the legacy
// behaviour. Validate enforces the per-type contract (Export required for
// wasm; Export and RunInTx forbidden for webhook/noop because the hop
// escapes the request transaction). Consumers (bridge/actions.go,
// runtime/wasm) are NOT updated in this revision; they will pick the new
// field up incrementally in follow-up PRs.
//
// # Relations
//
// ModelDefinition also carries an optional Relations slice. Each entry is a
// RelationDef discriminated by Kind ("one_to_many" or "many_to_many"); the
// kernel uses these declarations to derive joins, eager-loading hints, REST
// sub-resources and SDK metadata. Like the column extension, the slice is
// purely additive — manifests that omit it keep the legacy "flat tables"
// behaviour. See the RelationDef godoc for the per-kind required fields and
// the validate.go enforcement (kind whitelist, identifier shapes, pivot
// presence rules, name uniqueness within a model).
package manifest
