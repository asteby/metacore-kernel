package webhooks

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"

	"github.com/asteby/metacore-kernel/runtime/webhookout"
	"github.com/asteby/metacore-kernel/webhooks/signing"
)

// Ordering controla si los eventos de un mismo agregado salen en secuencia.
type Ordering string

const (
	OrderingNone         Ordering = "none"
	OrderingPerAggregate Ordering = "per_aggregate"
)

// DefaultIdempotencyHeader es la cabecera por omision de la llave (V2:
// X-Idempotency-Key).
const DefaultIdempotencyHeader = "X-Idempotency-Key"

// Declarative convierte un Webhook en una suscripcion declarativa de emision
// (RFC 0003 §2.1). Es DATO puro: sin codigo del cliente y sin secretos (la
// firma solo lleva referencias). Un Webhook con Declarative == nil se comporta
// exactamente como antes.
type Declarative struct {
	// Key es un nombre estable legible ("v2-clientes"); opcional.
	Key string `json:"key,omitempty"`
	// Method: POST (omision), PUT o PATCH.
	Method string `json:"method,omitempty"`
	// Filter acota que eventos (de los que casan con Events) se entregan.
	Filter *Filter `json:"filter,omitempty"`
	// Transform arma el cuerpo. Sin Transform el cuerpo es
	// {"event": {...sobre...}, "payload": {...}}.
	Transform *Transform `json:"transform,omitempty"`
	// Signing es el perfil de firma. Sin Signing se usa la firma historica
	// X-Metacore-* con Webhook.Secret.
	Signing *signing.Spec `json:"signing,omitempty"`
	// Headers son cabeceras constantes NO secretas (p. ej. X-7L-Contract).
	Headers map[string]string `json:"headers,omitempty"`
	// Idempotency es la plantilla de la llave que viaja al destino y es la
	// misma en cada reintento. Omision: "{{event.id}}".
	Idempotency string `json:"idempotency,omitempty"`
	// IdempotencyHeader: cabecera de la llave (omision X-Idempotency-Key).
	IdempotencyHeader string `json:"idempotency_header,omitempty"`
	// Ordering: none (omision) | per_aggregate.
	Ordering Ordering `json:"ordering,omitempty"`
	// AutoDisableThreshold: fallos consecutivos tras los que la suscripcion se
	// apaga sola (Enabled=false: los eventos se retienen, no se pierden) y avisa
	// por Config.OnAutoDisable. 0 = 10; negativo = nunca. Se reanuda a mano con
	// SetEnabled(true).
	AutoDisableThreshold int `json:"auto_disable_threshold,omitempty"`
	// Enabled: apagado por omision. Apagado, los eventos se RETIENEN y salen
	// al encender (SetEnabled / Update).
	Enabled bool `json:"enabled"`
}

func (d *Declarative) method() string {
	if d.Method == "" {
		return "POST"
	}
	return d.Method
}

func (d *Declarative) idemTemplate() string {
	if d.Idempotency == "" {
		return "{{event.id}}"
	}
	return d.Idempotency
}

func (d *Declarative) idemHeader() string {
	if d.IdempotencyHeader == "" {
		return DefaultIdempotencyHeader
	}
	return d.IdempotencyHeader
}

// SecretResolver resuelve una referencia (`secret_ref` / `key_ref`) al valor.
// Lo implementa el host (conector, secreto cifrado). El valor no se guarda ni
// se registra.
type SecretResolver interface {
	ResolveSecret(ctx context.Context, ref string) (string, error)
}

// SecretResolverFunc adapta una funcion.
type SecretResolverFunc func(ctx context.Context, ref string) (string, error)

func (f SecretResolverFunc) ResolveSecret(ctx context.Context, ref string) (string, error) {
	return f(ctx, ref)
}

