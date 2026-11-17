package webhooks

import "errors"

var (
	ErrWebhookNotFound  = errors.New("webhook not found")
	ErrDeliveryNotFound = errors.New("delivery not found")
	ErrInvalidURL       = errors.New("invalid webhook URL")
	ErrNoEvents         = errors.New("webhook must subscribe to at least one event")

	// Suscripciones declarativas (RFC 0003). Todas se devuelven AL GUARDAR,
	// salvo ErrPayloadPurged (al repetir una entrega).
	ErrInvalidDeclarative = errors.New("webhooks: invalid declarative subscription")
	ErrUnknownPath        = errors.New("webhooks: unknown path")
	ErrNoSchema           = errors.New("webhooks: no schema for event")
	ErrBlockedTarget      = errors.New("webhooks: target not allowed")
	ErrNoSecretResolver   = errors.New("webhooks: signing profile requires Config.Secrets (SecretResolver)")
	ErrPayloadPurged      = errors.New("webhooks: delivery payload was purged after delivery")
	ErrNotDeclarative     = errors.New("webhooks: webhook is not a declarative subscription")
	ErrNotDead            = errors.New("webhooks: delivery is not dead-lettered (delivered, queued, retained or in flight)")
)
