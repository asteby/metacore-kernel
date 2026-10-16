// Package apiroute is the kernel-side gateway for addon HTTP API routes (the
// v3 `contributions.api` block). It is the request/response counterpart of
// runtime/webhookin: where an inbound webhook only hands a POST body to the
// addon and gets an error back, an API route gives the handler the method,
// path parameters, query, an allowlist of request headers and the resolved
// principal, and lets it answer with a real status, an allowlist of response
// headers and a body — cacheable when the route says so.
//
// The pieces, all framework-agnostic except the optional Fiber adapter:
//
//   - Router — the route table shared by every installed addon. Register
//     replaces an addon's routes atomically and refuses a method + path shape
//     another addon already owns; Match resolves a request with deterministic
//     precedence (static segment > :param > *wildcard, left to right).
//   - Request / Response — the JSON envelopes exchanged with the handler.
//     DecodeResponse parses what a WASM export returned.
//   - Dispatcher — the seam the host plugs its runtime into (wasm, compiled).
//   - Gateway — matches, authorizes (auth kind + scopes), invokes and
//     post-processes: status validation, response-header allowlist,
//     Cache-Control derived from the route's cache policy, weak ETag and
//     If-None-Match → 304, request body size limit.
//   - ErrorResponse — maps the typed errors to {"error":{"code","message"}}.
//   - Handler (fiber.go) — mounts the gateway under a prefix given a
//     PrincipalResolver (API token, customer session, staff JWT, domain).
//
// Wiring, in a host:
//
//	router := apiroute.NewRouter()
//	for _, inst := range installed {
//	    if err := router.Register(inst.Key, apiroute.RoutesFromManifest(inst.Manifest.APIRoutes)); err != nil {
//	        return err // a conflicting route: refuse the install
//	    }
//	}
//	gw := apiroute.NewGateway(router, myWasmDispatcher,
//	    apiroute.WithAddonEnabled(isInstalledForOrg))
//	apiroute.NewHandler(gw, resolvePrincipal).Mount(app.Group("/api/gw"))
//
// A WASM dispatcher marshals the Request, invokes the export named after
// "wasm:" for Request.Addon, and returns DecodeResponse(out).
//
// Back-compat: hosts that never build a Gateway, and addons without an `api`
// block, never reach this package.
package apiroute
