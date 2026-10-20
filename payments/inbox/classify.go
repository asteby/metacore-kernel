package inbox

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/asteby/metacore-kernel/runtime/webhookin"
	"github.com/google/uuid"
)

// Reply is what the host answers the provider.
type Reply struct {
	Status  int
	Headers map[string]string
	Body    []byte
	// Default is true when the handler did not shape the answer and the host
	// keeps its usual 200 acknowledgement (Status is then 200).
	Default bool
}

// Structured is the optional machine-readable result a payments handler
// returns (inside the response envelope's body, or as the output's `data`):
//
//	{"auth":"firma_invalida","auth_reason":"signature_mismatch",
//	 "resource_kind":"payment","resource_id":"123","event_id":"...",
//	 "outcome":"reconciled","payment_id":"<uuid>","order_ref":"...",
//	 "write_failures":[{"table":"mp_payments","cause":"..."}]}
//
// outcome: processed | reconciled | duplicate | ignored | unresolved.
type Structured struct {
	Auth          string         `json:"auth"`
	AuthReason    string         `json:"auth_reason"`
	ResourceKind  string         `json:"resource_kind"`
	ResourceID    string         `json:"resource_id"`
	EventID       string         `json:"event_id"`
	Outcome       string         `json:"outcome"`
	PaymentID     string         `json:"payment_id"`
	OrderRef      string         `json:"order_ref"`
	Amount        string         `json:"amount"`
	Currency      string         `json:"currency"`
	WriteFailures []WriteFailure `json:"write_failures"`
}

// ParseStructured reads a Structured out of a handler body or output. Both
// the bare object and {"data": {...}} are accepted; anything else yields the
// zero value.
func ParseStructured(raw []byte) Structured {
	var s Structured
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return s
	}
	var wrap struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &wrap) == nil && len(wrap.Data) > 0 && wrap.Data[0] == '{' {
		_ = json.Unmarshal(wrap.Data, &s)
		return s
	}
	_ = json.Unmarshal(raw, &s)
	return s
}

