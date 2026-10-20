package inbox

import (
	"context"
	"fmt"

	"github.com/asteby/metacore-kernel/payments/incidents"
	"github.com/google/uuid"
)

// mergeIdentity overlays the identity fields a Result carries onto the row,
// so incident fingerprints use the best-known provider reference.
func mergeIdentity(cur Notification, r Result) Notification {
	m := cur
	if r.Auth != "" {
		m.Auth = r.Auth
	}
	if r.AuthReason != "" {
		m.AuthReason = r.AuthReason
	}
	if r.ResourceKind != "" {
		m.ResourceKind = r.ResourceKind
	}
	if r.ResourceID != "" {
		m.ResourceID = r.ResourceID
	}
	if r.CheckoutRef != "" {
		m.CheckoutRef = r.CheckoutRef
	}
	m.Estado = r.Estado
	return m
}

// subject is what an incident fingerprint is scoped to: the payment resource
// when the notification names one, else the notification itself.
func subject(n Notification) string {
	if n.ResourceID != "" {
		return n.ResourceID
	}
	return n.ID.String()
}

// applyIncidents opens the incidents the merged outcome calls for and closes
// the ones it makes obsolete. It returns the incident to link to the row.
func (s *Store) applyIncidents(ctx context.Context, cur, m Notification, r Result) (*uuid.UUID, error) {
	inc := s.cfg.Incidents
	if inc == nil {
		return nil, nil
	}
	nid := cur.ID
	base := incidents.Input{
		Org: cur.OrganizationID, Provider: m.Provider, PaymentRef: m.ResourceID, OrderRef: m.CheckoutRef,
		Amount: r.Amount, Currency: r.Currency, NotificationID: &nid,
	}
	var want []incidents.Input
	add := func(kind, sev, fp, title, hint, detail string) {
		in := base
		in.Kind, in.Severity, in.Fingerprint, in.Title, in.ActionHint, in.Detail = kind, sev, fp, title, hint, detail
		want = append(want, in)
	}
	P, S := m.Provider, subject(m)

	switch {
	case m.Auth == AuthFirmaInvalida:
		add(incidents.KindSignatureInvalid, incidents.SeverityHigh, incidents.KindSignatureInvalid+":"+P,
			fmt.Sprintf("%s: la firma del webhook no coincide", P),
			"El proveedor firma con otra clave: actualiza el secreto del webhook del conector", m.AuthReason)
	case m.Auth == AuthFirmaOK && m.AuthReason == AuthReasonMatchedPrevious:
		add(incidents.KindSignatureInvalid, incidents.SeverityLow, "signature_previous_key:"+P,
			fmt.Sprintf("%s: el proveedor sigue firmando con la clave anterior", P),
			"Actualiza el secreto del webhook; la clave anterior caduca en 72 h", "")
	}
	switch m.Estado {
	case EstadoRequiereRevision:
		add(incidents.KindNotificationUnresolve, incidents.SeverityHigh, incidents.KindNotificationUnresolve+":"+P+":"+S,
			fmt.Sprintf("%s: aviso de pago sin resolver", P), "Revisa el aviso y pulsa «Reconciliar ahora»", r.Motivo)
	case EstadoReintentar:
		if m.Auth == AuthFirmaInvalida {
			add(incidents.KindNotificationUnresolve, incidents.SeverityHigh, incidents.KindNotificationUnresolve+":"+P+":"+S,
				fmt.Sprintf("%s: pago sin conciliar (firma inválida y el proveedor no respondió)", P),
				"Se reintenta solo; si persiste, pulsa «Reconciliar ahora»", r.Motivo)
		}
	case EstadoDeadLetter:
		sev := incidents.SeverityMedium
		if m.ResourceKind == "payment" || m.ResourceKind == "" {
			sev = incidents.SeverityHigh
		}
		add(incidents.KindDeadLetter, sev, incidents.KindDeadLetter+":"+P+":"+S,
			fmt.Sprintf("%s: aviso de pago agotó los reintentos", P), "Pulsa «Reprocesar» o «Reconciliar ahora»", r.Motivo)
	}
	if len(r.WriteFailures) > 0 {
		detail := ""
		for i, w := range r.WriteFailures {
			if i > 0 {
				detail += "; "
			}
			detail += w.Table + ": " + w.Cause
		}
		add(incidents.KindPaymentWriteFailed, incidents.SeverityHigh, incidents.KindPaymentWriteFailed+":"+P+":"+S,
			fmt.Sprintf("%s: no se pudo escribir el pago", P), "Se reintenta solo; revisa la causa", detail)
	}

	var link *uuid.UUID
	for _, in := range want {
		got, _, err := inc.Open(ctx, in)
		if err != nil {
			return nil, err
		}
		if link == nil {
			id := got.ID
			link = &id
		}
	}

	// Conditions the outcome resolves.
	switch m.Estado {
	case EstadoProcesada, EstadoReconciliadaAPI, EstadoIgnorada, EstadoDuplicada, EstadoResueltaManual:
		for _, fp := range []string{
			incidents.KindNotificationUnresolve + ":" + P + ":" + S,
			incidents.KindDeadLetter + ":" + P + ":" + S,
			incidents.KindPaymentWriteFailed + ":" + P + ":" + S,
		} {
			if _, err := inc.CloseByFingerprint(ctx, cur.OrganizationID, fp, "aviso conciliado"); err != nil {
				return nil, err
			}
		}
	}
	if m.Auth == AuthFirmaOK && m.AuthReason != AuthReasonMatchedPrevious {
		// The signature matches again: the secret was fixed.
		for _, fp := range []string{incidents.KindSignatureInvalid + ":" + P, "signature_previous_key:" + P} {
			if _, err := inc.CloseByFingerprint(ctx, cur.OrganizationID, fp, "la firma volvió a coincidir"); err != nil {
				return nil, err
			}
		}
	}
	return link, nil
}
