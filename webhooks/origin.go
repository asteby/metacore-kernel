package webhooks

import (
	"encoding/json"
	"math"
)

// Marcas anti-bucle del cuerpo saliente. Los nombres son los que lee `inbound`
// (inbound.OriginField / inbound.HopField); se repiten aqui para no acoplar
// los paquetes.
const (
	originField = "_origin"
	hopField    = "_hop"

	// CorrelationHeader es la cabecera que viaja con cada entrega declarativa.
	CorrelationHeader = "X-Correlation-Id"
)

// maxStampedHop evita desbordes con un `_hop` hostil en el payload.
const maxStampedHop = 1000

// subKey nombra la suscripcion en `_origin`: su Key, o el id si no tiene.
func subKey(w *Webhook) string {
	if w.Declarative != nil && w.Declarative.Key != "" {
		return w.Declarative.Key
	}
	return w.ID.String()
}

// correlationOf: la correlacion del evento o, si falta, el id del sobre.
func correlationOf(ev *Event) string {
	if ev.CorrelationID != "" && validIdempotencyKey(ev.CorrelationID) {
		return ev.CorrelationID
	}
	if validIdempotencyKey(ev.ID) {
		return ev.ID
	}
	return ""
}

// incomingHop lee el `_hop` del payload del evento (lo pone `inbound` en
// event.emit: el salto con el que llego la cadena). Ausente o invalido = 0.
func incomingHop(root map[string]any) int {
	p, _ := root["payload"].(map[string]any)
	switch v := p[hopField].(type) {
	case float64:
		if v >= 0 && v <= maxStampedHop && v == math.Trunc(v) {
			return int(v)
		}
	case json.Number:
		if i, err := v.Int64(); err == nil && i >= 0 && i <= maxStampedHop {
			return int(i)
		}
	}
	return 0
}

// stampOrigin pone `_origin` y `_hop` = hop+1 en la raiz del cuerpo, sin pisar
// lo que el mapeo ya haya definido. Exige hostID no vacio (lo valida el
// llamador).
func stampOrigin(body map[string]any, hostID, key string, hop int) {
	if _, has := body[originField]; !has {
		body[originField] = hostID + ":sub:" + key
	}
	if _, has := body[hopField]; !has {
		body[hopField] = int64(hop + 1)
	}
}