// FromDispatch turns what webhookin.DispatchRequest returned into the Result
// to store and the Reply to send. It encodes the response policy of the
// design (§5.4):
//
//   - handled, duplicate or ignored: 200;
//   - signature invalid but reconciled by API: 200 (the payment is known);
//   - signature invalid and not reconciled: 401, so the provider retries
//     and the panel shows the failed delivery;
//   - transient error, write failure or success:false: 503;
//   - a handler 2xx that reports an invalid signature without reconciling
//     is forced to 401: an unresolved payment is never acknowledged.
func FromDispatch(resp webhookin.Response, err error) (Result, Reply) {
	if err != nil {
		switch {
		case errors.Is(err, webhookin.ErrSignatureMissing):
			return Result{Auth: AuthSinFirma, Estado: EstadoRequiereRevision, Motivo: "falta la firma", HTTPStatus: 401},
				jsonReply(http.StatusUnauthorized, "unauthorized")
		case errors.Is(err, webhookin.ErrSecretNotConfigured):
			return Result{Auth: AuthSinSecreto, Estado: EstadoRequiereRevision, Motivo: "el conector no tiene secreto de webhook", HTTPStatus: 401},
				jsonReply(http.StatusUnauthorized, "unauthorized")
		case errors.Is(err, webhookin.ErrSignatureInvalid):
			return Result{Auth: AuthFirmaInvalida, AuthReason: "signature_mismatch", Estado: EstadoRequiereRevision, Motivo: "firma inválida", HTTPStatus: 401},
				jsonReply(http.StatusUnauthorized, "unauthorized")
		case errors.Is(err, webhookin.ErrRouteNotFound):
			return Result{Estado: EstadoIgnorada, Motivo: "ruta desconocida", HTTPStatus: 404}, jsonReply(http.StatusNotFound, "not found")
		default:
			return Result{Estado: EstadoReintentar, Motivo: truncate("error: "+err.Error(), 300), HTTPStatus: 503},
				jsonReply(http.StatusServiceUnavailable, "temporarily unavailable")
		}
	}

	payload := resp.Output
	if resp.Envelope {
		payload = resp.Body
	}
	st := ParseStructured(payload)
	if st.ResourceID == "" && resp.Envelope && resp.Mapped {
		st = ParseStructured(resp.Output)
	}
	res := Result{
		Auth: st.Auth, AuthReason: st.AuthReason, ResourceKind: st.ResourceKind, ResourceID: st.ResourceID,
		EventID: st.EventID, CheckoutRef: st.OrderRef, WriteFailures: st.WriteFailures,
		Amount: st.Amount, Currency: st.Currency,
	}
	if id, perr := uuid.Parse(st.PaymentID); perr == nil {
		res.PaymentID = &id
	}
	// A safety-net mapping carries the handler's error text; an errEnvelope
	// "unauthorized" is a rejected signature.
	if resp.Mapped && res.Auth == "" {
		low := strings.ToLower(resp.Cause)
		if strings.Contains(low, "unauthorized") || strings.Contains(low, "signature") {
			res.Auth, res.AuthReason = AuthFirmaInvalida, "handler_rejected"
		}
	}

	status := http.StatusOK
	if resp.Envelope {
		status = resp.Status
	}
	reply := Reply{Status: status, Headers: resp.Headers, Body: resp.BodyBytes(), Default: !resp.Envelope}
	res.HTTPStatus = status

	switch {
	case len(res.WriteFailures) > 0:
		res.Estado, res.Motivo = EstadoReintentar, "no se pudo escribir el pago"
		reply = jsonReply(http.StatusServiceUnavailable, "temporarily unavailable")
	case status >= 500 || status == http.StatusTooManyRequests:
		res.Estado = EstadoReintentar
		res.Motivo = firstNonEmpty(resp.Cause, "el handler respondió "+http.StatusText(status))
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		if res.Auth == "" {
			res.Auth, res.AuthReason = AuthFirmaInvalida, "handler_rejected"
		}
		if st.Outcome == "reconciled" {
			res.Estado, res.Motivo = EstadoReconciliadaAPI, "conciliado por API"
			reply = Reply{Status: http.StatusOK, Body: []byte(`{"success":true}`), Headers: map[string]string{"Content-Type": "application/json"}}
			res.HTTPStatus = http.StatusOK
		} else {
			res.Estado, res.Motivo = EstadoRequiereRevision, "firma inválida, sin conciliar"
		}
	case status >= 400:
		res.Estado, res.Motivo = EstadoRequiereRevision, "el handler rechazó el aviso ("+http.StatusText(status)+")"
	default: // 2xx and 3xx
		switch st.Outcome {
		case "duplicate":
			res.Estado, res.Motivo = EstadoDuplicada, "duplicado"
		case "ignored":
			res.Estado, res.Motivo = EstadoIgnorada, "ignorado"
		case "reconciled":
			res.Estado, res.Motivo = EstadoReconciliadaAPI, "conciliado por API"
		case "unresolved":
			res.Estado, res.Motivo = EstadoRequiereRevision, "sin resolver"
		default:
			res.Estado = EstadoProcesada
		}
		if res.Auth == AuthFirmaInvalida && res.Estado != EstadoReconciliadaAPI && res.Estado != EstadoIgnorada && res.Estado != EstadoDuplicada {
			// Never acknowledge an unreconciled payment with a bad signature.
			res.Estado, res.Motivo = EstadoRequiereRevision, "firma inválida, sin conciliar"
			reply = jsonReply(http.StatusUnauthorized, "unauthorized")
			res.HTTPStatus = http.StatusUnauthorized
		}
	}
	if res.Estado == EstadoReintentar && reply.Status < 500 {
		reply = jsonReply(http.StatusServiceUnavailable, "temporarily unavailable")
		res.HTTPStatus = http.StatusServiceUnavailable
	}
	return res, reply
}

// StoreFailureReply is the answer when Record could not persist the request
// (invariant I1: never 2xx without having persisted).
func StoreFailureReply() Reply {
	r := jsonReply(http.StatusServiceUnavailable, "temporarily unavailable")
	r.Headers["Retry-After"] = "30"
	return r
}

func jsonReply(status int, msg string) Reply {
	b, _ := json.Marshal(map[string]any{"success": false, "error": msg})
	return Reply{Status: status, Body: b, Headers: map[string]string{"Content-Type": "application/json"}}
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
