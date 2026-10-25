package diag

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
)

var (
	reAddonKey = regexp.MustCompile(`^[a-z0-9][a-z0-9._\-]{0,63}$`)
	reVersion  = regexp.MustCompile(`^[0-9A-Za-z.+\-]{1,32}$`)
	reOp       = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.:\-]{0,63}$`)
	reID       = regexp.MustCompile(`^[A-Za-z0-9_\-]{1,64}$`)
	reSubjType = regexp.MustCompile(`^[a-z][a-z0-9_.\-]{0,39}$`)
	reHost     = regexp.MustCompile(`^([a-z0-9]([a-z0-9\-]{0,61}[a-z0-9])?\.)+[a-z]{2,24}$`)
	reShape    = regexp.MustCompile(`^[a-z][a-z0-9_]{0,29}:(string|number|bool|null|object|array)$`)
	reRef      = regexp.MustCompile(`^dg_[0-9a-f]{12}$`)
)

// Limites de volumen.
const (
	MaxBreadcrumbs = 20
	maxHostLen     = 253
)

// Sanitize es la UNICA funcion que decide que se guarda de un fallo. Aplica una
// allowlist por campo (lo que no encaja se descarta, nunca se "arregla" copiando
// texto), borra por valor los secretos del Scrubber y no extrae valores. Es
// idempotente y pura: no muta d. Todo lo que llega al Store pasa por aqui.
func Sanitize(d Diagnostic, sc *Scrubber) Diagnostic {
	o := Diagnostic{Org: d.Org, At: d.At.UTC()}
	if o.At.IsZero() {
		o.At = time.Now().UTC()
	}
	keep := func(s string) string { // campo corto aceptado: sin secretos ni digitos largos
		if sc.Contains(s) {
			return ""
		}
		return s
	}
	if reAddonKey.MatchString(d.AddonKey) {
		o.AddonKey = keep(d.AddonKey)
	}
	if reVersion.MatchString(d.AddonVersion) {
		o.AddonVersion = keep(d.AddonVersion)
	}
	o.Kind = d.Kind
	if !validKinds[o.Kind] {
		o.Kind = KindRuntime
	}
	if reOp.MatchString(d.Operation) && !longDigitRun(d.Operation, 5) {
		o.Operation = keep(d.Operation)
	}
	if reID.MatchString(d.CorrelationID) && !luhnValid(digitsOnly(d.CorrelationID)) {
		o.CorrelationID = keep(d.CorrelationID)
	}
	if reID.MatchString(d.InvocationID) && !luhnValid(digitsOnly(d.InvocationID)) {
		o.InvocationID = keep(d.InvocationID)
	}
	switch d.Severity {
	case SeverityInfo, SeverityWarning, SeverityError, SeverityCritical:
		o.Severity = d.Severity
	default:
		o.Severity = SeverityError
	}
	o.Code = cleanCode(d.Code, sc)
	if o.Code == "" {
		o.Code = "unknown_error"
	}
	if d.HTTPStatus >= 100 && d.HTTPStatus <= 599 {
		o.HTTPStatus = d.HTTPStatus
	}
	o.ProviderCode = cleanList(d.ProviderCode, maxProviderCds, func(s string) string { return cleanCode(s, sc) })
	o.Causes = cleanList(d.Causes, maxCauses, func(s string) string {
		s = strings.TrimSpace(s)
		if reCause.MatchString(s) && !sc.Contains(s) {
			return s
		}
		return ""
	})
	o.Props = cleanList(d.Props, maxProps, func(s string) string {
		if validPath(s, false) && !sc.Contains(s) {
			return s
		}
		return ""
	})
	sort.Strings(o.Props)
	o.Shape = cleanShape(d.Shape, sc)
	o.Retryable, o.Indeterminate = d.Retryable, d.Indeterminate
	if reSubjType.MatchString(d.Subject.Type) && reID.MatchString(d.Subject.ID) &&
		!luhnValid(digitsOnly(d.Subject.ID)) && !sc.Contains(d.Subject.ID) && !sc.Contains(d.Subject.Type) {
		o.Subject = d.Subject
	}
	o.UpstreamHost = cleanHost(d.UpstreamHost, sc)
	for _, b := range d.Breadcrumbs {
		if len(b) > maxCrumbRaw { // Diagnostic armado a mano: acota sin partir secretos
			b = clipCrumb(b, sc)
		}
		b = strings.TrimSpace(ScrubText(strings.Map(printable, b), sc))
		b = strings.TrimSpace(truncateRunes(b, MaxBreadcrumbLen))
		if b == "" {
			continue
		}
		o.Breadcrumbs = append(o.Breadcrumbs, b)
	}
	if n := len(o.Breadcrumbs); n > MaxBreadcrumbs {
		o.Breadcrumbs = o.Breadcrumbs[n-MaxBreadcrumbs:]
	}
	if reRef.MatchString(d.DiagRef) {
		o.DiagRef = d.DiagRef
	}
	return o
}

func printable(r rune) rune {
	if r == '\t' {
		return ' '
	}
	if r < 0x20 || r == 0x7f || unicode.Is(unicode.Cf, r) {
		return -1
	}
	return r
}

// cleanCode: codigo snake_case estable. Se admite mayuscula de origen
// (INVALID_REQUEST) por pasar a minuscula, que es mecanico.
func cleanCode(s string, sc *Scrubber) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if !reCode.MatchString(s) || longDigitRun(s, 5) || sc.Contains(s) {
		return ""
	}
	return s
}

func cleanList(in []string, max int, f func(string) string) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range in {
		v := f(s)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
		if len(out) >= max {
			break
		}
	}
	return out
}

func cleanShape(s string, sc *Scrubber) string {
	if s == "" {
		return ""
	}
	pairs := strings.Split(s, ",")
	seen := map[string]bool{}
	var out []string
	for _, p := range pairs {
		if !reShape.MatchString(p) || longDigitRun(p, 5) || sc.Contains(p[:strings.IndexByte(p, ':')]) {
			continue
		}
		k := p[:strings.IndexByte(p, ':')]
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, p)
	}
	sort.Strings(out)
	if len(out) > maxShapeKeys {
		out = out[:maxShapeKeys]
	}
	return strings.Join(out, ",")
}

// HostOf devuelve solo el dominio de una URL o host (sin esquema, userinfo,
// puerto, path ni query); "" si no es un nombre de dominio valido.
func HostOf(raw string) string { return cleanHost(raw, nil) }

func cleanHost(raw string, sc *Scrubber) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 2048 {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	h := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if len(h) > maxHostLen || !reHost.MatchString(h) || longDigitRun(h, 5) || sc.Contains(h) {
		return ""
	}
	return h
}

// VersionMajor devuelve el componente mayor de una version ("1.4.2" -> "1",
// "v2" -> "2"); "0" si no hay.
func VersionMajor(v string) string {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	end := 0
	for end < len(v) && v[end] >= '0' && v[end] <= '9' {
		end++
	}
	if end == 0 {
		return "0"
	}
	return v[:end]
}

// Fingerprint agrupa fallos iguales: hash(addon, version mayor, operacion, kind,
// code, http_status, provider_code, props ordenadas). d debe estar saneado.
func Fingerprint(d Diagnostic) string {
	pc := append([]string(nil), d.ProviderCode...)
	sort.Strings(pc)
	pr := append([]string(nil), d.Props...)
	sort.Strings(pr)
	h := sha256.New()
	for _, part := range []string{
		d.AddonKey, VersionMajor(d.AddonVersion), d.Operation, string(d.Kind), d.Code,
		strconv.Itoa(d.HTTPStatus), strings.Join(pc, ","), strings.Join(pr, ","),
	} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// GroupID es el id determinista del grupo (org + huella): permite el upsert
// atomico sin leer antes.
func GroupID(org uuid.UUID, fingerprint string) uuid.UUID {
	return uuid.NewSHA1(org, []byte("addon_diagnostic_group:"+fingerprint))
}

// RefFor es la referencia opaca (diag_ref) que un host guarda junto a la
// entidad de negocio para volver al diagnostico de una invocacion.
func RefFor(org uuid.UUID, invocationID string) string {
	sum := sha256.Sum256([]byte(org.String() + "|" + invocationID))
	return "dg_" + hex.EncodeToString(sum[:])[:12]
}
