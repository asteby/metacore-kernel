package dispatch_test

import (
	"context"
	"testing"

	"github.com/asteby/metacore-kernel/events"
	"github.com/google/uuid"
)

// publishOccurrence publishes one canonical `updated` event for a row, with an
// explicit occurrence id and an `after` snapshot — the shape
// dynamic.publishCanonical puts on the bus. An empty occurrence id models the
// pre-occurrence_id kernel (and any host that builds the envelope by hand).
func publishOccurrence(t *testing.T, bus *events.Bus, orgID uuid.UUID, rowID, occurrenceID, status string) {
	t.Helper()
	payload := map[string]any{
		"id":        rowID,
		"model":     "SalesOrder",
		"action":    "updated",
		"addon_key": "customers",
		"after":     map[string]any{"id": rowID, "status": status},
	}
	if occurrenceID != "" {
		payload["occurrence_id"] = occurrenceID
	}
	if err := bus.Publish(context.Background(), "kernel", "customers.SalesOrder.updated", orgID, payload); err != nil {
		t.Fatalf("publish: %v", err)
	}
}

// TestUpdate_EachOccurrenceIsItsOwnDelivery is the regression for the bug that
// froze the receivables ledger: `<Model>.updated` was keyed on the mutated ROW
// id, so the FIRST update of a row consumed the only delivery it would ever
// get and every later update was dropped as a phantom "re-publish". A credit
// sale moving credit_pending -> delivered never reached its settled handler.
//
// Two DIFFERENT updates of the same row must be two deliveries.
func TestUpdate_EachOccurrenceIsItsOwnDelivery(t *testing.T) {
	h := newDomainHarness(t, "customers.SalesOrder.updated")
	org := uuid.New()
	const row = "so-1"

	publishOccurrence(t, h.bus, org, row, uuid.NewString(), "credit_pending")
	h.await(t, 1)

	publishOccurrence(t, h.bus, org, row, uuid.NewString(), "delivered")
	h.await(t, 1)

	if got := h.rec.count(); got != 2 {
		t.Fatalf("invocations for two updates of the same row = %d, want 2", got)
	}
}

// TestUpdate_RedeliveryOfOneOccurrenceStaysSingle is the guard against the fix
// overshooting. The outbox relay re-publishes the PERSISTED payload bytes, so a
// replayed publication carries the SAME occurrence id — it must still collapse
// to one delivery. Losing this would turn a "never delivered" bug into a
// "delivered twice" one, which in an ERP that stamps CFDI is worse.
func TestUpdate_RedeliveryOfOneOccurrenceStaysSingle(t *testing.T) {
	h := newDomainHarness(t, "customers.SalesOrder.updated")
	org := uuid.New()
	const row = "so-2"
	occurrence := uuid.NewString()

	publishOccurrence(t, h.bus, org, row, occurrence, "delivered")
	h.await(t, 1)

	// Same occurrence, replayed verbatim by the relay.
	publishOccurrence(t, h.bus, org, row, occurrence, "delivered")
	h.settle()

	if got := h.rec.count(); got != 1 {
		t.Fatalf("invocations after replay of one occurrence = %d, want 1 (idempotent)", got)
	}
}

// TestUpdate_LegacyEnvelopeFallsBackToPayloadFingerprint covers the canonical
// events that carry no occurrence_id: an unpublished outbox row written by an
// older kernel, or a host assembling the envelope itself. The payload
// fingerprint has to keep BOTH properties — distinct updates deliver twice, a
// byte-identical replay only once.
func TestUpdate_LegacyEnvelopeFallsBackToPayloadFingerprint(t *testing.T) {
	h := newDomainHarness(t, "customers.SalesOrder.updated")
	org := uuid.New()
	const row = "so-3"

	publishOccurrence(t, h.bus, org, row, "", "credit_pending")
	h.await(t, 1)

	publishOccurrence(t, h.bus, org, row, "", "credit_pending") // verbatim replay
	h.settle()
	if got := h.rec.count(); got != 1 {
		t.Fatalf("invocations after identical legacy replay = %d, want 1", got)
	}

	publishOccurrence(t, h.bus, org, row, "", "delivered") // a real second update
	h.await(t, 1)
	if got := h.rec.count(); got != 2 {
		t.Fatalf("invocations after distinct legacy update = %d, want 2", got)
	}
}

