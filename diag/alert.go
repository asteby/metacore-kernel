package diag

import (
	"context"
	"errors"
	"fmt"

	"github.com/asteby/metacore-kernel/incidents"
)

// IncidentsDomain es el dominio bajo el que se abren las incidencias.
const IncidentsDomain = "integrations"

// IncidentsAlerter abre una incidencia (paquete incidents: huella con contador,
// campana y evento) por cada alerta de diagnostico. La huella de la incidencia
// es la del grupo, asi que la repeticion solo sube el contador.
type IncidentsAlerter struct {
	Store *incidents.Store
}

// Alert implementa Alerter.
func (a IncidentsAlerter) Alert(ctx context.Context, al Alert) error {
	if a.Store == nil {
		return errors.New("diag: nil incidents store")
	}
	sev := incidents.SeverityMedium
	if al.Severity.rank() >= SeverityError.rank() {
		sev = incidents.SeverityHigh
	}
	title := fmt.Sprintf("Falla repetida en el addon %s", al.AddonKey)
	if al.Reason == "new_group" {
		title = fmt.Sprintf("Nueva falla en el addon %s", al.AddonKey)
	}
	_, _, err := a.Store.Open(ctx, incidents.Input{
		Org:          al.Org,
		Domain:       IncidentsDomain,
		Source:       al.AddonKey,
		Kind:         IncidentsDomain + ".addon_failure",
		Severity:     sev,
		Fingerprint:  "diag:" + al.Fingerprint,
		ResourceType: "addon_diagnostic_group",
		ResourceID:   al.GroupID.String(),
		Meta: map[string]string{
			"addon_key": al.AddonKey, "operation": al.Operation, "kind": string(al.Kind), "code": al.Code,
			"http_status": fmt.Sprint(al.HTTPStatus), "reason": al.Reason, "count": fmt.Sprint(al.Count),
		},
		Title:      title,
		ActionHint: "Revisa el diagnostico del addon (Diagnostico de addons) con el codigo y las propiedades indicadas.",
		Detail:     "codigo " + al.Code,
	})
	return err
}
