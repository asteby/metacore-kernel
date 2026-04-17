// Package webhooks provides outbound webhook management: CRUD, async dispatch
// with exponential-backoff retry, HMAC-signed deliveries, and delivery logs.
//
// The package reuses security.Signer / security.WebhookDispatcher to sign
// outbound calls with per-webhook secrets. Owner is polymorphic (OwnerType +
// OwnerID) so the same Service covers device-scoped, org-scoped, or any other
// scoping an app needs.
//
// Example:
//
//	svc := webhooks.New(webhooks.Config{DB: db, WorkerCount: 5})
//	svc.Start(ctx)
//	h := webhooks.NewHandler(svc, func(c *fiber.Ctx) (string, uuid.UUID) {
//	    return "organization", auth.GetOrganizationID(c)
//	})
//	h.Mount(api.Group("/webhooks"))
//
//	// App code triggers events:
//	svc.Trigger(ctx, "order.created", "organization", orgID, map[string]any{"id": orderID})
package webhooks
