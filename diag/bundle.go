package diag

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Bundle arma el support bundle de una organizacion: grupos y las ultimas
// muestras desde since, re-saneados al salir. No incluye ajustes ni
// credenciales: el Store no los conoce.
func (h *Handler) Bundle(ctx context.Context, org uuid.UUID, since time.Time) (Bundle, error) {
	b := Bundle{GeneratedAt: h.opt.Now().UTC(), KernelVersion: h.opt.KernelVersion, Org: org, Issues: []Group{}, Events: []Event{}}
	groups, _, err := h.store.List(ctx, Filter{Org: org, Since: since, Limit: 200})
	if err != nil {
		return b, err
	}
	for _, g := range groups {
		b.Issues = append(b.Issues, resanitizeGroup(g))
		_, evs, err := h.store.Get(ctx, org, g.ID)
		if err != nil {
			return b, err
		}
		if len(evs) > 5 {
			evs = evs[:5]
		}
		b.Events = append(b.Events, evs...)
	}
	return b, nil
}

// resanitizeGroup pasa los campos de texto del grupo por la allowlist.
func resanitizeGroup(g Group) Group {
	d := Sanitize(Diagnostic{
		Org: g.OrgID, AddonKey: g.AddonKey, AddonVersion: g.AddonVersion, Operation: g.Operation,
		Kind: Kind(g.Kind), Code: g.Code, Severity: Severity(g.Severity), HTTPStatus: g.HTTPStatus,
	}, nil)
	g.AddonKey, g.AddonVersion, g.Operation = d.AddonKey, d.AddonVersion, d.Operation
	g.Kind, g.Code, g.Severity, g.HTTPStatus = string(d.Kind), d.Code, string(d.Severity), d.HTTPStatus
	return g
}
