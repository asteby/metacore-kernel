package guest

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Clasificación de errores del host (DISENO-DEGRADACION §B, DG1.4).
//
// Este archivo es PURO (sin imports de host): un addon con wrapper propio de
// data_query/data_mutate puede llamar ClassifyHostError sobre el sobre crudo y
// dejar de buscar "42P01" por texto en cada paquete.
//
// Reconoce tres formas, para que un mismo binario valga en un host viejo y en
// uno nuevo:
//
//   - Host nuevo (ABI >= 1.18, DG1.1): error.code == "source_unavailable" con
//     error.details{source, reason, owners, installed_owners, sqlstate,
//     degradable}.
//   - Host viejo (sin details.classified): error.code == "db_error" cuyo
//     message TERMINA en el sufijo que imprime pgx: "... (SQLSTATE 42P01)".
//     Solo se acepta ese sufijo final (un dato de usuario dentro del mensaje,
//     p. ej. `"SQLSTATE 42P01"`, no cuenta; un mensaje truncado sin sufijo no
//     se clasifica) y nunca el texto libre "does not exist". Si el sobre trae
//     details.classified == true (host >= 1.19) el host ya decidió: un db_error
//     se queda como error (p. ej. un 42P01 posterior al probe es un fallo real)
//     y la forma vieja NO se aplica.
//   - Éxito con meta.absent (host con on_absent:"empty", DG1.2): la fuente se
//     sirvió vacía y el host dice cuál faltó.
//
// 42P01 (tabla ausente) y 42703 (columna ausente) NO son lo mismo: la primera
// es table_missing, la segunda schema_mismatch (la tabla existe, el dueño corre
// una versión sin esa columna).

// CodeSourceUnavailable es el código de error del host para una fuente de datos
// estructuralmente ausente (runtime/wasm.CodeSourceUnavailable).
const CodeSourceUnavailable = "source_unavailable"

// Razones de ausencia (Absence.Reason). Mismos literales que el host.
const (
	ReasonAddonNotInstalled = "addon_not_installed"
	ReasonAddonDisabled     = "addon_disabled"
	ReasonTableMissing      = "table_missing"
	ReasonSchemaMismatch    = "schema_mismatch"
)

// SQLSTATEs que el clasificador reconoce. Todo lo demás no es una ausencia.
const (
	sqlStateUndefinedTable  = "42P01"
	sqlStateUndefinedColumn = "42703"
)

// Absence describe una fuente de datos que no está disponible.
type Absence struct {
	// Source es la tabla lógica pedida. Vacío si el sobre viejo no la trae
	// (QueryOptional la completa con la tabla que pidió).
	Source string
	// Reason es una de las constantes Reason*.
	Reason string
	// Owners son los addons que declaran la tabla (solo host nuevo).
	Owners []string
	// InstalledOwners son los dueños instalados y activos en la org.
	InstalledOwners []string
	// SQLState es 42P01 o 42703 cuando el origen fue Postgres.
	SQLState string
	// Degradable es lo que dijo el host. Un host viejo no lo dice: se asume
	// true SOLO si la operación es una lectura (ClassifyReadError / data_query;
	// no puede saberse si la lectura corrió sobre una transacción abierta, ahí
	// el siguiente statement fallará con 25P02 y llegará como error normal).
	// La forma vieja de una escritura (ClassifyHostError) es siempre false.
	Degradable bool
	// Message es el texto original del driver, intacto ("" en meta.absent).
	Message string
	// Legacy es true cuando se dedujo de la forma vieja (db_error + SQLSTATE).
	Legacy bool
}

// Error implementa error para poder envolver una ausencia cuando el autor la
// trata como fallo.
func (a *Absence) Error() string {
	if a == nil {
		return "guest: <nil absence>"
	}
	if a.Source == "" {
		return fmt.Sprintf("source unavailable: %s", a.Reason)
	}
	return fmt.Sprintf("source unavailable: %s (%s)", a.Source, a.Reason)
}

// HostError es un fallo del host que NO es una ausencia (forbidden,
// invalid_request, db_error transitorio, ...). Esa es la frontera que no se
// degrada: el autor decide si falla cerrado.
type HostError struct {
	// Code es error.code tal cual (nunca vacío: "unknown" si faltaba).
	Code string
	// Message es error.message.
	Message string
}

// Error implementa error.
func (e *HostError) Error() string {
	if e == nil {
		return "guest: <nil host error>"
	}
	if e.Message == "" {
		return "host: " + e.Code
	}
	return fmt.Sprintf("host: %s: %s", e.Code, e.Message)
}

// HostOutcome es el resultado de clasificar un sobre. A lo sumo uno de los dos
// campos es distinto de nil:
//
//   - Absent != nil: la fuente no está disponible. Viene de un error
//     (source_unavailable / forma vieja) o de un éxito con meta.absent; en este
//     último caso Err es nil y el sobre trae filas vacías.
//   - Err != nil: fallo que no es ausencia (*HostError, o error de decodificación).
//   - ambos nil: éxito normal.
type HostOutcome struct {
	Absent *Absence
	Err    error
}

// ClassifyHostError clasifica el sobre `{success, data, error, meta}` de
// data_query / data_mutate / data_batch. Es total: nunca entra en pánico y un
// sobre ilegible vuelve como Err.
//
// Como no sabe si el sobre es de una lectura o de una escritura, la forma vieja
// (db_error + SQLSTATE) sale con Degradable=false: una escritura nunca se
// degrada. Para el sobre de un data_query usa ClassifyReadError.
func ClassifyHostError(envelope []byte) HostOutcome {
	return classifyEnvelope(envelope, false)
}

