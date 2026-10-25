package diag

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/google/uuid"
)

const maxHTTPFailures = 5

// HTTPFailure es lo que el host retiene de una llamada saliente fallida: status,
// dominio, metodo y los hechos saneables del cuerpo (nunca el cuerpo).
type HTTPFailure struct {
	Status    int // 0 en error de transporte
	Host      string
	Method    string
	Transport string // "", "timeout", "transport"
	Facts     BodyFacts
}

// Bag es la bolsa de captura de UNA invocacion: fallos HTTP, ultimas lineas de
// log (breadcrumbs, que solo se persisten si la invocacion falla), secretos que
// hay que borrar por valor y la bandera "se envio una peticion que muta".
// Segura para uso concurrente.
type Bag struct {
	mu          sync.Mutex
	Scrubber    *Scrubber
	fails       []HTTPFailure
	crumbs      []string
	mutatingOut bool
	subject     Subject
}

// NewBag crea la bolsa de una invocacion.
func NewBag() *Bag { return &Bag{Scrubber: NewScrubber()} }

// maxCrumbRaw es el tope (bytes) de una linea cruda que llega a las regex de
// ScrubText (4 veces MaxBreadcrumbLen: ScrubText es superlineal y corre en la
// ruta de fallo, sincrona y fuera del presupuesto del guest). El borrado por
// valor (Redact, barato y lineal) se hace sobre la linea COMPLETA antes de
// cortar; asi un secreto conocido partido por el corte nunca se filtra. El
// recorte final a MaxBreadcrumbLen runas ocurre despues de sanear.
const maxCrumbRaw = 4 * MaxBreadcrumbLen

// clipCrumb acota una linea desmesurada sin dejar a medias un secreto: borra
// por valor ANTES de cortar y descarta el token que quede partido en el corte.
func clipCrumb(line string, sc *Scrubber) string {
	line = sc.Redact(line)
	if len(line) <= maxCrumbRaw {
		return line
	}
	cut := maxCrumbRaw
	for cut > 0 && !utf8.RuneStart(line[cut]) {
		cut--
	}
	line = line[:cut]
	// Descarta el tramo final sin separador: puede ser la mitad de un secreto
	// que no conocemos (o que se anadio despues).
	const tail = 128
	lo := len(line) - tail
	if lo < 0 {
		lo = 0
	}
	if i := strings.LastIndexAny(line[lo:], " \t\n,;&?=/\"'"); i >= 0 {
		return line[:lo+i]
	}
	return line[:lo]
}

// Log guarda una linea de log del guest (anillo de MaxBreadcrumbs). Una linea
// mayor que maxCrumbRaw se borra por valor completa y se corta; el resto se
// guarda cruda y se sanea (regex) solo si la invocacion falla.
func (b *Bag) Log(line string) {
	if b == nil {
		return
	}
	if len(line) > maxCrumbRaw {
		line = clipCrumb(line, b.Scrubber)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.crumbs) >= MaxBreadcrumbs {
		copy(b.crumbs, b.crumbs[1:])
		b.crumbs = b.crumbs[:MaxBreadcrumbs-1]
	}
	b.crumbs = append(b.crumbs, line)
}

// HTTPStarted anota una peticion saliente; las que mutan dejan el resultado
// indeterminado si la invocacion cae despues.
func (b *Bag) HTTPStarted(method string) {
	if b == nil {
		return
	}
	switch strings.ToUpper(method) {
	case "", http.MethodGet, http.MethodHead, http.MethodOptions:
		return
	}
	b.mu.Lock()
	b.mutatingOut = true
	b.mu.Unlock()
}

// HTTPResult anota una respuesta saliente. Solo guarda si status>=400.
func (b *Bag) HTTPResult(rawURL, method string, status int, body []byte) {
	if b == nil || status < 400 {
		return
	}
	f := HTTPFailure{Status: status, Host: HostOf(rawURL), Method: cleanMethod(method), Facts: ExtractBodyScrubbed(body, status, b.Scrubber)}
	b.addFail(f)
}

// HTTPTransportError anota un fallo de transporte (DNS, conexion, timeout).
func (b *Bag) HTTPTransportError(rawURL, method string, timeout bool) {
	if b == nil {
		return
	}
	kind := "transport"
	if timeout {
		kind = "timeout"
	}
	b.addFail(HTTPFailure{Host: HostOf(rawURL), Method: cleanMethod(method), Transport: kind})
}

// AddSecrets registra valores que hay que borrar por valor (credenciales de
// conector leidas por el guest).
func (b *Bag) AddSecrets(vals ...string) {
	if b != nil {
		b.Scrubber.Add(vals...)
	}
}

