// Package apitoken issues and authenticates scoped API tokens.
//
// Three kinds share one format, "<prefix>_" + 32 random bytes in unpadded
// base64url:
//
//   - publishable ("pk_") — org-scoped, safe to ship to a browser; may only
//     hold the scopes allowed by WithPublishableScopes (default: any
//     "<area>:read" plus "cart:write").
//   - secret ("sk_") — org-scoped, server to server; any scope.
//   - platform ("pt_") — NOT org-scoped: a multi-tenant renderer acting for
//     whichever organization owns the request's domain. The principal it
//     yields carries no organization; the host resolves it by domain (see
//     apiroute.DomainResolver). Issue it with uuid.Nil as the organization.
//
// Only the SHA-256 of a token is stored (TokenHash, unique). The plaintext is
// returned once by Issue / Rotate and never stored or logged; Prefix (its
// first 12 characters) is kept for display.
//
// Service (framework-agnostic):
//
//	svc := apitoken.NewService(apitoken.NewGormStore(db))
//	plain, tok, err := svc.Issue(ctx, orgID, "SSR", apitoken.KindSecret,
//	    []string{"catalog:read", "cart:write"}, nil, apitoken.WithCreatedBy(userID))
//	tok, err = svc.Authenticate(ctx, plain)          // status, expiry, last_used_at
//	plain2, tok2, err := svc.Rotate(ctx, tok.ID, 24*time.Hour) // old works 24h more
//	err = svc.Revoke(ctx, tok2.ID)
//
// Rotation keeps two tokens valid during the grace window: the new one
// records RotatedFromID and the old one's ExpiresAt is pulled in to
// now+grace (grace <= 0 revokes it at once).
//
// Fiber (optional, fiber.go): NewPrincipalResolver turns
// "Authorization: Bearer pk_|sk_|pt_..." into an apiroute.Principal (kind
// token) with per-token rate limiting (Limiter; MemoryLimiter by default),
// Middleware does the same for plain Fiber routes, and Handler is the staff
// CRUD (list / issue / rotate / revoke) for an organization's tokens.
package apitoken
