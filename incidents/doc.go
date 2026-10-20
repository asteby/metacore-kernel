// Package incidents is the platform incident log: the visible record of
// everything that did not close by itself and that a person must see (an
// inbound payment notification with a bad signature, an approved payment
// without an order, a dead-lettered event, a credential about to expire, a
// stock alert). A log line is not a record; an incident is.
//
// Incidents are partitioned by DOMAIN (payments, inventory, integrations,
// events, updates, store): one table (platform_incidents), one API, one UX
// (bell, dashboard widget, health tab), and each domain's screen is a filter
// on it. Domain packages (payments/incidents) are thin facades that fix the
// domain and speak their own vocabulary.
//
// One open incident exists per (organization, domain, fingerprint): the same
// problem seen again increments Occurrences and refreshes LastSeenAt instead
// of opening another row, and the same fingerprint in another domain is a
// different incident. Lifecycle: abierta -> reconocida -> resuelta |
// auto_resuelta (a closed fingerprint that recurs opens a NEW incident).
//
// Opening (or escalating) an incident bridges to the rest of the host through
// Config: it publishes kernel.<domain>.incident_opened (emitter "kernel",
// reserved for the host by security.EmitPolicy), enqueues an in-app
// notification to the recipients the host resolves and calls the optional
// RuleFlag hook (the review queue). The incident row is the source of truth:
// a failing bridge is logged and never fails the open.
package incidents
