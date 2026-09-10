package dispatch

import (
	"context"
	"log/slog"
	"strings"
)

// model_key_guard.go detects a canonical event published under a model segment
// that is NOT the manifest ModelKey its subscribers are registered under.
//
// WHY THIS EXISTS. ops#1435: the legacy update publisher built the event name
// from the raw route parameter, so `PUT /api/data/sales_orders/:id` published
// `customers.sales_orders.updated` while every subscription is registered on
// `customers.SalesOrder.updated`. Nothing matched — and nothing complained:
// no delivery row, no outbox row, no error, HTTP 200. Cartera, invoicing,
// credit release and stock silently stopped firing for every update made
// through the table route. It stayed invisible for as long as it did precisely
// because a mis-named event is indistinguishable from an event nobody
// subscribes to.
//
// WHY A GUARD HERE AND NOT A STATIC CHECK. There are several publishers of
// CanonicalEvent (dynamic.publishCanonical, the two wasm data_mutate/data_batch
// paths, and the host's own legacy publisher), and the wasm ones take the model
// name FROM THE GUEST: validateDataMutateRequest only requires `model` to be
// non-empty, never that it is a ModelKey. So the fourth publisher is not code
// in this repo or in the host — it is every addon's wasm handler, which no
// static analysis of the host repos can see. The dispatcher is the one funnel
// every publisher passes through, which makes it the only place the rule can be
// enforced for all of them at once, including publishers that do not exist yet.
//
// THE INVARIANT THAT DOES **NOT** WORK — read before "simplifying" this.
// The obvious check is to compare the event name against the payload's `model`
// field and flag them when they disagree. That would NOT have caught ops#1435:
// the publisher used the SAME raw variable for both, so the two fields were
// perfectly consistent with each other and perfectly wrong. Self-consistency
// proves nothing. The name has to be resolved against the model registry — the
// only source that knows which spelling subscribers actually use.
//
// This guard WARNS and self-heals; it never rejects. A publish happens after
// the data has committed, so refusing it would trade a mis-named event for a
// lost one.

// canonicalizeEventName rewrites the model segment of `<addon>.<model>.<action>`
// to `resolved`. Names that are not three-segment canonical events are returned
// untouched.
func canonicalizeEventName(eventName, resolved string) string {
	parts := strings.Split(eventName, ".")
	if len(parts) != 3 {
		return eventName
	}
	parts[1] = resolved
	return strings.Join(parts, ".")
}

// checkModelKey applies the guard to one publication. It returns the event name
// routing should use — the canonical one when the publisher got it wrong, the
// original otherwise.
//
// Silent (returns eventName unchanged) when: no resolver is wired, the payload
// is not a canonical event, the name is not three-segment, or the registry does
// not know the model. An unknown model is deliberately left alone: a core GORM
// table or a domain event has no ModelKey to be measured against, and warning
// about it would drown the signal this guard exists to produce.
func (d *Dispatcher) checkModelKey(ctx context.Context, eventName string, ce canonicalEvent) string {
	if d.opts == nil || d.opts.modelKeyResolver == nil || ce.Model == "" {
		return eventName
	}
	parts := strings.Split(eventName, ".")
	if len(parts) != 3 {
		return eventName
	}
	resolved := d.opts.modelKeyResolver(ctx, ce.Model)
	if resolved == "" || resolved == ce.Model {
		return eventName
	}

	fixed := canonicalizeEventName(eventName, resolved)

	// Structured fields, not an interpolated sentence: this number is meant to
	// be grouped by model and by addon without parsing prose.
	d.logger.Warn("dispatch.model_key_mismatch",
		slog.String("addon", parts[0]),
		slog.String("raw_model", ce.Model),
		slog.String("resolved_model", resolved),
		slog.String("action", parts[2]),
		slog.String("raw_event", eventName),
		slog.String("fixed_event", fixed))

	if d.opts.onModelKeyMismatch != nil {
		d.opts.onModelKeyMismatch(ModelKeyMismatch{
			AddonKey:    parts[0],
			RawModel:    ce.Model,
			ResolvedKey: resolved,
			Action:      parts[2],
			RawEvent:    eventName,
			FixedEvent:  fixed,
		})
	}

	// The payload bytes are deliberately NOT rewritten. They are the evidence
	// of the defect — the record of what the publisher actually emitted — and
	// the occurrence fingerprint is computed over them. Routing is corrected;
	// the trail is preserved.
	return fixed
}