// ClassifyReadError es ClassifyHostError para el sobre de una LECTURA
// (data_query): la forma vieja sale con Degradable=true. Es lo que usan Query y
// QueryOptional.
func ClassifyReadError(envelope []byte) HostOutcome {
	return classifyEnvelope(envelope, true)
}

func classifyEnvelope(envelope []byte, read bool) HostOutcome {
	if len(envelope) == 0 {
		return HostOutcome{Err: &HostError{Code: "db_error", Message: "host returned empty envelope"}}
	}
	var env hostEnvelope
	if err := json.Unmarshal(envelope, &env); err != nil {
		return HostOutcome{Err: fmt.Errorf("guest: decode host envelope: %w", err)}
	}
	if env.Success {
		if env.Meta != nil && env.Meta.Absent != nil {
			a := env.Meta.Absent.toAbsence()
			if a.Reason == "" {
				a.Reason = ReasonTableMissing
			}
			a.Degradable = true // el host solo lo sirve vacío si era degradable
			return HostOutcome{Absent: a}
		}
		return HostOutcome{}
	}
	if env.Error == nil {
		return HostOutcome{Err: &HostError{Code: "unknown"}}
	}
	code := env.Error.Code
	if code == "" {
		code = "unknown"
	}
	if code == CodeSourceUnavailable {
		a := env.Error.Details.toAbsenceDetails()
		a.Message = env.Error.Message
		if a.Reason == "" {
			a.Reason = reasonFromSQLState(a.SQLState)
		}
		if a.Reason == "" {
			a.Reason = ReasonTableMissing
		}
		return HostOutcome{Absent: a}
	}
	classified := env.Error.Details != nil && env.Error.Details.Classified != nil && *env.Error.Details.Classified
	if code == "db_error" && !classified {
		if st := sqlStateInMessage(env.Error.Message); st != "" {
			if reason := reasonFromSQLState(st); reason != "" {
				return HostOutcome{Absent: &Absence{
					Reason:     reason,
					SQLState:   st,
					Degradable: read,
					Message:    env.Error.Message,
					Legacy:     true,
				}}
			}
		}
	}
	return HostOutcome{Err: &HostError{Code: code, Message: env.Error.Message}}
}

func reasonFromSQLState(state string) string {
	switch state {
	case sqlStateUndefinedTable:
		return ReasonTableMissing
	case sqlStateUndefinedColumn:
		return ReasonSchemaMismatch
	}
	return ""
}

// sqlStateInMessage extrae el SQLSTATE de un texto de pgx SOLO si es el sufijo
// final "(SQLSTATE XXXXX)" (se ignora el espacio en blanco al final). Un
// "SQLSTATE" en medio del mensaje es texto (posiblemente un dato de usuario) y
// no se mira; un mensaje truncado sin sufijo devuelve "".
func sqlStateInMessage(msg string) string {
	const open = "(SQLSTATE "
	s := strings.TrimRight(msg, " \t\r\n")
	n := len(open) + 5 + 1
	if len(s) < n || s[len(s)-1] != ')' || s[len(s)-n:len(s)-6] != open {
		return ""
	}
	st := s[len(s)-6 : len(s)-1]
	if !isSQLState(st) {
		return ""
	}
	return st
}

func isSQLState(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isAlnum(s[i]) {
			return false
		}
	}
	return true
}

func isAlnum(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z'
}

// ---- wire ----

type hostEnvelope struct {
	Success bool           `json:"success"`
	Error   *hostWireError `json:"error,omitempty"`
	Meta    *hostWireMeta  `json:"meta,omitempty"`
}

type hostWireError struct {
	Code       string           `json:"code"`
	LegacyCode string           `json:"legacy_code"`
	Message    string           `json:"message"`
	Details    *absenceWireBody `json:"details,omitempty"`
}

type hostWireMeta struct {
	Absent *absenceWireBody `json:"absent,omitempty"`
}

// absenceWireBody sirve para error.details (host nuevo) y meta.absent.
type absenceWireBody struct {
	Source          string   `json:"source"`
	Reason          string   `json:"reason"`
	Owners          []string `json:"owners"`
	InstalledOwners []string `json:"installed_owners"`
	SQLState        string   `json:"sqlstate"`
	Degradable      *bool    `json:"degradable"`
	// Classified (host >= 1.19): el host ya clasificó este sobre; un db_error
	// con la marca es un fallo real y no se reinterpreta por SQLSTATE.
	Classified *bool `json:"classified"`
}

func (b *absenceWireBody) toAbsence() *Absence {
	return &Absence{
		Source:          b.Source,
		Reason:          b.Reason,
		Owners:          b.Owners,
		InstalledOwners: b.InstalledOwners,
		SQLState:        b.SQLState,
	}
}

// toAbsenceDetails es toAbsence para error.details: Degradable viene del host
// (ausente => false, el valor seguro: no degradar lo que el host no marcó).
func (b *absenceWireBody) toAbsenceDetails() *Absence {
	if b == nil {
		return &Absence{}
	}
	a := b.toAbsence()
	a.Degradable = b.Degradable != nil && *b.Degradable
	return a
}
