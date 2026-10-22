// Package setupflow is the runtime contract of guided addon setup
// (manifest contributions.setup / contributions.health): the result shapes the
// addon's handlers return, an executor that runs a check on behalf of the
// acting user (timeout, backoff polling, de-duplication of concurrent runs) and
// a pure planner that turns a flow plus step results into "what can the user do
// next".
//
// The package is host-neutral. It never touches a database, an HTTP server or
// the wasm runtime: hosts implement Invoker (dispatch a SetupHandler as the
// user) and persist progress themselves (ops: addon_setup_progress). The
// manifest side lives in manifest/v3 (SetupFlow, SetupStep, SetupCheck,
// SetupPoll, HealthProbe).
//
// # Handler contract
//
// A check handler (kind=check) and a health handler return the kernel
// envelope {success, data, meta}; data is a CheckResult / HealthResult:
//
//	{"success":true,"data":{
//	   "status":"pending",
//	   "message_key":"mail.dns.propagating",
//	   "params":{"missing":2},
//	   "detail":"MX mail.example.com not visible yet",
//	   "retry_after_s":60,
//	   "fix":{"label_key":"mail.dns.apply","action":"apply_dns_cloudflare"}}}
//
// Status is pending | running | ok | warn | error | blocked. Only pending and
// running make the executor poll again; ok, warn, error and blocked settle the
// step (the user re-runs it, or a schedule does). A handler error
// ({success:false} or a transport failure) is NOT a status: it is transient and
// the executor retries it with the same backoff, bounded by MaxConsecutiveErrors.
//
// Handlers receive the acting user's principal, never a host-level one: a step
// can only do what that user may do (the manifest's permission is the gate).
package setupflow
