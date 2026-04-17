// Package metadata exposes the TableMetadata / ModalMetadata shapes
// registered in modelbase to HTTP clients (typically the
// @asteby/metacore-runtime-react frontend).
//
// It is intentionally thin: the heavy lifting (column definitions, field
// definitions, option sources) lives in each app's ModelDefiner
// implementations, which register themselves via modelbase.Register. This
// package only wraps that registry with:
//
//   - a framework-agnostic Service (cacheable, transformable)
//   - a Fiber Handler that renders Service output using the standard
//     { "success": ..., "data": ... } envelope
//
// App-specific concerns — i18n localisation, org-settings overlays, addon
// column injection, fiscal-data nesting, per-branch filtering — are
// explicitly NOT in the kernel. Apps layer them on via TableTransformer /
// ModalTransformer registered on the Service.
//
// # Quick start
//
//	// 1. Apps register their models in init() functions (already done).
//	//    modelbase.Register("users", func() modelbase.ModelDefiner { ... })
//
//	// 2. Boot a Service and a Handler.
//	svc := metadata.New(metadata.Config{CacheTTL: 5 * time.Minute})
//	svc.WithTableTransformer(myapp.ApplyI18nTable)
//	svc.WithModalTransformer(myapp.ApplyI18nModal)
//
//	h := metadata.NewHandler(svc)
//	h.Mount(app.Group("/api/metadata"), authMW) // auth middleware optional
//
// # Routes mounted
//
//	GET /api/metadata/table/:model  -> TableMetadata
//	GET /api/metadata/modal/:model  -> ModalMetadata
//	GET /api/metadata/all           -> AllMetadata (tables + modals + version)
//
// # Cache semantics
//
// Config.CacheTTL controls both single-model and GetAll caches. After an
// admin toggles an org setting or installs an addon, call
// svc.InvalidateCache() (global) or svc.InvalidateModel(key) (single).
// GetAll always re-invalidates when any single-model call re-invalidates
// because it is stored under a reserved internal key.
//
// # Extensibility (Law 2)
//
// The transformer hooks are the escape hatch for app-specific logic that
// must not contaminate the kernel:
//
//	svc.WithTableTransformer(func(ctx context.Context, key string, t *modelbase.TableMetadata) error {
//	    // e.g. translate labels via go-i18n
//	    t.Title = myi18n.T(ctx, t.Title)
//	    return nil
//	})
//
// Transformers run in registration order and may mutate meta in place.
package metadata
