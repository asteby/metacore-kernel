// Package brand is the product-identity contract every metacore host
// exposes at a stable, public URL:
//
//	GET {origin}/api/brand        JSON manifest (spec, key, name, color, assets)
//	GET {origin}/api/brand/icon   square mark (SVG/PNG) — favicon, cards, <img>
//	GET {origin}/api/brand/logo   full identity (wordmark if the product has one,
//	                              otherwise the same bytes as icon)
//	GET {origin}/api/brand/og     1200×630 share card when the host ships one
//
// This is PRODUCT identity (Link, Ops, Pitsline, Hub), not tenant white-label.
// Tenant/org artwork stays on GET /api/platform/branding; a shop that uploads
// their own logo must never replace the product mark Hub and Google consume.
//
// The handler is net/http so Fiber, chi, and the appliance all mount the same
// bytes. Asset URLs in the manifest are root-relative (/api/brand/icon) so a
// reverse proxy that strips nothing keeps them valid, and <img src> works
// cross-origin without CORS.
package brand
