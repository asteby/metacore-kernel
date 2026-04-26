// Package tool provides the runtime contract for addon-provided Tools —
// LLM-callable or programmatic functions declared in an addon's manifest.
//
// An addon's manifest.ToolDef is declarative (what the tool accepts, which
// endpoint answers, what it does). Package tool turns that declaration into
// runtime-usable artifacts shared across hosts:
//
//   - Tool interface: host-agnostic contract for a registered tool.
//   - Registry: thread-safe map of installed tools, keyed by addon+tool id.
//   - InputParamValidator: normalize + validate + format caller-supplied args
//     against ToolInputParam rules.
//   - HTTPDispatcher: executes a tool by POST-ing to the declared endpoint
//     with HMAC signing via security.WebhookDispatcher.
//
// Hosts import this package to stop reinventing the same three patterns on
// their own. Any addon installed on any host dispatches through the same
// Registry+Dispatcher.
package tool