// AddHeaders registra los valores de las cabeceras de una peticion saliente.
func (b *Bag) AddHeaders(h map[string]string) {
	if b != nil {
		b.Scrubber.AddHeaders(h)
	}
}

// HTTPBlocked anota una salida que el host nego (capacidad http:fetch o guarda
// SSRF) antes de enviarla.
func (b *Bag) HTTPBlocked(rawURL, method string) {
	if b == nil {
		return
	}
	b.addFail(HTTPFailure{Host: HostOf(rawURL), Method: cleanMethod(method), Transport: "blocked"})
}

func (b *Bag) addFail(f HTTPFailure) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.fails) >= maxHTTPFailures {
		b.fails = append(b.fails[:0], b.fails[1:]...)
	}
	b.fails = append(b.fails, f)
}

func cleanMethod(m string) string {
	m = strings.ToUpper(strings.TrimSpace(m))
	switch m {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS":
		return m
	}
	return ""
}

// SetSubject fija la entidad de negocio afectada.
func (b *Bag) SetSubject(typ, id string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.subject = Subject{Type: typ, ID: id}
	b.mu.Unlock()
}

// Apply completa d con lo capturado: ultimo fallo HTTP, breadcrumbs, sujeto e
// indeterminacion. Llamar solo cuando la invocacion termino en fallo.
func (b *Bag) Apply(d *Diagnostic) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	d.Breadcrumbs = append([]string(nil), b.crumbs...)
	if d.Subject.Type == "" {
		d.Subject = b.subject
	}
	if len(b.fails) > 0 {
		f := b.fails[len(b.fails)-1]
		d.HTTPStatus = f.Status
		d.UpstreamHost = f.Host
		d.ProviderCode = append(d.ProviderCode, f.Facts.ProviderCodes...)
		d.Causes = append(d.Causes, f.Facts.Causes...)
		d.Props = append(d.Props, f.Facts.Props...)
		if d.Shape == "" {
			d.Shape = f.Facts.Shape
		}
		switch {
		case f.Transport == "timeout":
			d.ProviderCode = append(d.ProviderCode, "upstream_timeout")
			d.Retryable = true
		case f.Transport == "blocked":
			d.ProviderCode = append(d.ProviderCode, "egress_denied")
		case f.Transport != "":
			d.ProviderCode = append(d.ProviderCode, "upstream_unreachable")
			d.Retryable = true
		case f.Status == 408 || f.Status == 429 || f.Status >= 500:
			d.Retryable = true
		}
		if d.Kind == KindEnvelope {
			d.Kind = KindHTTP
		}
	}
	if b.mutatingOut {
		d.Indeterminate = d.Indeterminate || d.Kind == KindRuntime || (len(b.fails) > 0 && b.fails[len(b.fails)-1].Transport != "" && b.fails[len(b.fails)-1].Transport != "blocked")
	}
}

// HasFailures reporta si hubo llamadas salientes fallidas.
func (b *Bag) HasFailures() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.fails) > 0
}

// --- contexto ---

type ctxKey int

const (
	keyCorrelation ctxKey = iota
	keyInvocation
	keySubject
)

// WithCorrelation propaga un id de correlacion (el mismo de la peticion o
// accion que origino la invocacion) hasta el diagnostico.
func WithCorrelation(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, keyCorrelation, id)
}

// CorrelationFrom lee el id de correlacion del contexto.
func CorrelationFrom(ctx context.Context) string {
	s, _ := ctx.Value(keyCorrelation).(string)
	return s
}

// WithInvocationID fija el id de invocacion; con el, quien llama puede calcular
// de antemano RefFor(org, id). Si no se fija, el host genera uno.
func WithInvocationID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, keyInvocation, id)
}

// InvocationIDFrom devuelve el id de invocacion del contexto, o uno nuevo.
func InvocationIDFrom(ctx context.Context) string {
	if s, _ := ctx.Value(keyInvocation).(string); s != "" {
		return s
	}
	return uuid.NewString()
}

// WithSubject declara la entidad de negocio sobre la que opera la invocacion
// (pedido, cobro...), para correlacionar sin duplicar datos.
func WithSubject(ctx context.Context, typ, id string) context.Context {
	if typ == "" || id == "" {
		return ctx
	}
	return context.WithValue(ctx, keySubject, Subject{Type: typ, ID: id})
}

// SubjectFrom lee la entidad declarada con WithSubject.
func SubjectFrom(ctx context.Context) Subject {
	s, _ := ctx.Value(keySubject).(Subject)
	return s
}
