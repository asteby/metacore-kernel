// Package audit provides a universal, OPT-IN activity log (audit trail) for any
// app that embeds the metacore kernel. It captures every CRUD mutation routed
// through dynamic.Service by subscribing to the canonical events published on
// the kernel events.Bus, and persists a flat, app-agnostic Entry per mutation.
//
// # Why this lives in the kernel
//
// The audit trail is a generic mechanism, not domain logic — so per the
// ecosystem rule "the generic mechanism goes to the kernel, apps consume" it
// belongs here, where ops, link, hub and any future app get it for free instead
// of each re-implementing it.
//
// # Opt-in
//
// The package is inert until a host calls Wire exactly once at boot:
//
//	cancel, err := audit.Wire(bus, db)
//	if err != nil { return err }
//	defer cancel()
//
// Importing the package without calling Wire has ZERO effect: no subscription,
// no migration, no rows. Wire migrates the table, then subscribes a single
// wildcard ("*") handler that captures EVERY declarative model — present and
// future, including addons installed after boot — because the v0.52.0
// CanonicalEvent is self-describing (Model/Action/AddonKey/ActorID/CorrelationID
// are all populated by dynamic.publishCanonical).
//
// # Reading the trail
//
// A host mounts its own HTTP handler over the Query read service:
//
//	q := audit.NewQuery(db)
//	entries, total, err := q.List(ctx, nil, audit.QueryParams{
//	    OrganizationID: orgID,
//	    Model:          "products",
//	    Page:           1, PerPage: 50,
//	})
//
// Query is transport-agnostic — it returns rows and a total; the host owns the
// route, auth, and response envelope.
//
// # What the kernel does NOT do
//
// The kernel does not know the host's users table, so it stores only ActorID,
// never a denormalised name/email. A host that wants human-readable actor info
// installs an ActorResolver via WithActorResolver (backfills the in-memory Entry
// only) or denormalises at query time against its own users table.
package audit
