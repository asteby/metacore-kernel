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
//	h := webhooks.NewHandler(svc, func(c fiber.Ctx) (string, uuid.UUID) {
//	    return "organization", auth.GetOrganizationID(c)
//	})
//	h.Mount(api.Group("/webhooks"))
//
//	// App code triggers events:
//	svc.Trigger(ctx, "order.created", "organization", orgID, map[string]any{"id": orderID})
//
// # Declarative subscriptions (RFC 0003, outbound)
//
// A Webhook whose Declarative field is set is a declarative emission
// subscription: it is DATA, not code, and works for any event and any
// destination. A Webhook with Declarative == nil behaves exactly as before.
// Full guide: docs/webhooks-declarative.md. In short:
//
//   - Bridge feeds events from the in-process bus (events.Bus) into
//     the declarative subscriptions of the organization (pattern "customers.*").
//     Classic webhooks are never fed by the bridge.
//   - Declarative.Filter: all/any over eq, ne, in, exists on event.* / payload.*.
//   - Declarative.Transform: path substitution "{{payload.x}}", constants and
//     translation tables ("{{payload.channel | map:channels}}"); closed function
//     list lower, upper, trim, default, map. Validated AT SAVE against
//     Config.Schemas: a path that does not exist fails Create/Update. A transform
//     or payload_fields is required (the whole payload is never sent).
//   - Declarative.Signing: closed profile list of package webhooks/signing
//     (hmac_simple, hmac_canonical_v1, bearer, bearer+hmac_canonical_v1).
//     Secrets are references resolved by Config.Secrets FOR THE OWNER at send time; they are
//     never stored or logged.
//   - Idempotency key (template, default "{{event.id}}") is stored on the
//     delivery and is identical on every retry; the body is byte-identical and
//     X-Metacore-Body-SHA256 fingerprints it. Ordering "per_aggregate" sends the
//     events of one aggregate in sequence.
//   - Enabled is off by default: events are RETAINED (Held) and go out when
//     SetEnabled(true) (or Update) turns it on. The retained payload is personal
//     data: it is erased once delivered (PurgeDeadPayloads for the dead ones).
//   - Health: consecutive failures, auto-disable at Declarative.AutoDisableThreshold
//     (default 10, negative = never; events are then retained, Config.OnAutoDisable
//     alerts) and manual resume with SetEnabled(true).
//   - Responses: 2xx delivered; 4xx except 408/409/423/425/429 is permanent
//     (dead-letter, Requeue to repeat by hand); 5xx, 408, 409, 423, 425, 429 and
//     network errors retry with exponential backoff and +-20% jitter.
//   - Targets are validated at save and at connect with runtime/webhookout's
//     egress Policy (built on netguard): https, a domain (never an IP), no
//     internal name spaces or networks.
//
// Receiving side: webhooks/signing.Verifier (Verify(ctx, VerifyRequest) error)
// is the symmetric verifier the inbound package plugs in.
//
//	svc := webhooks.New(webhooks.Config{DB: db, Schemas: schemas, Secrets: resolver})
//	_ = webhooks.NewBridge(svc, webhooks.BridgeOptions{}).Attach(bus)
//	_ = svc.Create(ctx, &webhooks.Webhook{
//	    URL: "https://sandbox.7leguas.com.mx/api/channels/v1/customers", Events: webhooks.StringSlice{"customers.*"},
//	    Active: true, OwnerType: "organization", OwnerID: orgID,
//	    Declarative: &webhooks.Declarative{
//	        Transform: &webhooks.Transform{Body: map[string]any{"external_reference": "{{event.aggregate_id}}", "name": "{{payload.name}}"}},
//	        Signing:   &signing.Spec{Profile: signing.BearerHMACCanonicalV1, SecretRef: "v2.signing_secret", KeyRef: "v2.api_key"},
//	        Ordering:  webhooks.OrderingPerAggregate,
//	        Enabled:   true,
//	    },
//	})
//
// Schema: migration 0012 (additive; every new column is NULL/empty/false).
package webhooks
