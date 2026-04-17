// Package push provides Web Push notification support: VAPID key management,
// per-user subscription storage, and typed send helpers.
//
// Consumers register a subscription (from the browser's PushManager.subscribe)
// and the service delivers notifications via webpush-go under the hood.
// Expired endpoints (HTTP 404/410) are auto-cleaned.
//
// Example:
//
//	svc := push.New(push.Config{
//	    DB: db,
//	    VAPIDPublic: os.Getenv("VAPID_PUBLIC_KEY"),
//	    VAPIDPrivate: os.Getenv("VAPID_PRIVATE_KEY"),
//	    VAPIDSubject: "mailto:ops@example.com",
//	})
//	h := push.NewHandler(svc, func(c *fiber.Ctx) uuid.UUID { return auth.GetUserID(c) })
//	h.Mount(api.Group("/push"))
package push
