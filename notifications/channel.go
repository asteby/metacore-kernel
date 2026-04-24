package notifications

import (
	"context"
	"errors"
	"sync"
)

// ChannelHandler delivers a single QueueEntry through a specific transport
// (email, web push, WhatsApp, internal in-app, …).
//
// Returning a non-nil error triggers the retry/backoff machinery.  Return
// ErrPermanent (or wrap it) to skip retries and mark the entry failed
// immediately — useful for malformed targets the handler knows can never
// succeed.
type ChannelHandler interface {
	Deliver(ctx context.Context, entry *QueueEntry) error
}

// HandlerFunc adapts a plain function to ChannelHandler.
type HandlerFunc func(ctx context.Context, entry *QueueEntry) error

// Deliver implements ChannelHandler.
func (f HandlerFunc) Deliver(ctx context.Context, entry *QueueEntry) error {
	return f(ctx, entry)
}

// ErrPermanent, when returned (or wrapped) by a ChannelHandler, marks the
// entry as failed without scheduling further retries.  Useful for "address
// is invalid", "user opted out" and similar terminal errors.
var ErrPermanent = errors.New("notifications: permanent failure")

// IsPermanent reports whether err is or wraps ErrPermanent.
func IsPermanent(err error) bool {
	return err != nil && errors.Is(err, ErrPermanent)
}

// channelRegistry stores ChannelHandler instances keyed by channel name.
// Concurrent-safe so handlers can be registered after Service creation.
type channelRegistry struct {
	mu       sync.RWMutex
	handlers map[string]ChannelHandler
}

func newChannelRegistry() *channelRegistry {
	return &channelRegistry{handlers: map[string]ChannelHandler{}}
}

func (r *channelRegistry) set(channel string, h ChannelHandler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handlers[channel] = h
}

func (r *channelRegistry) get(channel string) (ChannelHandler, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	h, ok := r.handlers[channel]
	return h, ok
}
