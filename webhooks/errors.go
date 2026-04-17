package webhooks

import "errors"

var (
	ErrWebhookNotFound  = errors.New("webhook not found")
	ErrDeliveryNotFound = errors.New("delivery not found")
	ErrInvalidURL       = errors.New("invalid webhook URL")
	ErrNoEvents         = errors.New("webhook must subscribe to at least one event")
)
