// Package eventlog provides an org-scoped, persisted event log with
// SSE-friendly in-process subscriptions and cursor-based pagination.
//
// It is intentionally distinct from kernel/events (which implements the
// in-process addon Bus for non-persisted, capability-checked fan-out).
// Use eventlog when you need:
//
//   - durable, queryable history of domain events per organization,
//   - cursor pagination with Last-Event-ID / resume semantics,
//   - in-process live subscriptions for SSE endpoints,
//   - app-defined correlation tags (device_id, contact_id, …) without
//     coupling the kernel to the app's domain model.
//
// The Service owns an Event table keyed by (organization_id, sequence_num).
// Sequence numbers are assigned under a row-level lock so concurrent emits
// serialize per-org. Correlation data the app wants to filter on goes into
// Tags (map[string]string, stored as JSONB). Payload goes into Data
// (map[string]interface{}, stored as JSONB).
//
// Typical app integration pattern:
//
//	// App-side wrappers map domain IDs to generic tags:
//	func WithDeviceID(id uuid.UUID) eventlog.EventOption {
//	    return eventlog.WithTag("device_id", id.String())
//	}
//
// The Event struct exposed here is the base row shape. Apps that need
// extra columns or GORM metadata should embed or extend it (GORM honors
// embedded structs) — the Service operates exclusively on
// *eventlog.Event values for framework-level genericity.
package eventlog
