// Package auth provides a reusable authentication module for Metacore apps
// built on Fiber + GORM. It is deliberately free of app-specific concerns
// (no X-Branch-ID, no hardcoded plans, no country lookup) so any host
// application can consume it without duplicating code.
//
// # Quick start
//
//	db := connectDB()
//
//	svc := auth.New(db, auth.Config{
//	    JWTSecret: []byte(os.Getenv("JWT_SECRET")),
//	    JWTIssuer: "myapp",
//	    JWTExpiry: 24 * time.Hour,
//	}).
//	    WithUserModel(func() modelbase.AuthUser { return &myapp.User{} }).
//	    WithOrgModel(func() modelbase.AuthOrg { return &myapp.Organization{} }).
//	    WithPostLoginHook(func(ctx context.Context, u modelbase.AuthUser, _ modelbase.AuthOrg, _ string) error {
//	        // e.g. load app-specific session state
//	        return nil
//	    })
//
//	h := auth.NewHandler(svc)
//	mw := auth.Middleware(auth.MiddlewareConfig{Secret: []byte(os.Getenv("JWT_SECRET"))})
//
//	h.Mount(app.Group("/api/auth"), mw)
//
// # Response shape
//
// Login and Register respond with:
//
//	{
//	  "success": true,
//	  "data": {
//	    "user":         { ... app-specific User JSON ... },
//	    "organization": { ... optional Org JSON ... },
//	    "token":        "eyJ...",
//	    "expires_at":   "2026-04-17T12:00:00Z"
//	  }
//	}
//
// This matches what @asteby/metacore-auth on the frontend already expects.
//
// # Custom / domain-specific JWT claims
//
// The default Claims struct (UserID, OrganizationID, Email, Role) covers most
// cases. When an app needs extra fields (e.g. Plan, Features, Audience), use
// the generic helpers:
//
//	type MarketplaceClaims struct {
//	    jwt.RegisteredClaims
//	    Plan     string   `json:"plan"`
//	    Features []string `json:"features"`
//	}
//
//	// Signing
//	signed, err := auth.GenerateTokenWithClaims(&mc, secret, 24*time.Hour)
//
//	// Validation
//	var out MarketplaceClaims
//	err = auth.ValidateTokenWithClaims(signed, secret, &out)
//
// The default GenerateToken/ValidateToken functions remain unchanged for full
// backward compatibility.
package auth
