// Package inbox is the durable journal of inbound payment notifications
// (docs/DISENO-PAGOS-A-PRUEBA-DE-FALLOS, layer 0). A payment provider's
// webhook is written here BEFORE anything decides about it: before the
// signature is verified and before the addon's handler runs. If the write
// fails the host answers 503 so the provider retries; it never answers 2xx
// without having persisted (invariant I1).
//
// Lifecycle of a row (estado):
//
//	recibida -> procesando -> procesada | duplicada | ignorada |
//	            reconciliada_por_api | requiere_revision | reintentar |
//	            dead_letter
//	requiere_revision | dead_letter -> resuelta_manual
//
// Record inserts the row (input columns: headers, body, query, source IP,
// received_at) and Resolve writes the outcome. Input columns are append-only:
// on Postgres a trigger rejects any UPDATE of them (retention purge excepted,
// which may only blank headers and body_raw). Every outcome is also appended
// to commerce_payment_notification_attempts, one row per processing attempt.
//
// Secrets never reach the table: only an allowlist of headers is kept, the
// signature headers are stored in redacted shape (length plus four leading
// characters of each hash, never the hash) and the UI sees body_redacted,
// with personal data masked. body_raw is kept verbatim (up to MaxBodyBytes)
// so a notification can be replayed, and is reachable only through Raw.
//
// Failures that need a human open an incident (package incidents) when
// Config.Incidents is set; retryable ones are scheduled with
// Config.Backoff (default DefaultBackoff, 9 steps over about 45 hours) and
// announced to the host through Config.OnRetry, so a worker (an outbox kind,
// see RetryKind) can re-run them.
package inbox
