// Package diag es la capacidad de plataforma "Diagnostico de addons": cuando
// un addon falla (trap, timeout, 400 de un proveedor, sobre de error, fallo de
// capacidad), el kernel deja un diagnostico SANEADO que un administrador puede
// ver sin acceso al servidor. Ver docs/adr/0003-addon-diagnostics.md.
//
// Reglas de oro:
//
//   - Sanitize es la unica puerta de escritura: el Store solo se alimenta desde
//     el Recorder y este sanea siempre. Ningun addon puede saltarsela.
//   - Allowlist por campo y nunca se extraen VALORES: del cuerpo de error de un
//     proveedor solo salen codigos, rutas de campo y un esqueleto de claves/tipos.
//   - Nada es especifico de un addon ni de un proveedor.
//   - Reversible: KERNEL_DIAGNOSTICS=off apaga la captura y la escritura.
package diag

import (
	"time"

	"github.com/google/uuid"
)

// Kind clasifica el origen del fallo.
type Kind string

const (
	KindRuntime    Kind = "runtime"    // trap, timeout, pool ocupado, memoria, panic
	KindHTTP       Kind = "http"       // http_request/http_fetch >=400 o transporte, que termino en fallo
	KindEnvelope   Kind = "envelope"   // el guest devolvio {success:false,error:{...}}
	KindCapability Kind = "capability" // capability.Executor: timeout, panic, invalid_output, error del proveedor
	KindWebhook    Kind = "webhook"
	KindEvent      Kind = "event"
	KindNative     Kind = "native"
)

var validKinds = map[Kind]bool{
	KindRuntime: true, KindHTTP: true, KindEnvelope: true, KindCapability: true,
	KindWebhook: true, KindEvent: true, KindNative: true,
}

// Severity del diagnostico.
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityError    Severity = "error"
	SeverityCritical Severity = "critical"
)

func (s Severity) rank() int {
	switch s {
	case SeverityInfo:
		return 0
	case SeverityWarning:
		return 1
	case SeverityError:
		return 2
	case SeverityCritical:
		return 3
	}
	return 2
}

// Status de un grupo (issue).
const (
	StatusOpen     = "open"
	StatusMuted    = "muted"
	StatusResolved = "resolved"
)

// Subject enlaza el diagnostico con la entidad de negocio afectada sin
// duplicar sus datos (tipo + id opacos).
type Subject struct {
	Type string `json:"type,omitempty"`
	ID   string `json:"id,omitempty"`
}

// Diagnostic es un fallo de addon. Todos sus campos pasan por Sanitize antes de
// persistirse; construirlo a mano es seguro precisamente por eso.
type Diagnostic struct {
	Org           uuid.UUID `json:"org"`
	AddonKey      string    `json:"addon_key"`
	AddonVersion  string    `json:"addon_version,omitempty"`
	Kind          Kind      `json:"kind"`
	Operation     string    `json:"operation,omitempty"`
	CorrelationID string    `json:"correlation_id,omitempty"`
	InvocationID  string    `json:"invocation_id,omitempty"`
	Severity      Severity  `json:"severity"`
	// Code es estable y snake_case (^[a-z][a-z0-9_]{0,47}$).
	Code         string   `json:"code"`
	HTTPStatus   int      `json:"http_status,omitempty"`
	ProviderCode []string `json:"provider_code,omitempty"`
	// Causes: solo codigos numericos (hasta 6 digitos).
	Causes []string `json:"causes,omitempty"`
	// Props: rutas de campo (payer.email, items[0].price), nunca valores.
	Props []string `json:"props,omitempty"`
	// Shape: esqueleto "clave:tipo,clave:tipo" del cuerpo, sin valores.
	Shape         string   `json:"shape,omitempty"`
	Retryable     bool     `json:"retryable,omitempty"`
	Indeterminate bool     `json:"indeterminate,omitempty"`
	Subject       Subject  `json:"subject,omitempty"`
	UpstreamHost  string   `json:"upstream_host,omitempty"`
	Breadcrumbs   []string `json:"breadcrumbs,omitempty"`
	// DiagRef es la referencia opaca para guardar junto a la entidad de negocio.
	DiagRef string    `json:"diag_ref,omitempty"`
	At      time.Time `json:"at"`
}
