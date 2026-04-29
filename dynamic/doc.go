// Package dynamic implements the generic CRUD handler that renders arbitrary
// registered models as /admin/dynamic/:model endpoints, driven purely by
// modelbase metadata + GORM. It is the backend counterpart to the frontend
// DynamicTable / DynamicModal components.
//
// The goal is to keep every tenant-scoped resource (products, invoices,
// contacts, …) reachable without an app having to write yet another
// boilerplate CRUD handler: register the model with modelbase.Register,
// mount dynamic.Handler once, and the five verbs are live.
//
// App-specific behaviour (embedding pipelines, branch scoping, websocket
// fan-out, fiscal data nesting, …) is intentionally out of scope here.
// Apps layer those on via:
//
//   - TenantScoper         — swap the default WHERE organization_id = ? scope
//   - HookRegistry         — BeforeCreate/AfterCreate/… per model
//   - metadata transformers — mutate the TableMetadata consumed by the frontend
//   - handler wrappers      — bespoke routes under a different prefix
//
// # Quick start
//
//	svc := dynamic.New(dynamic.Config{
//	    DB:          db,
//	    Metadata:    metaSvc,
//	    Permissions: permSvc,   // optional — nil disables the check
//	    Query:       query.New(nil),
//	})
//
//	h := dynamic.NewHandler(svc, func(c fiber.Ctx) modelbase.AuthUser {
//	    // app-specific: pull the authenticated principal from c.Locals
//	    return c.Locals("user").(modelbase.AuthUser)
//	})
//	h.Mount(app.Group("/admin/dynamic"), authMW)
//
// # Response shape
//
//	GET /admin/dynamic/products
//	{ "success": true, "data": [ ... ], "meta": { total, page, per_page, last_page } }
//
//	POST /admin/dynamic/products
//	{ "success": true, "data": { ... created record ... } }
//
// All mutating endpoints require a permission.Capability of
// "<model>.<action>" when Config.Permissions is set; action ∈ create, read,
// update, delete. Apps bypass this by leaving Permissions nil (auth-only).
package dynamic