var (
	keyRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
	eventRe  = regexp.MustCompile(`^(\*|[A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)*(\.\*)?)$`)
	hdrRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,63}$`)
	labelRe  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	badHdrRe = regexp.MustCompile(`(?i)(auth|token|secret|key|passw|cookie|signature|credential)`)
)

// compiled es la forma ejecutable (validada) de un Declarative.
type compiled struct {
	filter    *compiledFilter
	transform *compiledTransform
	idem      []tmplPart
}

func (d *Declarative) compile() (*compiled, error) {
	c := &compiled{}
	var err error
	if c.filter, err = compileFilter(d.Filter); err != nil {
		return nil, err
	}
	if c.transform, err = compileTransform(d.Transform); err != nil {
		return nil, err
	}
	if c.idem, err = parseTemplate(d.idemTemplate()); err != nil {
		return nil, err
	}
	for _, p := range c.idem {
		if p.expr != nil && len(p.expr.fns) > 0 {
			for _, f := range p.expr.fns {
				if f.name == "map" {
					return nil, fmt.Errorf("%w: idempotency cannot use map tables", ErrInvalidDeclarative)
				}
			}
		}
	}
	return c, nil
}

func (c *compiled) refs(d *Declarative) []pathRef {
	var r []pathRef
	r = append(r, c.filter.refs()...)
	if c.transform != nil {
		r = append(r, c.transform.refs...)
	}
	for _, p := range c.idem {
		if p.expr != nil {
			r = append(r, p.expr.path)
		}
	}
	return r
}

// validateDeclarative corre AL GUARDAR (Create/Update). Todo lo que pueda
// fallar al entregar y sea conocible aqui, falla aqui.
func (s *Service) validateDeclarative(w *Webhook) error {
	d := w.Declarative
	if d == nil {
		return nil
	}
	if d.Key != "" && !keyRe.MatchString(d.Key) {
		return fmt.Errorf("%w: bad key %q", ErrInvalidDeclarative, d.Key)
	}
	switch d.method() {
	case "POST", "PUT", "PATCH":
	default:
		return fmt.Errorf("%w: method must be POST, PUT or PATCH", ErrInvalidDeclarative)
	}
	switch d.Ordering {
	case "", OrderingNone, OrderingPerAggregate:
	default:
		return fmt.Errorf("%w: ordering must be none or per_aggregate", ErrInvalidDeclarative)
	}
	for _, e := range w.Events {
		if !eventRe.MatchString(e) {
			return fmt.Errorf("%w: bad event pattern %q", ErrInvalidDeclarative, e)
		}
	}
	if w.RetryMax > 50 || w.TimeoutSec > 120 {
		return fmt.Errorf("%w: retry_max <= 50 and timeout_sec <= 120", ErrInvalidDeclarative)
	}
	if err := s.validateTarget(w.URL); err != nil {
		return err
	}
	c, err := d.compile()
	if err != nil {
		return err
	}
	if err := checkRefs(s.schemas, w.Events, c.refs(d)); err != nil {
		return err
	}
	if d.Signing != nil {
		if err := d.Signing.Validate(); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidDeclarative, err)
		}
		if s.secrets == nil {
			return ErrNoSecretResolver
		}
	}
	return d.validateHeaders()
}

func (d *Declarative) validateHeaders() error {
	taken := map[string]bool{strings.ToLower(d.idemHeader()): true}
	if ih := strings.ToLower(d.idemHeader()); !hdrRe.MatchString(d.idemHeader()) || ih == "authorization" || ih == "host" ||
		ih == "content-length" || ih == "content-type" || strings.HasPrefix(ih, "x-metacore-") {
		return fmt.Errorf("%w: bad idempotency_header", ErrInvalidDeclarative)
	}
	if d.Signing != nil {
		for _, h := range []string{d.Signing.Header, d.Signing.TimestampHeader, d.Signing.NonceHeader, d.Signing.SignatureHeader,
			signing.DefaultSimpleHeader, signing.DefaultTimestampHeader, signing.DefaultNonceHeader, signing.DefaultSignatureHeader} {
			if h != "" {
				taken[strings.ToLower(h)] = true
			}
		}
	}
	if len(d.Headers) > 20 {
		return fmt.Errorf("%w: too many headers", ErrInvalidDeclarative)
	}
	for k, v := range d.Headers {
		lk := strings.ToLower(k)
		if !hdrRe.MatchString(k) || badHdrRe.MatchString(k) || strings.HasPrefix(lk, "x-metacore-") || taken[lk] {
			return fmt.Errorf("%w: header %q is not allowed (secrets belong in signing, by reference)", ErrInvalidDeclarative, k)
		}
		switch lk {
		case "host", "content-length", "content-type", "transfer-encoding", "connection", "user-agent", "te", "trailer", "upgrade", "expect":
			return fmt.Errorf("%w: header %q is reserved", ErrInvalidDeclarative, k)
		}
		if strings.ContainsAny(v, "\r\n") || len(v) > 256 {
			return fmt.Errorf("%w: bad value for header %q", ErrInvalidDeclarative, k)
		}
	}
	return nil
}

// validateTarget aplica la regla de salida del kernel (netguard): https, un
// DOMINIO (no IP, ni formas numericas), sin credenciales en la URL, sin
// espacios de nombres internos y, si el host fijo AllowHosts, dentro de ellos.
// La defensa definitiva (IP resuelta, DNS rebinding, redirecciones) vive en el
// cliente de salida (netguard.NewClient) al conectar.
func (s *Service) validateTarget(raw string) error {
	if s.allowPrivate {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return fmt.Errorf("%w: unparsable URL", ErrBlockedTarget)
		}
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%w: unparsable URL", ErrBlockedTarget)
	}
	if u.User != nil {
		return fmt.Errorf("%w: credentials in URL", ErrBlockedTarget)
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" || net.ParseIP(host) != nil || strings.ContainsAny(host, ":[]%") {
		return fmt.Errorf("%w: host must be a domain name, not an IP", ErrBlockedTarget)
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return fmt.Errorf("%w: host must be a fully-qualified domain", ErrBlockedTarget)
	}
	tld := labels[len(labels)-1]
	if allDigitsOrHex(tld) {
		return fmt.Errorf("%w: numeric host", ErrBlockedTarget)
	}
	for _, l := range labels {
		if l == "" || len(l) > 63 || !labelRe.MatchString(l) {
			return fmt.Errorf("%w: invalid host name", ErrBlockedTarget)
		}
	}
	// The shared egress rule (https only, forbidden name spaces, AllowHosts,
	// ports): the same one runtime/webhookout applies, built on netguard.
	if err := s.egressPolicy().CheckURL(u); err != nil {
		return fmt.Errorf("%w: %v", ErrBlockedTarget, err)
	}
	return nil
}

// egressPolicy is the webhookout policy used to validate targets and to build
// the default delivery client. Any path is fine (the subscription names its own
// URL); everything else is the safe zero value.
func (s *Service) egressPolicy() webhookout.Policy {
	return webhookout.Policy{AllowHosts: s.allowHosts, AllowPathPrefixes: []string{"/"}}
}

func allDigitsOrHex(s string) bool {
	if strings.HasPrefix(s, "0x") {
		return true
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}
