package push

import "errors"

var (
	ErrSubscriptionNotFound = errors.New("push subscription not found")
	ErrInvalidKeys          = errors.New("invalid VAPID keys")
)