// TestUpdate_LegacyFingerprintCollapsesIdenticalBytes AFIRMA UN LÍMITE
// CONOCIDO, no una garantía: es el precio del fallback, escrito para que quede
// a la vista de quien lo toque.
//
// Sin `occurrence_id`, el discriminador es la huella sha256 del payload. Eso da
// idempotencia sobre el replay —que es lo que se busca— y a cambio hace que dos
// actualizaciones DISTINTAS con bytes IDÉNTICOS sean indistinguibles: la
// segunda se descarta como si fuera un re-envío de la primera.
//
// Que eso no muerda depende del PRODUCTOR, no del fallback:
// dynamic.publishCanonical mete `updated_at` en el `after`, así que dos updates
// reales difieren. Es una garantía del CONTENIDO del payload, no del diseño del
// dispatcher — y eso es lo que la hace frágil.
//
// ESTE CAMINO NO ES UN RINCÓN LEGACY: ES POR DONDE PASA HOY LA FACTURACIÓN.
// Hay tres publicadores de CanonicalEvent y sólo UNO estampa occurrence_id:
//
//  1. dynamic.Service.publishCanonical (kernel) — outbox sí, occurrence_id sí.
//     Es el que arregló #322, y el único que NO depende de esta huella.
//  2. DynamicHandler.publishCanonical (ops, fallback legacy) — bus directo,
//     sin outbox, sin occurrence_id.
//  3. Los guests wasm, vía runtime/wasm/datamutate.go y databatch.go — igual
//     que el 2.
//
// Los caminos 2 y 3 resuelven por ESTA huella. Y no son marginales: en ops,
// `updateDelegationGate` desvía a la rama legacy todo modelo con columna
// `fiscal_data` —por una razón legítima, que el Save del kernel pisa el JSONB
// en vez de mergearlo—, y `SalesOrder` e `Invoice` la tienen. O sea que la
// venta a crédito que factura sola y la cartera que se mueve en cada cobro
// viajan por acá. Medido en el sandbox: 66 filas de outbox sin occurrence_id
// contra 4 con él, en 48 horas.
//
// Un publicador de los caminos 2 o 3 que arme el envelope sin un campo que
// cambie —y ninguno está OBLIGADO a incluirlo— pierde la segunda entrega en
// silencio: no falla, no loguea, simplemente no llega. Estampar occurrence_id
// en esos dos publicadores vuelve la garantía incondicional y es el arreglo
// correcto; aun así este test sigue haciendo falta después, porque las filas
// de outbox ya escritas sin occurrence_id se siguen resolviendo por la huella.
//
// Si mañana alguien cambia el fingerprint (un contador, un timestamp de
// publicación, un uuid), ESTE TEST DEBE FALLAR: le está diciendo qué contrato
// está tocando. Lo correcto entonces es actualizarlo, no borrarlo — pero sabiendo
// que se cambia la idempotencia del replay, que es lo que el fingerprint compra.
func TestUpdate_LegacyFingerprintCollapsesIdenticalBytes(t *testing.T) {
	h := newDomainHarness(t, "customers.SalesOrder.updated")
	org := uuid.New()
	const row = "so-limit"

	// Dos publicaciones legacy con EL MISMO estado. En el mundo real serían dos
	// cambios distintos que dejan el mismo `after` (ida y vuelta a un valor
	// anterior, o un campo que el snapshot no incluye).
	publishOccurrence(t, h.bus, org, row, "", "credit_pending")
	h.await(t, 1)

	publishOccurrence(t, h.bus, org, row, "", "credit_pending")
	h.settle()

	if got := h.rec.count(); got != 1 {
		t.Fatalf("invocations = %d, want 1: el fallback ya no colapsa bytes idénticos. "+
			"Si el cambio es deliberado, actualizá este test Y revisá que el replay "+
			"de una misma publicación siga entregándose una sola vez "+
			"(TestUpdate_LegacyEnvelopeFallsBackToPayloadFingerprint).", got)
	}
}
