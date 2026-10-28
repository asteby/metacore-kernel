// Package brand is the product-identity contract every metacore host
// exposes at a stable, public URL:
//
//	GET {origin}/api/brand        JSON manifest (spec, key, name, color, assets)
//	GET {origin}/api/brand/icon   square mark (SVG/PNG) — favicon, cards, <img>
//	GET {origin}/api/brand/logo   full identity (wordmark if the product has one,
//	                              otherwise the same bytes as icon)
//	GET {origin}/api/brand/og     1200×630 share card when the host ships one
//
// Optional artwork, all additive (the manifest stays spec 1; a client that only
// reads key, name, color and assets.icon/logo/og is unaffected):
//
//	/api/brand/logo-dark      logo for dark backgrounds (falls back to logo, then icon)
//	/api/brand/favicon        ico/png/svg (falls back to icon)
//	/api/brand/maskable       PNG with the PWA maskable safe zone
//	/api/brand/monochrome     single-colour SVG (mask-icon, notifications)
//	/api/brand/animated       SVG animated declaratively (SMIL/CSS), never script
//	/api/brand/animated-dark  same, for dark backgrounds
//	/api/brand/assistant      avatar of the product's assistant
//
// plus theme tokens (primary, accent, background, foreground, dark variant,
// gradient, PWA colors), the assistant name and mascot skin, short_name,
// description, per-locale names and storage_namespace.
//
// # Serving
//
// Handler(src) serves a fixed Source exactly as it always did. NewHandler takes
// a Provider (for hosts that let operators override the brand per instance) and
// options: WithMaxAge, WithVersion (cheap change token for dynamic providers)
// and WithVersionedURLs ("version" in the manifest and ?v=<12 hex> on every
// asset URL, which lets browsers cache assets as immutable). Every response
// carries an ETag and honors If-None-Match (304); every asset is sent with
// "Content-Security-Policy: sandbox", nosniff and CORS *.
//
// # Brand packs
//
// A product brand is data: a folder with brand.json (schema in
// brand.schema.json, SchemaJSON) and its artwork. LoadFS(fs.FS) reads and
// validates it; Source.Validate/Check/MustValidate fail fast at boot (NewHandler
// itself never validates, to stay compatible with partial Sources); Merge
// applies a per-instance Overlay on top of a base. SVGs go through
// svgsafe.Validate: animated slots allow declarative animation, but a file with
// <script> (for example an animation exported with embedded scripting) is
// rejected by design: export a SMIL/CSS version for assets.animated, or leave it
// out and let the client animate the static mark. Video does not belong in the
// contract.
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
