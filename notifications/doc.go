// Package notifications provides a transport-agnostic notification queue with
// background workers, deduplication, retry-with-backoff, and crash recovery.
//
// The package owns the persistence layer (QueueEntry GORM model) and the
// worker lifecycle.  Apps plug in delivery transports by implementing
// ChannelHandler and registering them by channel name:
//
//	svc := notifications.New(notifications.Config{DB: db})
//	svc.Register("email", emailHandler)
//	svc.Register("webpush", webpushHandler)
//	svc.Register("sms", smsHandler)
//	svc.Start()
//	defer svc.Shutdown()
//
//	id, _ := svc.Enqueue(ctx, notifications.EnqueueRequest{
//	    OrganizationID: orgID,
//	    Source:         "billing",
//	    Event:          "invoice_paid",
//	    Channel:        "email",
//	    Target:         "user@example.com",
//	    Message:        "Your invoice has been paid.",
//	})
//
// Deduplication: every entry has a DedupKey (callers can supply or let the
// service hash event|channel|target|message).  Within DedupWindow another
// enqueue with the same key + organization is silently dropped.
//
// Retry: ChannelHandler.Deliver returns an error → entry stays "pending" with
// next_retry = now + base_delay*attempts (attempts < MaxRetries).  After
// MaxRetries the entry is marked "failed" permanently.
//
// Recovery: a poller sweeps the DB every PollInterval for pending entries
// whose next_retry has elapsed, so crashed workers don't lose deliveries.
//
// The package intentionally does NOT include rule evaluation or channel
// handlers — those live in the consuming app where domain knowledge belongs.
//
// A small generic Render() helper is provided for templates with the very
// common shape of {{var}} interpolation plus {{#var}}body{{/var}} sections.
// Apps that want a richer template language should bring their own renderer
// and pass the already-rendered string into EnqueueRequest.Message.
package notifications
